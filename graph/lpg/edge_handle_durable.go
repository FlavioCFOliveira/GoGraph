package lpg

// edge_handle_durable.go — Stage 2/3 durability surface for stable edge
// handles: the helpers the WAL replay and the snapshot load paths use to
// rebuild a recovered graph whose parallel edges keep their original
// per-CREATE identity (type + properties) and whose handle counter stays
// monotone across a reopen.
//
// # Why these exist
//
// edge_handle.go assigns handles in memory and keys per-instance edge
// metadata by them, but the handle was volatile: a recovered edge got a
// fresh handle and an empty per-handle store, so two distinctly-typed
// parallel CREATEs collapsed to one type on the next reopen. Persisting the
// handle (WAL: OpAddEdgeH / OpSetEdge{Label,Property}ByHandle; snapshot:
// the edgehandles.bin component) closes that, but recovery must replay it
// without double-inserting an edge the snapshot already loaded. These
// helpers give recovery the two primitives it needs:
//
//   - [Graph.HasEdgeHandle] — does this (src, dst) pair already carry an
//     edge stamped with `handle`? Lets replay of an OpAddEdgeH no-op when
//     the snapshot (or an earlier replayed frame) already materialised it,
//     making snapshot + full-WAL recovery idempotent (the examples/24
//     doubling fix). The per-pair handle column is the single source of
//     truth; no second live-handle set is kept (derive, don't duplicate).
//   - [Graph.AddEdgeHIfAbsent] — insert (src, dst, w) with the explicit
//     `handle` only when [Graph.HasEdgeHandle] is false, so the replay is
//     all-or-nothing per edge.
//   - [Graph.SeedEdgeHandle] — raise the handle high-water counter to at
//     least `next` after recovery so a post-recovery AddEdge never re-mints
//     a handle already live on disk (invariant I5).
//   - [Graph.WalkEdgeHandles] — enumerate every live (src, dst, handle)
//     triple so the snapshot writer can persist the handle column and the
//     per-handle metadata deterministically.
//
// # Concurrency
//
// All four are intended for the single-threaded recovery / snapshot phase.
// HasEdgeHandle and SeedEdgeHandle are individually safe for concurrent
// use (they take the same locks the in-memory write path takes);
// AddEdgeHIfAbsent and WalkEdgeHandles are not (they read-then-write or
// walk the whole adjacency without a global barrier).

import "github.com/FlavioCFOliveira/GoGraph/graph"

// HasEdgeHandle reports whether the directed (src, dst) pair carries a
// stored edge whose stable handle equals `handle`. It scans the pair's
// parallel handle column on the adjacency slot — the single source of
// truth for which handles are live on which pair — and returns false when
// handle is 0 (the no-handle sentinel), when either endpoint is unknown to
// the mapper, or when the pair has no slot stamped with that handle.
//
// HasEdgeHandle is the idempotency predicate WAL replay uses: an
// OpAddEdgeH whose handle is already present (loaded from the snapshot or
// applied by an earlier frame) is a no-op, so snapshot + full-WAL recovery
// does not double the edge.
//
// HasEdgeHandle is safe for concurrent use.
func (g *Graph[N, W]) HasEdgeHandle(src, dst N, handle uint64) bool {
	if handle == 0 {
		return false
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return false
	}
	neighbours, _, handles := g.adj.LoadEntryH(srcID)
	if handles == nil {
		return false
	}
	for i, nb := range neighbours {
		if nb == dstID && i < len(handles) && handles[i] == handle {
			return true
		}
	}
	return false
}

// AddEdgeHIfAbsent inserts a directed edge (src, dst, w) stamped with the
// explicit stable `handle`, but only when no edge with that handle already
// exists on the (src, dst) pair ([Graph.HasEdgeHandle]). When the handle is
// already present the call is a no-op and returns (false, nil): the edge
// was loaded by the snapshot or applied by an earlier WAL frame, so
// re-inserting it would create a spurious parallel duplicate. When the
// handle is absent the edge is inserted via the explicit-handle adjacency
// path ([adjlist.AdjList.AddEdgeH]) and the call returns (true, nil).
//
// AddEdgeHIfAbsent is the replay primitive that makes snapshot + full-WAL
// recovery idempotent without a second live-handle index. It does NOT
// advance the handle counter — the handle is supplied by the durable
// record, not freshly minted; [Graph.SeedEdgeHandle] re-seeds the counter
// once after replay.
//
// A handle of 0 is treated as "no durable identity" and falls back to a
// plain [Graph.AddEdge] so a pre-Stage-2 WAL frame (which carried no
// handle) still replays. AddEdgeHIfAbsent is NOT safe for concurrent use.
func (g *Graph[N, W]) AddEdgeHIfAbsent(src, dst N, w W, handle uint64) (inserted bool, err error) {
	if handle == 0 {
		if err := g.adj.AddEdge(src, dst, w); err != nil {
			return false, err
		}
		return true, nil
	}
	if g.HasEdgeHandle(src, dst, handle) {
		return false, nil
	}
	if err := g.adj.AddEdgeH(src, dst, w, handle); err != nil {
		return false, err
	}
	return true, nil
}

// SeedEdgeHandle raises the per-graph stable-handle high-water counter so
// the next [Graph.AddEdgeH] returns a value strictly greater than `next-1`
// — i.e. at least `next`. It is called once at the end of recovery with
// max(live handle)+1 so a post-recovery edge creation never re-mints a
// handle that is already live on disk (invariant I5: handles stay unique
// and monotone across a reopen).
//
// The operation is monotone: seeding with a value at or below the current
// counter is a no-op, so calling it with a stale `next` cannot rewind the
// counter. SeedEdgeHandle is safe for concurrent use, though recovery
// calls it from the single load goroutine.
func (g *Graph[N, W]) SeedEdgeHandle(next uint64) {
	if next == 0 {
		return
	}
	// Raise the counter to next-1 so the following Add(1) yields next.
	// nextEdgeHandle is edgeHandleSeq.Add(1); the stored value is the last
	// HANDED-OUT handle. To make the next handout >= next, the stored value
	// must be >= next-1.
	target := next - 1
	for {
		cur := g.edgeHandleSeq.Load()
		if cur >= target {
			return
		}
		if g.edgeHandleSeq.CompareAndSwap(cur, target) {
			return
		}
	}
}

// EdgeHandleTriple is one live durable edge identity: the (src, dst)
// endpoint NodeIDs and the stable handle stamped on that slot. Emitted by
// [Graph.WalkEdgeHandles] for the snapshot writer.
type EdgeHandleTriple struct {
	Src    graph.NodeID
	Dst    graph.NodeID
	Handle uint64
}

// WalkEdgeHandles calls fn once for every live directed edge slot that
// carries a non-zero stable handle. It returns early if fn returns false.
// Slots with a 0 handle (the no-handle sentinel, e.g. a simple-graph edge
// or a pre-Stage-2 edge) are skipped: there is no durable identity to
// persist for them.
//
// The walk is the snapshot writer's enumeration of the adjacency handle
// column. It iterates source nodes in the underlying mapper's [Walk] order
// — the exact order the CSR, labels and properties snapshot writers use —
// and within each source in adjacency slot order (insertion order). That
// makes the persisted edgehandles.bin component byte-stable across writes
// of the same logical state and aligned with the CSR component, honouring
// the cross-process byte-equality contract the snapshot relies on.
//
// WalkEdgeHandles is NOT safe for concurrent use with mutations on g.
func (g *Graph[N, W]) WalkEdgeHandles(fn func(EdgeHandleTriple) bool) {
	adj := g.adj
	adj.Mapper().Walk(func(srcID graph.NodeID, _ N) bool {
		neighbours, _, handles := adj.LoadEntryH(srcID)
		if handles == nil {
			return true
		}
		for i, dstID := range neighbours {
			if i >= len(handles) {
				break
			}
			h := handles[i]
			if h == 0 {
				continue
			}
			if !fn(EdgeHandleTriple{Src: srcID, Dst: dstID, Handle: h}) {
				return false
			}
		}
		return true
	})
}

// EdgeLabelsByHandleID returns the labels recorded for the edge identified
// by `handle` on the directed (srcID, dstID) NodeID pair, resolving NodeIDs
// directly rather than through the natural key. It is the NodeID-keyed
// dual of [Graph.EdgeLabelsByHandle] used by the snapshot writer, which
// walks the adjacency by NodeID and must not pay a Resolve→Lookup round
// trip per handle. Returns nil when handle is 0, the handle was never
// labelled, or no handle store exists for the pair.
//
// EdgeLabelsByHandleID is safe for concurrent use.
func (g *Graph[N, W]) EdgeLabelsByHandleID(srcID, dstID graph.NodeID, handle uint64) []string {
	if handle == 0 {
		return nil
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandleLabelShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	byHandle, ok := sh.m[k]
	if !ok {
		return nil
	}
	bag, ok := byHandle[handle]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(bag))
	for lid := range bag {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	}
	return out
}

// EdgePropertiesByHandleID returns the property map recorded for the edge
// identified by `handle` on the directed (srcID, dstID) NodeID pair. It is
// the NodeID-keyed dual of [Graph.EdgePropertiesByHandle] used by the
// snapshot writer. Returns nil when handle is 0, the handle was never
// written, or no handle store exists for the pair.
//
// EdgePropertiesByHandleID is safe for concurrent use.
func (g *Graph[N, W]) EdgePropertiesByHandleID(srcID, dstID graph.NodeID, handle uint64) map[string]PropertyValue {
	if handle == 0 {
		return nil
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandlePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	byHandle, ok := sh.m[k]
	if !ok {
		return nil
	}
	bag, ok := byHandle[handle]
	if !ok {
		return nil
	}
	out := make(map[string]PropertyValue, len(bag))
	for pid, v := range bag {
		if name, ok := g.pkeys.Resolve(pid); ok {
			out[name] = v
		}
	}
	return out
}

// SetEdgeLabelByHandleID attaches `name` to the edge identified by `handle`
// on the directed (srcID, dstID) NodeID pair, resolving by NodeID rather
// than natural key. It is the NodeID-keyed dual of
// [Graph.SetEdgeLabelByHandle] used by the snapshot/WAL recovery path,
// which has already restored the mapper by NodeID and must not pay a
// Resolve→Lookup round trip. No-op when handle is 0.
//
// SetEdgeLabelByHandleID is safe for concurrent use.
func (g *Graph[N, W]) SetEdgeLabelByHandleID(srcID, dstID graph.NodeID, handle uint64, name string) {
	if handle == 0 {
		return
	}
	lid := g.reg.Intern(name)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandleLabelShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]map[uint64]map[LabelID]struct{})
	}
	byHandle, ok := sh.m[k]
	if !ok {
		byHandle = make(map[uint64]map[LabelID]struct{})
		sh.m[k] = byHandle
	}
	bag, ok := byHandle[handle]
	if !ok {
		bag = make(map[LabelID]struct{})
		byHandle[handle] = bag
	}
	bag[lid] = struct{}{}
}

// SetEdgePropertyByHandleID records key=value on the edge identified by
// `handle` on the directed (srcID, dstID) NodeID pair. It is the
// NodeID-keyed dual of [Graph.SetEdgePropertyByHandle] used by the
// snapshot/WAL recovery path. No-op when handle is 0.
//
// This method is intentionally called only by the snapshot/WAL recovery
// path and bypasses the SchemaValidator: values replayed here were
// validated at the time of the original write and must not fail during
// recovery.
//
// SetEdgePropertyByHandleID is safe for concurrent use.
func (g *Graph[N, W]) SetEdgePropertyByHandleID(srcID, dstID graph.NodeID, handle uint64, key string, value PropertyValue) {
	if handle == 0 {
		return
	}
	pid := g.pkeys.Intern(key)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandlePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]map[uint64]map[PropertyKeyID]PropertyValue)
	}
	byHandle, ok := sh.m[k]
	if !ok {
		byHandle = make(map[uint64]map[PropertyKeyID]PropertyValue)
		sh.m[k] = byHandle
	}
	bag, ok := byHandle[handle]
	if !ok {
		bag = make(map[PropertyKeyID]PropertyValue)
		byHandle[handle] = bag
	}
	bag[pid] = value
}
