package lpg

// edge_property.go — the public per-pair edge-property surface, backed by the
// columnar tier (sprint 222, design D1 in docs/columnar-edge-properties-design.md).
//
// The map[edgeKey]propBag store that previously held one boxed property bag per
// (src,dst) pair is retired. Edge properties now live in an opaque, immutable,
// per-source-node columnar block ([edgePropCols]) carried inside the adjacency
// entry as its [adjlist.AuxColumn], with one de-boxed typed column per
// (propertyKeyID, kind). See edge_property_column.go for the block.
//
// # Per-pair contract preserved exactly
//
// The public surface is unchanged: [Graph.EdgeProperties] returns one coalesced,
// latest-wins map per (src,dst), folding any parallel edges; [Graph.GetEdgeProperty],
// [Graph.SetEdgeProperty], and [Graph.DelEdgeProperty] keep their semantics. The
// reconciliation between the per-slot columns and the per-pair surface is:
//
//   - WRITE: SetEdgeProperty writes the value to EVERY adjacency slot of src
//     whose neighbour is dst (all parallel edges to dst get the identical value).
//   - READ: EdgeProperties / GetEdgeProperty COALESCE across every dst-matching
//     slot, latest slot winning per key. Because the write fans out to all
//     dst-matching slots, the live slots carry the identical set; a slot that was
//     appended after the last write (and so is still absent) simply contributes
//     nothing, which is exactly what the per-pair coalesce expects. This makes
//     the derived per-pair view byte-identical to the old single-bag-per-pair map.
//   - DELETE: DelEdgeProperty clears the key on EVERY dst-matching slot.
//
// Because SetEdgeProperty is gated on the edge existing (HasEdge), a property
// only ever lives on a live adjacency slot, and the per-pair state is dropped
// when the last edge between the pair is removed (clearEdgePairState). There is
// therefore no orphan tier for properties (unlike relationship labels, whose
// RemoveEdgeLabel can be called on an absent edge).

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// SetEdgeProperty records the named property on the directed edge
// (src, dst). The edge must already exist; otherwise the call is a
// no-op (mirroring SetEdgeLabel). Returns any error returned by the
// installed [SchemaValidator]; when the validator rejects the write the
// graph state is left unchanged.
//
// The value is written into the per-slot columnar block of src at every slot
// whose neighbour is dst, so the per-pair view coalesces to the latest value
// for the key. The write is copy-on-write under the adjacency shard lock: a new
// immutable column block is built with every dst-matching slot updated and is
// published with a single atomic store, so a concurrent lock-free reader
// observes either the prior block or the fully-updated one.
func (g *Graph[N, W]) SetEdgeProperty(src, dst N, key string, value PropertyValue) error {
	if v := g.validator.load(); v != nil {
		if err := v.Validate(key, value); err != nil {
			return err
		}
	}
	if !g.adj.HasEdge(src, dst) {
		return nil
	}
	srcID, _ := g.adj.Mapper().Lookup(src)
	dstID, _ := g.adj.Mapper().Lookup(dst)
	keyID := g.pkeys.Intern(key)
	g.adj.UpdateEntryAux(srcID, func(cur adjlist.AuxColumn, neighbours []graph.NodeID) (adjlist.AuxColumn, bool) {
		block := asEdgePropCols(cur)
		length := len(neighbours)
		changed := false
		for i, nb := range neighbours {
			if nb != dstID {
				continue
			}
			block = block.set(keyID, i, length, value)
			changed = true
		}
		if !changed {
			return cur, false
		}
		return block, true
	})
	return nil
}

// GetEdgeProperty returns the property value attached to the
// directed edge (src, dst) under key. When several parallel edges connect the
// pair the latest-winning value across their slots is returned (the slots carry
// the identical value by the SetEdgeProperty fan-out, so this is well-defined).
func (g *Graph[N, W]) GetEdgeProperty(src, dst N, key string) (PropertyValue, bool) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return PropertyValue{}, false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return PropertyValue{}, false
	}
	keyID, ok := g.pkeys.Lookup(key)
	if !ok {
		return PropertyValue{}, false
	}
	block := asEdgePropCols(g.adj.LoadEntryAux(srcID))
	if block == nil {
		return PropertyValue{}, false
	}
	nbs, _ := g.adj.LoadEntry(srcID)
	// Bound the scan by the shorter of the two snapshots: a concurrent writer may
	// publish a longer neighbours snapshot after the block was loaded.
	n := minInt(len(nbs), block.lenOrZero())
	var out PropertyValue
	found := false
	for i := 0; i < n; i++ {
		if nbs[i] != dstID {
			continue
		}
		if v, present := block.get(keyID, i); present {
			out, found = v, true // latest dst-matching slot wins
		}
	}
	return out, found
}

// EdgeHasProperty reports whether the directed edge (src, dst) carries a value
// under key that would materialise to a NON-NULL Cypher value — without
// building that value. It is the storage-presence fast path behind a bound
// relationship's `r.key IS NOT NULL` / `IS NULL` predicate: the caller needs
// only the boolean presence, so fetching and boxing the value (as
// [Graph.GetEdgeProperty] / [Graph.EdgeProperties] would) is pure waste.
//
// Congruence with the value path is BY CONSTRUCTION, on two axes:
//
//   - Per-pair coalescing — it folds parallel edges exactly as
//     [Graph.EdgeProperties] does: the LATEST dst-matching adjacency slot that
//     carries key wins. The returned answer therefore reflects the same single
//     coalesced value the Cypher evaluator would observe via EdgeProperties,
//     never an earlier shadowed write.
//   - Kind gating — the winning slot's storage kind is tested with
//     [kindMapsToNonNullCypher], which mirrors cypher.lpgPropToExpr's
//     nullability table. A present-but-null-mapping property (a stored PropTime
//     or PropBytes) reads as Null through Cypher, so this reports false for it,
//     exactly as `r.key IS NOT NULL` would evaluate to false.
//
// The scan reads only validity bits and per-column kind tags (no value cell), so
// it allocates nothing. Returns false when either endpoint or key is unknown, or
// when no dst-matching slot carries a non-null-mapping value for key.
//
// Concurrency-safe under the same lock-free contract as [Graph.GetEdgeProperty]:
// it reads an immutable published columnar block and bounds its scan by the
// shorter of the block and the neighbours snapshot, so a concurrent copy-on-write
// writer is observed atomically (old block or new, never half-built).
func (g *Graph[N, W]) EdgeHasProperty(src, dst N, key string) bool {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return false
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return false
	}
	keyID, ok := g.pkeys.Lookup(key)
	if !ok {
		return false
	}
	block := asEdgePropCols(g.adj.LoadEntryAux(srcID))
	if block == nil {
		return false
	}
	nbs, _ := g.adj.LoadEntry(srcID)
	n := minInt(len(nbs), block.lenOrZero())
	found := false
	var winning PropertyKind
	for i := 0; i < n; i++ {
		if nbs[i] != dstID {
			continue
		}
		if k, present := block.slotKind(keyID, i); present {
			winning, found = k, true // latest dst-matching slot wins
		}
	}
	if !found {
		return false
	}
	return kindMapsToNonNullCypher(winning)
}

// DelEdgeProperty removes the named property from the directed edge
// (src, dst). No-op if absent. The key is cleared on every dst-matching slot so
// the per-pair view no longer reports it.
func (g *Graph[N, W]) DelEdgeProperty(src, dst N, key string) {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return
	}
	keyID, ok := g.pkeys.Lookup(key)
	if !ok {
		return
	}
	g.adj.UpdateEntryAux(srcID, func(cur adjlist.AuxColumn, neighbours []graph.NodeID) (adjlist.AuxColumn, bool) {
		block := asEdgePropCols(cur)
		if block == nil {
			return cur, false
		}
		changed := false
		for i, nb := range neighbours {
			if nb != dstID {
				continue
			}
			next, did := block.del(keyID, i)
			if did {
				block = next
				changed = true
			}
		}
		if !changed {
			return cur, false
		}
		return block, true
	})
}

// EdgeProperties returns a snapshot of every property currently
// attached to the directed edge (src, dst). When several parallel edges connect
// the pair the result is the latest-wins coalesced union across their slots.
func (g *Graph[N, W]) EdgeProperties(src, dst N) map[string]PropertyValue {
	srcID, ok := g.adj.Mapper().Lookup(src)
	if !ok {
		return nil
	}
	dstID, ok := g.adj.Mapper().Lookup(dst)
	if !ok {
		return nil
	}
	return g.EdgePropertiesByID(srcID, dstID)
}

// EdgePropertiesByID is the NodeID-keyed counterpart of [Graph.EdgeProperties]:
// it returns the latest-wins coalesced property map of the directed edge
// identified by the endpoint NodeIDs (srcID, dstID), or nil when the pair
// carries no properties. It is the edge dual of [Graph.NodePropertiesByID].
//
// Unlike [Graph.EdgeProperties] it performs NO Mapper access — no external-key →
// NodeID lookup — so a caller that already holds both endpoint NodeIDs can
// resolve edge properties without re-entering the Mapper. This is precisely what
// the snapshot collectors require: they enumerate endpoints from inside
// [graph.Mapper.Walk], which holds a Mapper shard read lock across its callback,
// and the Mapper contract forbids re-entry there while a writer may be running
// (graph/mapper.go:337-345, #1648). The read is served from the lock-free
// immutable adjacency entry, so EdgePropertiesByID is safe for concurrent use.
func (g *Graph[N, W]) EdgePropertiesByID(srcID, dstID graph.NodeID) map[string]PropertyValue {
	block := asEdgePropCols(g.adj.LoadEntryAux(srcID))
	if block == nil {
		return nil
	}
	nbs, _ := g.adj.LoadEntry(srcID)
	n := minInt(len(nbs), block.lenOrZero())
	var out map[string]PropertyValue
	for i := 0; i < n; i++ {
		if nbs[i] != dstID {
			continue
		}
		block.forEachAt(i, func(kk PropertyKeyID, v PropertyValue) {
			name, ok := g.pkeys.Resolve(kk)
			if !ok {
				return
			}
			if out == nil {
				out = make(map[string]PropertyValue, 2)
			}
			out[name] = v // latest dst-matching slot wins
		})
	}
	return out
}

// asEdgePropCols narrows the opaque [adjlist.AuxColumn] to the concrete
// [edgePropCols] this package stores there, returning nil when the column is
// absent. The aux column on an LPG adjacency entry is always an *edgePropCols
// (this package is the only writer), so the type assertion never fails for a
// non-nil column; a failed assertion yields nil and is treated as "no
// properties", which is safe.
func asEdgePropCols(c adjlist.AuxColumn) *edgePropCols {
	if c == nil {
		return nil
	}
	b, _ := c.(*edgePropCols)
	return b
}

// minInt returns the smaller of two ints.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
