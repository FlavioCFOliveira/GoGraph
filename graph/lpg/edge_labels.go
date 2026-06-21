package lpg

import "github.com/FlavioCFOliveira/GoGraph/graph"

// EdgeLabels returns the names of every label attached to the
// directed edge (src, dst) in unspecified order. The returned slice
// is freshly allocated and may be mutated by the caller. If either
// endpoint is unknown or the endpoint pair has no labels attached,
// EdgeLabels returns nil.
//
// EdgeLabels is the dual of [Graph.NodeLabels]. It is safe for
// concurrent use; the snapshot is taken under the per-shard RWMutex
// (one of 16 stripes keyed by the src endpoint) and the registry's
// own lock.
//
// The returned set is DERIVED: the union of the relationship type stored
// inline in each dst-matching adjacency slot and the per-shard overflow store
// (the second-and-later types of a multi-label pair and any orphaned types).
// Distinct labels are deduplicated across both sources, so a multigraph pair
// whose parallel slots happen to share a type reports it once.
func (g *Graph[N, W]) EdgeLabels(src, dst N) []string {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	return g.EdgeLabelsByID(srcID, dstID)
}

// EdgeLabelsByID is the NodeID-keyed counterpart of [Graph.EdgeLabels]: it
// returns the labels attached to the directed edge identified by the endpoint
// NodeIDs (srcID, dstID), in unspecified order, or nil when the pair carries no
// labels. It is the edge dual of [Graph.NodeLabelsByID].
//
// Unlike [Graph.EdgeLabels] it performs NO Mapper access — no external-key →
// NodeID lookup — so a caller that already holds both endpoint NodeIDs can
// resolve edge labels without re-entering the Mapper. This is precisely what the
// snapshot collectors require: they enumerate endpoints from inside
// [graph.Mapper.Walk], which holds a Mapper shard read lock across its callback,
// and the Mapper contract forbids re-entry there while a writer may be running
// (graph/mapper.go:337-345, #1648). The label snapshot is still taken under the
// per-shard edge-label RWMutex and the registry's own lock, so EdgeLabelsByID is
// safe for concurrent use.
func (g *Graph[N, W]) EdgeLabelsByID(srcID, dstID graph.NodeID) []string {
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.RLock()
	// Collect the distinct label ids from the inline slots and the overflow
	// under the shard RLock, then resolve names. A small set deduplicates the
	// two sources; the common single-label case touches it once.
	var ids []LabelID
	seen := func(lid LabelID) bool {
		for _, x := range ids {
			if x == lid {
				return true
			}
		}
		return false
	}
	g.slotLabelsForPair(srcID, dstID, func(lid LabelID) {
		if !seen(lid) {
			ids = append(ids, lid)
		}
	})
	for _, lid := range sh.overflow[k] {
		if !seen(lid) {
			ids = append(ids, lid)
		}
	}
	sh.mu.RUnlock()
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, lid := range ids {
		if name, ok := g.reg.Resolve(lid); ok {
			out = append(out, name)
		}
	}
	return out
}
