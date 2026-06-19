package lpg

// edge_instance_labels.go — per-CREATE-instance edge label storage.
//
// Sidecar to the per-pair edge_labels.go store so each Cypher CREATE
// call can carry its own label set independent of the merged union the
// adjacency layer maintains. The instance index is the value returned
// by [Graph.IncEdgeCreateCount] for the (src, dst) pair at the time
// CreateRelationship runs, which is 1-based and monotonically
// increasing across CREATE calls targeting the same endpoint pair.
//
// Read path:
//   - [Graph.EdgeLabels] keeps returning the per-pair union, preserving
//     the historical semantics every read in the codebase relies on.
//   - [Graph.EdgeLabelsAt] returns just the labels recorded at a
//     specific instance index, used by Cypher Expand to filter
//     parallel edges by their CREATE-time label rather than the
//     merged union (closes Match2 [6] / Match7 [29] regressions that
//     surface when adjlist.Config.Multigraph is enabled).
//
// Write path:
//   - [Graph.SetEdgeLabelAt] stores per-instance. The existing
//     [Graph.SetEdgeLabel] keeps updating the union store.
//   - CreateRelationship calls both: SetEdgeLabel for the union and
//     SetEdgeLabelAt for the just-incremented instance.

import (
	"sync"
)

// edgeInstanceLabelShard holds the per-(src, dst, idx) label sets. The
// innermost per-instance set is the compact tiered [labelBag] (sprint 221,
// #1633), stored by value, so a 1-2-label edge instance pays a small slice
// instead of a ~300 B Go map.
type edgeInstanceLabelShard struct {
	mu sync.Mutex
	m  map[edgeKey]map[int64]labelBag
}

// SetEdgeLabelAt attaches `name` to the directed edge instance
// (src, dst) at the supplied 1-based CREATE index. No-op when either
// endpoint is unknown to the underlying mapper.
//
// SetEdgeLabelAt is safe for concurrent use.
func (g *Graph[N, W]) SetEdgeLabelAt(src, dst N, idx int64, name string) {
	if idx <= 0 {
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
	sh := g.edgeInstanceLabelShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]map[int64]labelBag)
	}
	byIdx, ok := sh.m[k]
	if !ok {
		byIdx = make(map[int64]labelBag)
		sh.m[k] = byIdx
	}
	// labelBag is stored by value: mutate a local copy and write it back under
	// the shard lock (the write-back is load-bearing — add may grow/promote).
	bag := byIdx[idx]
	bag.add(lid)
	byIdx[idx] = bag
}

// EdgeLabelsAt returns the labels recorded at instance `idx` of the
// directed edge (src, dst). Returns nil when the instance was never
// labelled, when either endpoint is unknown, or when no per-instance
// store has been initialised for this pair.
//
// This per-instance store is guarded by its own per-shard mutex and is
// only per-operation atomic: it is NOT cross-store consistent with
// [Graph.EdgeCreateCount], [Graph.EdgePropertiesAt], or the adjacency
// layer outside a transaction barrier. A reader correlating this with
// [Graph.EdgeCreateCount] while a multi-CREATE multigraph transaction
// commits can observe a partial cross-store state. To read a consistent
// cross-store view, bracket the correlated reads in [Graph.View]
// (writers commit under [Graph.ApplyAtomically]); see
// docs/isolation-design.md.
//
// EdgeLabelsAt is safe for concurrent use.
func (g *Graph[N, W]) EdgeLabelsAt(src, dst N, idx int64) []string {
	if idx <= 0 {
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
	sh := g.edgeInstanceLabelShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	byIdx, ok := sh.m[k]
	if !ok {
		return nil
	}
	bag, ok := byIdx[idx]
	if !ok {
		return nil
	}
	out := make([]string, 0, bag.len())
	bag.forEach(func(lid LabelID) {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	})
	return out
}

// RemoveEdgeInstance discards every per-instance label and property
// for (src, dst) at `idx` so subsequent reads (EdgeLabelsAt /
// EdgePropertiesAt) return empty. Used by DELETE to drop a specific
// logical edge while leaving sibling instances at other indices
// untouched.
//
// RemoveEdgeInstance is safe for concurrent use.
func (g *Graph[N, W]) RemoveEdgeInstance(src, dst N, idx int64) {
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
		sh := g.edgeInstanceLabelShardFor(k)
		sh.mu.Lock()
		if byIdx, ok := sh.m[k]; ok {
			delete(byIdx, idx)
			if len(byIdx) == 0 {
				delete(sh.m, k)
			}
		}
		sh.mu.Unlock()
	}
	{
		sh := g.edgeInstancePropShardFor(k)
		sh.mu.Lock()
		if byIdx, ok := sh.m[k]; ok {
			delete(byIdx, idx)
			if len(byIdx) == 0 {
				delete(sh.m, k)
			}
		}
		sh.mu.Unlock()
	}
}

// edgeInstanceLabelShardFor selects the responsible shard.
func (g *Graph[N, W]) edgeInstanceLabelShardFor(k edgeKey) *edgeInstanceLabelShard {
	return &g.edgeInstanceLabelShards[uint64(k.src)&(propMapShards-1)]
}
