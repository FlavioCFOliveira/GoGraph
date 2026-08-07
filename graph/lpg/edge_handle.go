package lpg

// edge_handle.go — stable per-edge handle contract and handle-keyed
// per-instance edge metadata stores.
//
// # Stable-handle contract (Stage 1, in-memory)
//
// Every directed edge created through [Graph.AddEdgeH] is assigned a
// stable handle: a uint64 drawn from the per-graph [Graph.edgeHandleSeq]
// counter. The contract on a handle is:
//
//   - Unique per logical edge creation within a graph instance.
//   - Monotone: each [Graph.nextEdgeHandle] returns a strictly larger
//     value than the previous one.
//   - Never reused and never renumbered. Deleting an edge does NOT free
//     its handle for re-allocation, and removing a parallel sibling does
//     NOT shift the surviving edges' handles. The adjacency layer carries
//     the handle in a column parallel to the neighbour slice and copies it
//     verbatim across the slot compaction performed on delete
//     ([adjlist.AdjList.removeOneEdge]).
//   - 0 is reserved as the "no handle" sentinel. Real handles start at 1.
//     A CSR or adjacency slot whose handle is 0 either predates the first
//     AddEdgeH on the graph or belongs to a graph that never used handles.
//
// The handle lets the Cypher read path resolve a parallel edge's
// per-CREATE type and properties by an explicit, delete-stable identity
// (read from [csr.CSR.HandlesSlice] at the edge's CSR position) instead of
// re-deriving the CREATE index positionally from CSR slot order — the
// inference that mis-mapped after a delete compacted the neighbour slice.
//
// # Stores
//
// edgeHandleLabelShards / edgeHandlePropShards key per-CREATE label and
// property sets by (edgeKey, handle). They mirror the (edgeKey, idx)
// instance stores in edge_instance_labels.go / edge_instance_props.go; the
// idx stores remain as the simple-graph fallback (where parallel CREATEs
// collapse onto one slot and the read path falls back to the per-pair
// union), while the handle stores are the authoritative per-instance
// surface in multigraph mode.
//
// # Concurrency
//
// All stores are sharded by the src endpoint NodeID (mod propMapShards),
// matching every other per-edge metadata map on [Graph]; each shard's
// mutex serialises only writers landing in the same shard. All exported
// methods here are safe for concurrent use.
//
// # Stage 2 (not in this stage)
//
// Stage 2 makes the handle durable: the handle column is persisted in the
// WAL and snapshot so a recovered edge keeps its identity across a reopen
// (closing the parallel-typed-edge-collapse-on-recovery bug). The handle
// source counter is also persisted so handles stay monotone across
// restarts. A later stage substitutes a process-global uint64 Id for the
// per-graph [Graph.edgeHandleSeq] source so node and edge identities share
// one space; the per-edge metadata stores key by that Id unchanged.

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// edgeHandleLabelShard holds the per-(src, dst, handle) label sets. The
// innermost per-handle set is the compact tiered [labelBag] (sprint 221,
// #1633), stored by value, so a 1-2-label edge handle pays a small slice
// instead of a ~300 B Go map.
type edgeHandleLabelShard struct {
	m map[edgeKey]map[uint64]labelBag
	// v indexes the pre-image chains of the instances a writer has touched, so
	// a reader can reconstruct one instance's type set as of its own start
	// timestamp (rmp #2291). Keyed by (pair, handle) rather than by pair, so a
	// write copies ONE instance's small bag instead of the whole pair's inner
	// map — O(1) against O(parallel edges) per write.
	v  sideVersions[edgeHandleKey, labelBag]
	mu sync.Mutex
}

// edgeHandlePropShard holds the per-(src, dst, handle) property bags. The
// innermost per-handle bag is the compact tiered [propBag] (sprint 221,
// #1633), stored by value, so a 1-2-property edge handle pays a small slice
// instead of a ~300 B Go map.
type edgeHandlePropShard struct {
	m map[edgeKey]map[uint64]propBag
	// v indexes the pre-image chains of the instances a writer has touched.
	// See [edgeHandleLabelShard].v for why it is keyed by (pair, handle).
	v  sideVersions[edgeHandleKey, propBag]
	mu sync.Mutex
}

// edgeHandleLabelShardFor selects the responsible label shard for k.
func (g *Graph[N, W]) edgeHandleLabelShardFor(k edgeKey) *edgeHandleLabelShard {
	return &g.edgeHandleLabelShards[uint64(k.src)&(propMapShards-1)]
}

// edgeHandlePropShardFor selects the responsible property shard for k.
func (g *Graph[N, W]) edgeHandlePropShardFor(k edgeKey) *edgeHandlePropShard {
	return &g.edgeHandlePropShards[uint64(k.src)&(propMapShards-1)]
}

// edgeHandleHasLabel reports whether the edge identified by handle on the
// directed (srcID, dstID) pair carries lid, and whether a handle-keyed label
// record exists for it at all.
//
// known is false when handle is the 0 sentinel or no label was ever recorded
// against it — the caller must then fall back to another source of the
// relationship type rather than read the absence as "does not carry lid".
//
// It is the allocation-free, LabelID-level counterpart of
// [Graph.EdgeLabelsByHandleID], which resolves every id back to a name and
// builds a slice. A per-slot walk over a hub's adjacency cannot pay either
// (rmp #2241).
//
// edgeHandleHasLabel is safe for concurrent use.
func (g *Graph[N, W]) edgeHandleHasLabel(srcID, dstID graph.NodeID, handle uint64, lid LabelID) (has, known bool) {
	return g.edgeHandleHasLabelAsOf(srcID, dstID, handle, lid, nil)
}

// edgeHandleHasLabelAsOf is [Graph.edgeHandleHasLabel] as the instance stood at
// snap.
func (g *Graph[N, W]) edgeHandleHasLabelAsOf(srcID, dstID graph.NodeID, handle uint64, lid LabelID, snap *Snapshot) (has, known bool) {
	if handle == 0 {
		return false, false
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandleLabelShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	bag, ok := g.handleLabelBagAsOf(sh, k, handle, snap)
	if !ok {
		return false, false
	}
	return bag.has(lid), true
}

// HasEdgeHandleLabelRecordByID reports whether the by-handle label store holds a
// relationship-type record for handle on the directed (srcID, dstID) pair.
//
// It is the presence question on its own, without the types: a slot WITHOUT a
// record is COLUMN-TYPED — its relationship type lives in the adjacency label
// column and [Graph.ForEachSlotRelTypeByID] is what reads it — while a slot WITH
// one has the record as its authority. [Graph.slotCarriesType] applies exactly
// that precedence, so a caller classifying slots must ask the same question or the
// two will disagree about which source owns a slot.
//
// It exists because the alternative probe — calling [Graph.EdgeLabelsByHandle] and
// testing the result for emptiness — resolves every id to a name and allocates a
// []string per slot, which a per-slot classification sweep over a whole graph
// cannot pay. This allocates nothing and takes one map lookup under the pair's
// shard lock. It also needs no Mapper round-trip, taking NodeIDs directly.
//
// HasEdgeHandleLabelRecordByID is safe for concurrent use.
func (g *Graph[N, W]) HasEdgeHandleLabelRecordByID(srcID, dstID graph.NodeID, handle uint64) bool {
	return g.HasEdgeHandleLabelRecordByIDAsOf(srcID, dstID, handle, nil)
}

// HasEdgeHandleLabelRecordByIDAsOf is [Graph.HasEdgeHandleLabelRecordByID] as
// the store stood at snap. The precedence it feeds — a slot WITH a record is
// handle-typed, one WITHOUT is column-typed — must be resolved at the reader's
// own instant, or a reader from before a CREATE would classify a slot by a
// record that did not exist for it.
//
// Safe for concurrent use.
func (g *Graph[N, W]) HasEdgeHandleLabelRecordByIDAsOf(srcID, dstID graph.NodeID, handle uint64, snap *Snapshot) bool {
	if handle == 0 {
		return false
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandleLabelShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	_, ok := g.handleLabelBagAsOf(sh, k, handle, snap)
	return ok
}

// SetEdgeLabelByHandle attaches name to the directed edge identified by
// the stable handle on the (src, dst) pair. No-op when handle is 0 (the
// no-handle sentinel) or when either endpoint is unknown to the mapper.
//
// SetEdgeLabelByHandle is safe for concurrent use.
func (g *Graph[N, W]) SetEdgeLabelByHandle(src, dst N, handle uint64, name string) {
	g.setEdgeLabelByHandleInfo(src, dst, handle, name, nil)
}

// setEdgeLabelByHandleInfo is [Graph.SetEdgeLabelByHandle] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) setEdgeLabelByHandleInfo(src, dst N, handle uint64, name string, tx *writeCtx) {
	if handle == 0 {
		return
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return
	}
	lid := g.reg.Intern(name)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandleLabelShardFor(k)
	// changed is read by the FIRST-registered defer, which therefore runs LAST —
	// after sh.mu.Unlock below. That ordering is deliberate: the derived
	// edge-label set is inside [Graph.TopoGeneration]'s scope (rmp #2255), and
	// rmp #2151/fafc50c7 established that the epoch must move only once the write
	// it announces is fully published, so a reader sampling the new epoch cannot
	// miss it.
	changed := false
	defer func() {
		if changed {
			g.topoGeneration.Add(1)
		}
	}()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]map[uint64]labelBag)
	}
	byHandle, ok := sh.m[k]
	if !ok {
		byHandle = make(map[uint64]labelBag)
		sh.m[k] = byHandle
	}
	// labelBag is stored by value: mutate a local copy and write it back under
	// the shard lock (the write-back is load-bearing — add may grow/promote).
	bag := byHandle[handle]
	if bag.has(lid) {
		// Already recorded for this handle. Re-asserting it must NOT bump the
		// epoch: the MERGE MATCH branch (cypher/exec/merge_relationship.go) calls
		// through here for every MERGE that binds an existing relationship, and a
		// spurious bump would invalidate the forward/reverse CSR pair cache and
		// force an O(V+E) rebuild for a mutation that changed nothing.
		return
	}
	if !g.pushHandleLabelVersion(sh, k, handle, tx) {
		// Refused: the conflict is recorded on tx and this write must not land,
		// so the epoch must not move either.
		return
	}
	bag.add(lid)
	byHandle[handle] = bag
	changed = true
}

// EdgeLabelsByHandle returns the labels recorded for the edge identified
// by handle on the (src, dst) pair. Returns nil when handle is 0, the
// handle was never labelled, either endpoint is unknown, or no handle
// store has been initialised for this pair.
//
// Like the (src, dst, idx) instance stores, this handle store is guarded
// by its own per-shard mutex and is only per-operation atomic: it is NOT
// cross-store consistent with [Graph.EdgeCreateCount],
// [Graph.EdgePropertiesByHandle], or the adjacency layer outside a
// transaction barrier. To read a consistent cross-store view, bracket
// the correlated reads in [Graph.View] (writers commit under
// [Graph.ApplyAtomically]); see docs/isolation-design.md.
//
// EdgeLabelsByHandle is safe for concurrent use.
func (g *Graph[N, W]) EdgeLabelsByHandle(src, dst N, handle uint64) []string {
	if handle == 0 {
		return nil
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	return g.EdgeLabelsByHandleIDAsOf(srcID, dstID, handle, nil)
}

// EdgeLabelsByHandleAsOf is [Graph.EdgeLabelsByHandle] as the instance stood at
// snap. A nil snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgeLabelsByHandleAsOf(src, dst N, handle uint64, snap *Snapshot) []string {
	if handle == 0 {
		return nil
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	return g.EdgeLabelsByHandleIDAsOf(srcID, dstID, handle, snap)
}

// SetEdgePropertyByHandle records key=value for the edge identified by
// handle on the (src, dst) pair. No-op when handle is 0 or when either
// endpoint is unknown to the mapper. Returns any error returned by the
// installed [SchemaValidator]; when the validator rejects the write the
// graph state is left unchanged.
//
// SetEdgePropertyByHandle is safe for concurrent use.
func (g *Graph[N, W]) SetEdgePropertyByHandle(src, dst N, handle uint64, key string, value PropertyValue) error {
	return g.setEdgePropertyByHandleInfo(src, dst, handle, key, value, nil)
}

// setEdgePropertyByHandleInfo is [Graph.SetEdgePropertyByHandle] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) setEdgePropertyByHandleInfo(src, dst N, handle uint64, key string, value PropertyValue, tx *writeCtx) error {
	if v := g.validator.load(); v != nil {
		if err := v.Validate(key, value); err != nil {
			return err
		}
	}
	if handle == 0 {
		return nil
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	pid := g.pkeys.Intern(key)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandlePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]map[uint64]propBag)
	}
	byHandle, ok := sh.m[k]
	if !ok {
		byHandle = make(map[uint64]propBag)
		sh.m[k] = byHandle
	}
	// propBag is stored by value: mutate a local copy and write it back under
	// the shard lock (the write-back is load-bearing — set may grow/promote).
	bag := byHandle[handle]
	if !g.pushHandlePropVersion(sh, k, handle, tx) {
		return nil
	}
	bag.set(pid, value)
	byHandle[handle] = bag
	return nil
}

// EdgePropertiesByHandle returns the property map recorded for the edge
// identified by handle on the (src, dst) pair. Returns nil when handle is
// 0, the handle was never written, or either endpoint is unknown.
//
// Like the (src, dst, idx) instance stores, this handle store is guarded
// by its own per-shard mutex and is only per-operation atomic: it is NOT
// cross-store consistent with [Graph.EdgeCreateCount],
// [Graph.EdgeLabelsByHandle], or the adjacency layer outside a
// transaction barrier. To read a consistent cross-store view, bracket
// the correlated reads in [Graph.View] (writers commit under
// [Graph.ApplyAtomically]); see docs/isolation-design.md.
//
// EdgePropertiesByHandle is safe for concurrent use.
func (g *Graph[N, W]) EdgePropertiesByHandle(src, dst N, handle uint64) map[string]PropertyValue {
	if handle == 0 {
		return nil
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	return g.EdgePropertiesByHandleIDAsOf(srcID, dstID, handle, nil)
}

// EdgePropertiesByHandleAsOf is [Graph.EdgePropertiesByHandle] as the instance
// stood at snap. A nil snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) EdgePropertiesByHandleAsOf(src, dst N, handle uint64, snap *Snapshot) map[string]PropertyValue {
	if handle == 0 {
		return nil
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	return g.EdgePropertiesByHandleIDAsOf(srcID, dstID, handle, snap)
}

// FirstEdgeHandle returns the stable handle stamped on the FIRST adjacency
// slot from src to dst — the slot a subsequent [Graph.RemoveEdge] would
// remove, because [adjlist.AdjList.RemoveEdge] removes the lowest-indexed
// occurrence and compacts the handle column in lock-step. The boolean
// reports whether such a slot exists AND carries a non-zero handle; it is
// false when either endpoint is unknown, no src→dst edge exists, or the
// matched slot has the 0 "no handle" sentinel (a simple-graph or
// pre-Stage-2 edge).
//
// It lets the write-query transaction-undo log capture the identity of the
// exact parallel edge instance a DELETE is about to remove, so the inverse
// can re-add that instance with its ORIGINAL handle (via
// [Graph.AddEdgeHIfAbsent]) and the surviving siblings keep theirs — fully
// reverting an "remove one parallel edge, then fail a later row" rollback
// without renumbering any handle. See cypher/undo_record.go.
//
// FirstEdgeHandle reads an immutable adjacency snapshot ([adjlist.AdjList.LoadEntryH])
// and allocates nothing; it is safe for concurrent use under the same
// lock-free contract as [Graph.EdgeWeight].
func (g *Graph[N, W]) FirstEdgeHandle(src, dst N) (uint64, bool) {
	return g.FirstEdgeHandleAsOf(src, dst, nil)
}

// FirstEdgeHandleAsOf is [Graph.FirstEdgeHandle] as the pair stood at snap. A
// nil snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) FirstEdgeHandleAsOf(src, dst N, snap *Snapshot) (uint64, bool) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return 0, false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return 0, false
	}
	v := g.EntryViewAsOf(srcID, snap)
	neighbours, handles := v.Neighbours, v.Handles
	if handles == nil {
		return 0, false
	}
	for i, nb := range neighbours {
		if nb == dstID {
			if i < len(handles) && handles[i] != 0 {
				return handles[i], true
			}
			return 0, false
		}
	}
	return 0, false
}

// DelEdgePropertyByHandle removes exactly key from the property bag of the
// edge identified by handle on the (src, dst) pair, leaving every other
// property of that handle — and every sibling handle on the same pair —
// untouched. No-op when handle is 0 (the no-handle sentinel), when either
// endpoint is unknown to the mapper, when no handle store exists for the
// pair, or when the handle never carried key. When the removal empties the
// handle's bag the inner byHandle[handle] entry is pruned, and when that
// leaves the pair with no handles the outer sh.m[k] entry is pruned too,
// mirroring the pruning [Graph.RemoveEdgeInstanceByHandle] performs.
//
// It is the single-key analogue of [Graph.RemoveEdgeInstanceByHandle] (which
// drops ALL of a handle's labels and properties): a Cypher REMOVE r.x or
// SET r.x = null on one parallel edge must delete only x from that one
// instance, not the whole instance. The per-pair coalesced store is mutated
// separately by the caller (dual-write); this method only touches the
// handle-keyed per-instance store.
//
// DelEdgePropertyByHandle is safe for concurrent use.
func (g *Graph[N, W]) DelEdgePropertyByHandle(src, dst N, handle uint64, key string) {
	g.delEdgePropertyByHandleInfo(src, dst, handle, key, nil)
}

// delEdgePropertyByHandleInfo is [Graph.DelEdgePropertyByHandle] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) delEdgePropertyByHandleInfo(src, dst N, handle uint64, key string, tx *writeCtx) {
	if handle == 0 {
		return
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return
	}
	pid, ok := g.pkeys.Lookup(key)
	if !ok {
		// The key was never interned, so no handle can carry it.
		return
	}
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeHandlePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	byHandle, ok := sh.m[k]
	if !ok {
		return
	}
	bag, ok := byHandle[handle]
	if !ok {
		return
	}
	// propBag is stored by value: mutate a local copy and either write it back
	// or drop the entry when the removal emptied it.
	if !g.pushHandlePropVersion(sh, k, handle, tx) {
		return
	}
	if bag.del(pid) {
		delete(byHandle, handle)
		if len(byHandle) == 0 {
			delete(sh.m, k)
		}
		return
	}
	byHandle[handle] = bag
}

// RemoveEdgeInstanceByHandle discards every per-handle label and property
// for (src, dst) at handle so subsequent reads (EdgeLabelsByHandle /
// EdgePropertiesByHandle) return empty. The handle-keyed analogue of
// [Graph.RemoveEdgeInstance]; used by DELETE to drop one logical edge
// while leaving sibling handles untouched. No-op when handle is 0.
//
// RemoveEdgeInstanceByHandle is safe for concurrent use.
func (g *Graph[N, W]) RemoveEdgeInstanceByHandle(src, dst N, handle uint64) {
	g.removeEdgeInstanceByHandleInfo(src, dst, handle, nil)
}

// removeEdgeInstanceByHandleInfo is [Graph.RemoveEdgeInstanceByHandle] with an explicit write transaction; tx is
// nil for a direct Go-API mutation, which is committed the instant it is made
// and takes no conflict check. See [writeCtx].
func (g *Graph[N, W]) removeEdgeInstanceByHandleInfo(src, dst N, handle uint64, tx *writeCtx) {
	if handle == 0 {
		return
	}
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return
	}
	k := edgeKey{src: srcID, dst: dstID}
	{
		sh := g.edgeHandleLabelShardFor(k)
		sh.mu.Lock()
		if byHandle, ok := sh.m[k]; ok && g.pushHandleLabelVersion(sh, k, handle, tx) {
			delete(byHandle, handle)
			if len(byHandle) == 0 {
				delete(sh.m, k)
			}
		}
		sh.mu.Unlock()
	}
	{
		sh := g.edgeHandlePropShardFor(k)
		sh.mu.Lock()
		if byHandle, ok := sh.m[k]; ok && g.pushHandlePropVersion(sh, k, handle, tx) {
			delete(byHandle, handle)
			if len(byHandle) == 0 {
				delete(sh.m, k)
			}
		}
		sh.mu.Unlock()
	}
}
