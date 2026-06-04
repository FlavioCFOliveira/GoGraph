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
)

// edgeHandleLabelShard holds the per-(src, dst, handle) label sets.
type edgeHandleLabelShard struct {
	mu sync.Mutex
	m  map[edgeKey]map[uint64]map[LabelID]struct{}
}

// edgeHandlePropShard holds the per-(src, dst, handle) property maps.
type edgeHandlePropShard struct {
	mu sync.Mutex
	m  map[edgeKey]map[uint64]map[PropertyKeyID]PropertyValue
}

// edgeHandleLabelShardFor selects the responsible label shard for k.
func (g *Graph[N, W]) edgeHandleLabelShardFor(k edgeKey) *edgeHandleLabelShard {
	return &g.edgeHandleLabelShards[uint64(k.src)&(propMapShards-1)]
}

// edgeHandlePropShardFor selects the responsible property shard for k.
func (g *Graph[N, W]) edgeHandlePropShardFor(k edgeKey) *edgeHandlePropShard {
	return &g.edgeHandlePropShards[uint64(k.src)&(propMapShards-1)]
}

// SetEdgeLabelByHandle attaches name to the directed edge identified by
// the stable handle on the (src, dst) pair. No-op when handle is 0 (the
// no-handle sentinel) or when either endpoint is unknown to the mapper.
//
// SetEdgeLabelByHandle is safe for concurrent use.
func (g *Graph[N, W]) SetEdgeLabelByHandle(src, dst N, handle uint64, name string) {
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

// SetEdgePropertyByHandle records key=value for the edge identified by
// handle on the (src, dst) pair. No-op when handle is 0 or when either
// endpoint is unknown to the mapper.
//
// SetEdgePropertyByHandle is safe for concurrent use.
func (g *Graph[N, W]) SetEdgePropertyByHandle(src, dst N, handle uint64, key string, value PropertyValue) {
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
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return 0, false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return 0, false
	}
	neighbours, _, handles := g.adj.LoadEntryH(srcID)
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

// RemoveEdgeInstanceByHandle discards every per-handle label and property
// for (src, dst) at handle so subsequent reads (EdgeLabelsByHandle /
// EdgePropertiesByHandle) return empty. The handle-keyed analogue of
// [Graph.RemoveEdgeInstance]; used by DELETE to drop one logical edge
// while leaving sibling handles untouched. No-op when handle is 0.
//
// RemoveEdgeInstanceByHandle is safe for concurrent use.
func (g *Graph[N, W]) RemoveEdgeInstanceByHandle(src, dst N, handle uint64) {
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
		if byHandle, ok := sh.m[k]; ok {
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
		if byHandle, ok := sh.m[k]; ok {
			delete(byHandle, handle)
			if len(byHandle) == 0 {
				delete(sh.m, k)
			}
		}
		sh.mu.Unlock()
	}
}
