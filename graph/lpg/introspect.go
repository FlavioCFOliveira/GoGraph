package lpg

import "github.com/FlavioCFOliveira/GoGraph/graph"

// This file holds the read-only schema-introspection enumerators:
// NodeLabelsInUse, RelationshipTypesInUse, and PropertyKeysInUse. They
// report the distinct names currently borne by live (non-tombstoned)
// graph elements and back the Cypher procedures db.labels(),
// db.relationshipTypes(), and db.propertyKeys().
//
// All three are read-only snapshot enumerators. Each iterates the 16
// label/property shards one at a time, acquiring a single shard RLock at
// a time (never two simultaneously), deduplicating the interned ids into
// a set before resolving them to strings via the lock-free registries.
// They are safe for concurrent use with each other and with mutators;
// the result is a point-in-time snapshot whose elements are not
// guaranteed to reflect any single instant across shards.
//
// Empty-result convention: all three return a freshly allocated, non-nil
// empty slice (len 0) when nothing matches. The caller owns the returned
// slice and may mutate it. Order is unspecified.

// edgeEndpointLive reports whether both endpoints of k are live, i.e.
// neither src nor dst is tombstoned. When no tombstone exists in the
// graph (tombstoneActive == 0) every node is live, so the call resolves
// without touching the tombstone lock.
func (g *Graph[N, W]) edgeEndpointLive(k edgeKey) bool {
	if g.tombstoneActive.Load() == 0 {
		return true
	}
	return !g.IsTombstoned(k.src) && !g.IsTombstoned(k.dst)
}

// NodeLabelsInUse returns the distinct names of every label currently
// attached to at least one non-tombstoned node, in unspecified order.
// Labels borne only by tombstoned (removed) nodes are excluded.
//
// The returned slice is freshly allocated and non-nil; when no live node
// carries a label it is empty (len 0). The caller owns the slice and may
// mutate it.
//
// NodeLabelsInUse is safe for concurrent use. It snapshots each of the 16
// node-label shards under that shard's RLock (one at a time) and resolves
// ids through the lock-free [LabelRegistry]. The result is a point-in-time
// view and is not guaranteed to be consistent across shards.
func (g *Graph[N, W]) NodeLabelsInUse() []string {
	tombstoned := g.tombstoneActive.Load() != 0
	seen := make(map[LabelID]struct{})
	for i := range g.nodeLabelShards {
		sh := &g.nodeLabelShards[i]
		sh.mu.RLock()
		for id, bag := range sh.m {
			if len(bag) == 0 {
				continue
			}
			if tombstoned && g.IsTombstoned(id) {
				continue
			}
			for lid := range bag {
				seen[lid] = struct{}{}
			}
		}
		sh.mu.RUnlock()
	}
	return g.resolveLabelIDs(seen)
}

// RelationshipTypesInUse returns the distinct names of every edge label
// attached to at least one edge whose endpoints are both non-tombstoned,
// in unspecified order. An edge label survives only while at least one
// live edge (both endpoints live) still bears it.
//
// The returned slice is freshly allocated and non-nil; when no live edge
// carries a label it is empty (len 0). The caller owns the slice and may
// mutate it.
//
// RelationshipTypesInUse is safe for concurrent use. It walks the inline
// per-slot label column of every source's adjacency (the lock-free snapshot)
// and each of the 16 edge-label overflow shards under that shard's RLock (one
// at a time), and resolves ids through the lock-free [LabelRegistry]. The
// result is a point-in-time view and is not guaranteed to be consistent across
// shards.
func (g *Graph[N, W]) RelationshipTypesInUse() []string {
	seen := make(map[LabelID]struct{})
	// Inline labels: walk every source's adjacency label column. A label
	// counts only while both endpoints of the bearing slot are live.
	maxID := uint64(g.adj.MaxNodeID())
	for id := uint64(0); id < maxID; id++ {
		srcID := graph.NodeID(id)
		labs := g.adj.LoadEntryLabels(srcID)
		if labs == nil {
			continue
		}
		nbs, _ := g.adj.LoadEntry(srcID)
		n := len(nbs)
		if len(labs) < n {
			n = len(labs)
		}
		for i := 0; i < n; i++ {
			lid, ok := decodeSlotLabel(labs[i])
			if !ok {
				continue
			}
			if !g.edgeEndpointLive(edgeKey{src: srcID, dst: nbs[i]}) {
				continue
			}
			seen[lid] = struct{}{}
		}
	}
	// Overflow labels (multi-label and orphaned pairs).
	for i := range g.edgeLabelShards {
		sh := &g.edgeLabelShards[i]
		sh.mu.RLock()
		for k, ls := range sh.overflow {
			if !g.edgeEndpointLive(k) {
				continue
			}
			for _, lid := range ls {
				seen[lid] = struct{}{}
			}
		}
		sh.mu.RUnlock()
	}
	return g.resolveLabelIDs(seen)
}

// PropertyKeysInUse returns the distinct names of every property key
// present on at least one non-tombstoned node, or on at least one edge
// whose endpoints are both non-tombstoned, in unspecified order. The
// result is the union across the node and edge property stores.
//
// The returned slice is freshly allocated and non-nil; when no live
// element carries a property it is empty (len 0). The caller owns the
// slice and may mutate it.
//
// PropertyKeysInUse is safe for concurrent use. It snapshots each of the
// 16 node-property shards and each of the 16 edge-property shards under
// that shard's RLock (one at a time) and resolves ids through the
// lock-free [PropertyKeyRegistry]. The result is a point-in-time view and
// is not guaranteed to be consistent across shards.
func (g *Graph[N, W]) PropertyKeysInUse() []string {
	tombstoned := g.tombstoneActive.Load() != 0
	seen := make(map[PropertyKeyID]struct{})

	for i := range g.nodePropShards {
		sh := &g.nodePropShards[i]
		sh.mu.RLock()
		for id, bag := range sh.m {
			if len(bag) == 0 {
				continue
			}
			if tombstoned && g.IsTombstoned(id) {
				continue
			}
			for pk := range bag {
				seen[pk] = struct{}{}
			}
		}
		sh.mu.RUnlock()
	}

	for i := range g.edgePropShards {
		sh := &g.edgePropShards[i]
		sh.mu.RLock()
		for k, bag := range sh.m {
			if len(bag) == 0 {
				continue
			}
			if !g.edgeEndpointLive(k) {
				continue
			}
			for pk := range bag {
				seen[pk] = struct{}{}
			}
		}
		sh.mu.RUnlock()
	}

	out := make([]string, 0, len(seen))
	for pk := range seen {
		if name, ok := g.pkeys.Resolve(pk); ok {
			out = append(out, name)
		}
	}
	return out
}

// resolveLabelIDs turns a deduplicated set of label ids into a
// freshly allocated, non-nil slice of their names, dropping any id the
// registry cannot resolve.
func (g *Graph[N, W]) resolveLabelIDs(seen map[LabelID]struct{}) []string {
	out := make([]string, 0, len(seen))
	for lid := range seen {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	}
	return out
}
