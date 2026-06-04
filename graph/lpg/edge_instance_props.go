package lpg

// edge_instance_props.go — per-CREATE-instance edge property storage.
//
// Mirror of edge_instance_labels.go for properties. Each CREATE call
// records its property map under the 1-based instance index returned
// by [Graph.IncEdgeCreateCount]. The per-pair [Graph.EdgeProperties]
// surface keeps returning the latest-wins merge (existing behaviour);
// [Graph.EdgePropertiesAt] returns the snapshot captured at one
// specific CREATE.

import (
	"sync"
)

// edgeInstancePropShard holds the per-(src, dst, idx) property maps.
type edgeInstancePropShard struct {
	mu sync.Mutex
	m  map[edgeKey]map[int64]map[PropertyKeyID]PropertyValue
}

// SetEdgePropertyAt records the property `key`=`value` for the directed
// edge instance (src, dst) at the supplied 1-based CREATE index.
//
// SetEdgePropertyAt is safe for concurrent use.
func (g *Graph[N, W]) SetEdgePropertyAt(src, dst N, idx int64, key string, value PropertyValue) {
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
	pid := g.pkeys.Intern(key)
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeInstancePropShardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.m == nil {
		sh.m = make(map[edgeKey]map[int64]map[PropertyKeyID]PropertyValue)
	}
	byIdx, ok := sh.m[k]
	if !ok {
		byIdx = make(map[int64]map[PropertyKeyID]PropertyValue)
		sh.m[k] = byIdx
	}
	bag, ok := byIdx[idx]
	if !ok {
		bag = make(map[PropertyKeyID]PropertyValue)
		byIdx[idx] = bag
	}
	bag[pid] = value
}

// EdgePropertiesAt returns the property map recorded at instance `idx`
// of the directed edge (src, dst). Returns nil when the instance was
// never written or when either endpoint is unknown.
//
// This per-instance store is guarded by its own per-shard mutex and is
// only per-operation atomic: it is NOT cross-store consistent with
// [Graph.EdgeCreateCount], [Graph.EdgeLabelsAt], or the adjacency layer
// outside a transaction barrier. A reader correlating the count of
// populated instance indices with [Graph.EdgeCreateCount] while a
// multi-CREATE multigraph transaction commits can observe a partial
// cross-store state (count ahead of the populated indices). To read a
// consistent cross-store view, bracket the correlated reads in
// [Graph.View] (writers commit under [Graph.ApplyAtomically]); see
// docs/isolation-design.md.
//
// EdgePropertiesAt is safe for concurrent use.
func (g *Graph[N, W]) EdgePropertiesAt(src, dst N, idx int64) map[string]PropertyValue {
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
	sh := g.edgeInstancePropShardFor(k)
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
	out := make(map[string]PropertyValue, len(bag))
	for pid, v := range bag {
		if name, ok := g.pkeys.Resolve(pid); ok {
			out[name] = v
		}
	}
	return out
}

// edgeInstancePropShardFor selects the responsible shard.
func (g *Graph[N, W]) edgeInstancePropShardFor(k edgeKey) *edgeInstancePropShard {
	return &g.edgeInstancePropShards[uint64(k.src)&(propMapShards-1)]
}
