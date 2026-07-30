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

// ForEachSlotRelTypeByID streams the relationship types carried by ONE
// column-typed adjacency slot of the directed pair (srcID → dstID) — the slot
// whose own label column entry is encoded — invoking visit once per resolved type
// name. encoded is the raw column value: 0 means the slot carries no inline type.
//
// It is the per-SLOT counterpart of [Graph.ForEachEdgeLabelByID], which streams
// the pair's whole derived union. A caller that must decide what ONE parallel edge
// is typed as cannot use the union: on a multigraph pair holding one :K edge and
// one untyped edge the union reports :K for both, so a pattern `()-[r:K]->()`
// matched twice where once was correct (rmp #2258).
//
// A column-typed slot carries its own inline type plus every type in the pair's
// overflow list. Overflow holds a type [Graph.SetEdgeLabel] could not place
// per-slot because no slot was free; SetEdgeLabel names the PAIR, so such a type
// belongs to every column-typed slot of it. Nothing else is visited — in
// particular, no sibling slot's inline type — which is what makes the answer
// per-slot.
//
// This is the accessor for a slot whose type is NOT recorded against a stable
// per-edge handle. When a handle record exists it is authoritative for that slot
// and [Graph.EdgeLabelsByHandleID] is the accessor to use; see
// [Graph.slotCarriesType], which applies the same precedence.
//
// Names are resolved and visited after the edge-label shard read lock is
// released, exactly as ForEachEdgeLabelByID does, so visit may safely read the
// graph. It allocates nothing when the pair has no overflow, which is the case for
// every Cypher-built graph.
//
// ForEachSlotRelTypeByID is safe for concurrent use.
func (g *Graph[N, W]) ForEachSlotRelTypeByID(srcID, dstID graph.NodeID, encoded uint32, visit func(name string)) {
	own, hasOwn := decodeSlotLabel(encoded)
	var extra []LabelID
	if g.edgeLabelOverflowActive.Load() != 0 {
		k := edgeKey{src: srcID, dst: dstID}
		sh := g.edgeLabelShardFor(k)
		sh.mu.RLock()
		for _, lid := range sh.overflow[k] {
			if hasOwn && lid == own {
				continue
			}
			extra = append(extra, lid)
		}
		sh.mu.RUnlock()
	}
	if hasOwn {
		if name, ok := g.reg.Resolve(own); ok {
			visit(name)
		}
	}
	for _, lid := range extra {
		if name, ok := g.reg.Resolve(lid); ok {
			visit(name)
		}
	}
}

// ForEachEdgeLabelByID streams the distinct labels of the directed edge (src,
// dst), invoking visit once per resolved label name without materialising the
// []string that [Graph.EdgeLabelsByID] returns. It is the allocation-fusing
// counterpart of EdgeLabelsByID — the edge-label analogue of
// [Graph.ForEachNodeLabelByID] — chiefly for the snapshot writer.
//
// The distinct label ids (inline slots + overflow, deduplicated) are gathered
// under the edge-label shard read lock; names are resolved and visited after the
// lock is released, exactly as EdgeLabelsByID does, so visit may safely read the
// graph. The dedup scratch is the same small per-call slice EdgeLabelsByID uses;
// the saving is the []string result slice the caller would otherwise range over.
func (g *Graph[N, W]) ForEachEdgeLabelByID(srcID, dstID graph.NodeID, visit func(name string)) {
	k := edgeKey{src: srcID, dst: dstID}
	sh := g.edgeLabelShardFor(k)
	sh.mu.RLock()
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
	for _, lid := range ids {
		if name, ok := g.reg.Resolve(lid); ok {
			visit(name)
		}
	}
}
