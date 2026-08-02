package lpg

// edge_slot_reltype.go — reading and writing ONE parallel slot's relationship
// type by a durable ordinal (rmp #2262).
//
// A relationship type belongs to the relationship INSTANCE, so a pair's parallel
// slots must be able to disagree about their type ([Graph.slotCarriesType]).
// Persisting that requires naming a slot in a way that survives a checkpoint,
// which neither the adjacency index (an implementation detail of the live entry)
// nor the stable handle (absent on every slot a raw AddEdge creates) can do on
// its own.
//
// # What has to be persisted
//
// The type state of a pair is exactly two things: each slot's INLINE entry in
// the adjacency label column, and the pair's OVERFLOW list. Everything a reader
// asks about a type is derived from those two — [Graph.slotCarriesType] per
// slot, [Graph.EdgeLabels] as the pair's union, [Graph.RelationshipTypesInUse]
// across the graph. So this file exposes exactly those two halves, per pair, on
// both the read side (for the snapshot writer) and the write side (for the
// snapshot apply). Persisting the DERIVED per-slot sets instead would be lossy
// in one direction and inventive in the other, which is the defect being fixed.
//
// The by-handle type store is deliberately NOT part of this: it has its own
// durable component (edgehandles.bin) and stays authoritative for the slots it
// covers. But the label COLUMN of such a slot is still written and still read —
// [Graph.EdgeLabels] and [Graph.RelationshipTypesInUse] consult only the column
// and the overflow — so a slot's inline entry is persisted here whether or not a
// handle record also covers it. Skipping it would empty the column of every
// Cypher-created relationship across a restart.
//
// # The CANONICAL ordinal
//
// The ordinal of a slot is its position among the pair's dst-matching slots after
// a STABLE sort by handle ascending. That specific order is chosen because it is
// the one BOTH recovery paths converge on:
//
//   - the self-sufficient (v3 snapshot) path replays csr.bin, whose runs
//     [csr.OrderRuns] has already stably ordered by the total key (destination,
//     handle); restricted to one destination that is exactly a stable sort by
//     handle, so the replayed adjacency IS in canonical order;
//   - the WAL-authoritative (v2) path replays OpAddEdge / OpAddEdgeH frames in
//     commit order, so the replayed adjacency is in the ORIGINAL insertion order.
//
// Canonicalising is idempotent and a stable sort preserves the relative order of
// the handle-0 residual, so canonical(csr order) and canonical(insertion order)
// are the same sequence. Computing the ordinal against the canonical order on
// both the write and the apply side therefore addresses the same slot whichever
// path recovery took, without the file having to carry the handle.
//
// The sort is almost never executed: a pair whose handles are already
// non-decreasing in adjacency order is already canonical, which covers every
// all-handle-less pair (a Go-API build) and every all-Cypher pair (handles are
// minted from a monotonic counter). The check is one O(degree) pass and the fast
// path allocates nothing.

import (
	"cmp"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// slotHandleAt returns the stable handle of adjacency slot i, or 0 when the
// entry carries no handle column (a graph that never used AddEdgeH).
func slotHandleAt(handles []uint64, i int) uint64 {
	if i < len(handles) {
		return handles[i]
	}
	return 0
}

// canonicalPairSlots returns the adjacency indexes of the slots whose neighbour
// is dstID, in CANONICAL order (stable by handle ascending), appended into buf.
//
// The returned slice aliases buf, so the caller must treat the result as valid
// only until its next call.
func canonicalPairSlots(
	neighbours []graph.NodeID,
	handles []uint64,
	dstID graph.NodeID,
	buf []int,
) []int {
	buf = buf[:0]
	canonical := true
	var prev uint64
	for i, nb := range neighbours {
		if nb != dstID {
			continue
		}
		h := slotHandleAt(handles, i)
		if len(buf) > 0 && h < prev {
			canonical = false
		}
		prev = h
		buf = append(buf, i)
	}
	if canonical || len(buf) < 2 {
		return buf
	}
	// Rare: the pair mixes handle-carrying and handle-less slots in an order the
	// csr.bin run ordering would permute. Stable-sorting by handle reproduces
	// exactly that permutation (see the file comment).
	slices.SortStableFunc(buf, func(a, b int) int {
		return cmp.Compare(slotHandleAt(handles, a), slotHandleAt(handles, b))
	})
	return buf
}

// ForEachPairSlotRelTypeByID streams the INLINE relationship type of each slot of
// the directed pair (srcID → dstID), in canonical ordinal order, invoking visit
// once per typed slot with that slot's ordinal and type name. A slot whose label
// column entry is the 0 sentinel carries no inline type and is not visited, so a
// pair of two slots where only the second is typed yields exactly one call, with
// ordinal 1.
//
// It reports the slot's OWN entry and nothing else: not a sibling slot's entry,
// not the pair's overflow list (that is
// [Graph.ForEachPairOverflowRelTypeByID]), and not the by-handle store (that has
// its own durable component). Reporting the pair's derived union instead is what
// made a checkpoint round trip lose a type on one shape and invent one on
// another (rmp #2262).
//
// It takes NO Mapper lock and performs no external-key lookup, so the snapshot
// writer may call it after snapshotting node ids inside [graph.Mapper.Walk]
// without re-entering the Mapper (#1648).
//
// ForEachPairSlotRelTypeByID is safe for concurrent use; it observes a lock-free
// adjacency snapshot, so a slot added concurrently may or may not be included.
func (g *Graph[N, W]) ForEachPairSlotRelTypeByID(
	srcID, dstID graph.NodeID,
	visit func(ordinal int, name string),
) {
	neighbours, _, handles := g.adj.LoadEntryH(srcID)
	if len(neighbours) == 0 {
		return
	}
	labels := g.adj.LoadEntryLabels(srcID)
	if len(labels) == 0 {
		return
	}
	var scratch [8]int
	for ordinal, idx := range canonicalPairSlots(neighbours, handles, dstID, scratch[:0]) {
		if idx >= len(labels) {
			continue
		}
		lid, ok := decodeSlotLabel(labels[idx])
		if !ok {
			continue
		}
		if name, ok := g.reg.Resolve(lid); ok {
			visit(ordinal, name)
		}
	}
}

// ForEachPairOverflowRelTypeByID streams the directed pair's OVERFLOW
// relationship types, in list order, invoking visit once per resolved name.
//
// The overflow list holds a type [Graph.SetEdgeLabel] could not place per slot
// because no column-typed slot of the pair was free; naming the pair named every
// one of them, so [Graph.slotCarriesType] reads an overflow type as carried by
// every column-typed slot of the pair. It is the second half of a pair's durable
// type state, and it is per-PAIR rather than per-slot by construction.
//
// It short-circuits on [Graph.edgeLabelOverflowActive], so a graph with no
// overflow anywhere — every Cypher-built one, and every graph whose pairs never
// needed a second type — pays one atomic load and takes no lock.
//
// ForEachPairOverflowRelTypeByID is safe for concurrent use. Names are resolved
// after the shard lock is released, so visit may safely read the graph.
func (g *Graph[N, W]) ForEachPairOverflowRelTypeByID(srcID, dstID graph.NodeID, visit func(name string)) {
	if g.edgeLabelOverflowActive.Load() == 0 {
		return
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.RLock()
	var ids []LabelID
	if ls := sh.overflow[k]; len(ls) > 0 {
		ids = make([]LabelID, len(ls))
		copy(ids, ls)
	}
	sh.mu.RUnlock()
	for _, lid := range ids {
		if name, ok := g.reg.Resolve(lid); ok {
			visit(name)
		}
	}
}

// SetEdgeRelTypeAtSlotByID attaches name as a relationship type to the ONE slot
// of the directed pair (srcID → dstID) sitting at the supplied canonical ordinal
// (see the file comment). It reports whether the ordinal resolved to a slot; a
// pair holding fewer slots than the ordinal demands is a no-op returning false,
// which is how the snapshot apply path degrades when the adjacency it replays
// onto is not the one the records were written from.
//
// It is the per-SLOT counterpart of [Graph.SetEdgeLabel], which names the PAIR
// and therefore types every free column-typed slot of it. The type goes into the
// slot's own label column when that column is free. When the column already
// holds a DIFFERENT type the call does NOT overwrite it — a slot carries
// {inline} plus the pair's overflow, so the extra type spills to the overflow,
// exactly where SetEdgeLabel puts a type it cannot place per slot. Not
// overwriting is what keeps a snapshot replayed on top of a WAL tail from
// destroying the tail's more recent type. Re-asserting a type the slot already
// holds inline is a no-op that still reports true.
//
// It does NOT write the by-handle type store, which has its own durable
// component and stays authoritative for the slots it covers; this writes the
// adjacency label column, which is what [Graph.EdgeLabels],
// [Graph.HasEdgeLabel] and [Graph.RelationshipTypesInUse] read regardless of
// whether a handle record also exists.
//
// SetEdgeRelTypeAtSlotByID is safe for concurrent use.
func (g *Graph[N, W]) SetEdgeRelTypeAtSlotByID(srcID, dstID graph.NodeID, ordinal int, name string) bool {
	return g.setEdgeRelTypeAtSlotByIDInfo(srcID, dstID, ordinal, name, nil)
}

// setEdgeRelTypeAtSlotByIDInfo is [Graph.SetEdgeRelTypeAtSlotByID] with an explicit
// write transaction; tx is nil for a direct Go-API mutation, which is committed the
// instant it is made and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) setEdgeRelTypeAtSlotByIDInfo(srcID, dstID graph.NodeID, ordinal int, name string, tx *writeCtx) bool {
	if ordinal < 0 {
		return false
	}
	lid := g.reg.Intern(name)
	enc := encodeSlotLabel(lid)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.Lock()
	resolved, changed := g.setSlotRelTypeLocked(k, ordinal, lid, enc, tx)
	sh.mu.Unlock()
	if !resolved {
		return false
	}
	g.edgeIdx.Add(uint32(lid), srcID)
	if changed {
		// Same ordering rule [Graph.SetEdgeLabel] follows: bump the topology
		// generation only after the shard is released and the index add is done,
		// so a reader that samples the new epoch cannot miss the write it
		// announces (rmp #2151/#2255).
		g.topoGeneration.Add(1)
	}
	return true
}

// setSlotRelTypeLocked places lid on the slot of k at the canonical ordinal. The
// caller must hold k's edge-label shard write lock. It reports whether the
// ordinal resolved to a slot and whether the type state actually changed.
func (g *Graph[N, W]) setSlotRelTypeLocked(k edgeKey, ordinal int, lid LabelID, enc uint32, tx *writeCtx) (resolved, changed bool) {
	neighbours, _, handles := g.adj.LoadEntryH(k.src)
	var scratch [8]int
	slots := canonicalPairSlots(neighbours, handles, k.dst, scratch[:0])
	if ordinal >= len(slots) {
		return false, false
	}
	idx := slots[ordinal]
	labels := g.adj.LoadEntryLabels(k.src)
	var cur uint32
	if idx < len(labels) {
		cur = labels[idx]
	}
	switch cur {
	case 0:
		one := [1]int{idx}
		return true, g.adj.SetEdgeLabelSlotsAt(k.src, k.dst, one[:], enc) > 0
	case enc:
		return true, false
	}
	return true, g.addPairOverflowLocked(k, lid, tx)
}

// AddEdgeRelTypeOverflowByID adds name to the OVERFLOW list of the directed pair
// (srcID → dstID) — the per-pair half of its type state, read by
// [Graph.slotCarriesType] as carried by every column-typed slot of the pair. It
// reports whether the list changed; re-asserting a type already present reports
// false.
//
// It exists so the snapshot apply path can restore a pair's overflow exactly as
// it stood, instead of re-deriving it from a placement heuristic. Ordinary
// callers should use [Graph.SetEdgeLabel], which decides for itself whether a
// type fits in a slot's column or has to spill.
//
// Unlike [Graph.SetEdgeLabel] it does NOT require the edge to exist: an overflow
// entry for an absent pair is inert, and the apply path has already checked
// [adjlist.AdjList.HasEdge] before calling.
//
// AddEdgeRelTypeOverflowByID is safe for concurrent use.
func (g *Graph[N, W]) AddEdgeRelTypeOverflowByID(srcID, dstID graph.NodeID, name string) bool {
	return g.addEdgeRelTypeOverflowByIDInfo(srcID, dstID, name, nil)
}

// addEdgeRelTypeOverflowByIDInfo is [Graph.AddEdgeRelTypeOverflowByID] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) addEdgeRelTypeOverflowByIDInfo(srcID, dstID graph.NodeID, name string, tx *writeCtx) bool {
	lid := g.reg.Intern(name)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.Lock()
	changed := g.addPairOverflowLocked(k, lid, tx)
	sh.mu.Unlock()
	g.edgeIdx.Add(uint32(lid), srcID)
	if changed {
		g.topoGeneration.Add(1)
	}
	return changed
}

// addPairOverflowLocked appends lid to k's overflow list, keeping the
// [Graph.edgeLabelOverflowActive] gate exact. The caller must hold k's
// edge-label shard write lock.
func (g *Graph[N, W]) addPairOverflowLocked(k edgeKey, lid LabelID, tx *writeCtx) bool {
	sh := g.edgeLabelShardFor(k)
	if !g.addOverflowVersioned(sh, k, lid, tx) {
		return false
	}
	g.edgeLabelOverflowActive.Add(1)
	return true
}
