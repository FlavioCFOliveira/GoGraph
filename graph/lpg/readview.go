package lpg

// readview.go — a graph bound to a read snapshot (rmp #2289, MVCC P4b).
//
// # Why a wrapper rather than threading a parameter
//
// The Cypher engine reaches the graph from about a hundred and twenty call
// sites, most of them inside closures the physical builder captures. Adding a
// snapshot argument to each is a hundred and twenty chances to pass the wrong
// one, or to miss one — and a missed one is not a compile error, it is a read
// that silently observes a later instant than the rest of its query.
//
// Binding the snapshot to the RECEIVER removes both failure modes. A call site
// reads `g.HasNodeLabel(n, "P")` before and after; only the TYPE of `g`
// changes. The compiler then does the work this design exists for: every read
// method the engine uses must appear here, so a read that has no versioned form
// FAILS TO BUILD instead of quietly reading the present.
//
// # nil is a valid snapshot and means "now"
//
// A view built with a nil snapshot reads the current stored value with no
// version walk, which is the only correct answer for a writer inside the
// visibility barrier: it applies eagerly and must see its own not-yet-published
// work. So the write path uses `g.ReadAt(nil)` and needs no second code path.
//
// # What is deliberately NOT here
//
// Anything that mutates. A ReadView cannot write, which is the second reason to
// have it: the read build cannot reach a mutator by accident. A caller holding
// one that genuinely needs the graph — the mapper, a registry, a config flag,
// or the write surface — asks for it explicitly with [ReadView.Raw], and that
// call is greppable.
//
// # What is here but not yet versioned
//
// The label bitmap index, the property indexes, the tombstone set, the node
// mapper and the count store answer WHICH objects a scan should consider, not
// what an object contains. They are not versioned, so the methods that expose
// them read the present. That is the candidate-set gap P4c (rmp #2290) closes;
// each is marked below so none is mistaken for a finished one.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/label"
)

// ReadView is a [Graph] bound to a [Snapshot]: the same read surface, with
// every method already resolved as of that instant.
//
// The zero value is not usable; obtain one from [Graph.ReadAt].
//
// Safe for concurrent use, on the same terms as the underlying graph: the view
// is immutable once created and the snapshot it holds is immutable too.
type ReadView[N comparable, W any] struct {
	g    *Graph[N, W]
	snap *Snapshot
}

// ReadAt binds g to snap, returning a view whose every read is resolved at that
// instant. A nil snapshot yields a view that reads the current stored value.
//
// It allocates: one small header per query, against a query that already
// allocates a physical operator tree. The alternative — a value type — would be
// copied at every call site that stores it in a struct.
func (g *Graph[N, W]) ReadAt(snap *Snapshot) *ReadView[N, W] {
	return &ReadView[N, W]{g: g, snap: snap}
}

// Raw returns the underlying graph, unbound.
//
// It is the escape hatch for a caller that needs the mapper, a registry, a
// configuration flag, or the write surface. It is named to be greppable: a
// review looking for reads that escaped the snapshot looks for this.
func (v *ReadView[N, W]) Raw() *Graph[N, W] { return v.g }

// Snapshot returns the instant this view reads at, or nil for a view that reads
// the current value.
func (v *ReadView[N, W]) Snapshot() *Snapshot { return v.snap }

// At returns a view of the same graph bound to a different snapshot.
func (v *ReadView[N, W]) At(snap *Snapshot) *ReadView[N, W] { return v.g.ReadAt(snap) }

// ── node labels ──────────────────────────────────────────────────────────────

// HasNodeLabel reports whether n carried the named label at this view's
// instant.
func (v *ReadView[N, W]) HasNodeLabel(n N, name string) bool {
	return v.g.HasNodeLabelAsOf(n, name, v.snap)
}

// HasNodeLabelByID is [ReadView.HasNodeLabel] keyed by NodeID.
func (v *ReadView[N, W]) HasNodeLabelByID(id graph.NodeID, name string) bool {
	return v.g.HasNodeLabelByIDAsOf(id, name, v.snap)
}

// NodeLabels returns n's label names at this view's instant, in unspecified
// order.
func (v *ReadView[N, W]) NodeLabels(n N) []string { return v.g.NodeLabelsAsOf(n, v.snap) }

// NodeLabelsByID is [ReadView.NodeLabels] keyed by NodeID.
func (v *ReadView[N, W]) NodeLabelsByID(id graph.NodeID) []string {
	return v.g.NodeLabelsByIDAsOf(id, v.snap)
}

// ForEachNodeLabelByID streams id's label names at this view's instant.
func (v *ReadView[N, W]) ForEachNodeLabelByID(id graph.NodeID, visit func(name string)) {
	v.g.ForEachNodeLabelByIDAsOf(id, v.snap, visit)
}

// ── node properties ──────────────────────────────────────────────────────────

// NodeProperties returns n's properties at this view's instant.
func (v *ReadView[N, W]) NodeProperties(n N) map[string]PropertyValue {
	return v.g.NodePropertiesAsOf(n, v.snap)
}

// GetNodeProperty returns the value n carried under key at this view's instant.
func (v *ReadView[N, W]) GetNodeProperty(n N, key string) (PropertyValue, bool) {
	return v.g.GetNodePropertyAsOf(n, key, v.snap)
}

// NodePropertyByID is [ReadView.GetNodeProperty] keyed by NodeID.
func (v *ReadView[N, W]) NodePropertyByID(id graph.NodeID, key string) (PropertyValue, bool) {
	return v.g.NodePropertyByIDAsOf(id, key, v.snap)
}

// NodePropertiesByIDFunc streams id's properties at this view's instant.
func (v *ReadView[N, W]) NodePropertiesByIDFunc(id graph.NodeID, visit func(name string, pv PropertyValue)) {
	v.g.NodePropertiesByIDFuncAsOf(id, v.snap, visit)
}

// ── edge types ───────────────────────────────────────────────────────────────

// EdgeLabels returns the pair's derived relationship-type union at this view's
// instant.
func (v *ReadView[N, W]) EdgeLabels(src, dst N) []string {
	return v.g.EdgeLabelsAsOf(src, dst, v.snap)
}

// EdgeLabelsByID is [ReadView.EdgeLabels] keyed by NodeIDs.
func (v *ReadView[N, W]) EdgeLabelsByID(srcID, dstID graph.NodeID) []string {
	return v.g.EdgeLabelsByIDAsOf(srcID, dstID, v.snap)
}

// ForEachEdgeLabelByID streams the pair's derived type union at this view's
// instant.
func (v *ReadView[N, W]) ForEachEdgeLabelByID(srcID, dstID graph.NodeID, visit func(name string)) {
	v.g.ForEachEdgeLabelByIDAsOf(srcID, dstID, v.snap, visit)
}

// ForEachSlotRelTypeByID streams ONE column-typed slot's relationship types at
// this view's instant.
func (v *ReadView[N, W]) ForEachSlotRelTypeByID(srcID, dstID graph.NodeID, encoded uint32, visit func(name string)) {
	v.g.ForEachSlotRelTypeByIDAsOf(srcID, dstID, encoded, v.snap, visit)
}

// HasEdgeLabel reports whether the pair carried the named type at this view's
// instant.
func (v *ReadView[N, W]) HasEdgeLabel(src, dst N, name string) bool {
	return v.g.HasEdgeLabelAsOf(src, dst, name, v.snap)
}

// EdgeLabelsAt returns the by-ordinal instance's types at this view's instant.
func (v *ReadView[N, W]) EdgeLabelsAt(src, dst N, idx int64) []string {
	return v.g.EdgeLabelsAtAsOf(src, dst, idx, v.snap)
}

// EdgeLabelsByHandle returns the by-handle instance's types at this view's
// instant.
func (v *ReadView[N, W]) EdgeLabelsByHandle(src, dst N, handle uint64) []string {
	return v.g.EdgeLabelsByHandleAsOf(src, dst, handle, v.snap)
}

// EdgeLabelsByHandleID is [ReadView.EdgeLabelsByHandle] keyed by NodeIDs.
func (v *ReadView[N, W]) EdgeLabelsByHandleID(srcID, dstID graph.NodeID, handle uint64) []string {
	return v.g.EdgeLabelsByHandleIDAsOf(srcID, dstID, handle, v.snap)
}

// HasEdgeHandleLabelRecordByID reports whether a handle-keyed type record
// existed for the slot at this view's instant, which is the precedence question
// that decides whether a slot is handle-typed or column-typed.
func (v *ReadView[N, W]) HasEdgeHandleLabelRecordByID(srcID, dstID graph.NodeID, handle uint64) bool {
	return v.g.HasEdgeHandleLabelRecordByIDAsOf(srcID, dstID, handle, v.snap)
}

// ── edge properties ──────────────────────────────────────────────────────────

// EdgeProperties returns the pair's coalesced properties at this view's
// instant.
func (v *ReadView[N, W]) EdgeProperties(src, dst N) map[string]PropertyValue {
	return v.g.EdgePropertiesAsOf(src, dst, v.snap)
}

// ForEachEdgeProperty streams the pair's coalesced properties at this view's
// instant.
func (v *ReadView[N, W]) ForEachEdgeProperty(src, dst N, visit func(name string, pv PropertyValue)) {
	v.g.ForEachEdgePropertyAsOf(src, dst, v.snap, visit)
}

// GetEdgeProperty returns the pair's coalesced value under key at this view's
// instant.
func (v *ReadView[N, W]) GetEdgeProperty(src, dst N, key string) (PropertyValue, bool) {
	return v.g.GetEdgePropertyAsOf(src, dst, key, v.snap)
}

// EdgeHasProperty reports storage presence of a non-null-mapping value at this
// view's instant.
func (v *ReadView[N, W]) EdgeHasProperty(src, dst N, key string) bool {
	return v.g.EdgeHasPropertyAsOf(src, dst, key, v.snap)
}

// EdgePropertiesAt returns the by-ordinal instance's properties at this view's
// instant.
func (v *ReadView[N, W]) EdgePropertiesAt(src, dst N, idx int64) map[string]PropertyValue {
	return v.g.EdgePropertiesAtAsOf(src, dst, idx, v.snap)
}

// EdgePropertiesByHandle returns the by-handle instance's properties at this
// view's instant.
func (v *ReadView[N, W]) EdgePropertiesByHandle(src, dst N, handle uint64) map[string]PropertyValue {
	return v.g.EdgePropertiesByHandleAsOf(src, dst, handle, v.snap)
}

// EdgePropertiesByHandleID is [ReadView.EdgePropertiesByHandle] keyed by
// NodeIDs.
func (v *ReadView[N, W]) EdgePropertiesByHandleID(srcID, dstID graph.NodeID, handle uint64) map[string]PropertyValue {
	return v.g.EdgePropertiesByHandleIDAsOf(srcID, dstID, handle, v.snap)
}

// ── topology ─────────────────────────────────────────────────────────────────

// EntryView returns every column of id's adjacency entry at this view's
// instant, resolved from ONE entry so the columns are mutually consistent.
func (v *ReadView[N, W]) EntryView(id graph.NodeID) adjlist.EntryView[W] {
	return v.g.EntryViewAsOf(id, v.snap)
}

// HasEdge reports whether a directed edge existed at this view's instant.
func (v *ReadView[N, W]) HasEdge(src, dst N) bool { return v.g.HasEdgeAsOf(src, dst, v.snap) }

// HasEdgeByID is [ReadView.HasEdge] keyed by NodeIDs.
func (v *ReadView[N, W]) HasEdgeByID(srcID, dstID graph.NodeID) bool {
	return v.g.HasEdgeByIDAsOf(srcID, dstID, v.snap)
}

// EdgeWeight returns the first matching edge's weight at this view's instant.
func (v *ReadView[N, W]) EdgeWeight(src, dst N) (W, bool) {
	return v.g.EdgeWeightAsOf(src, dst, v.snap)
}

// FirstEdgeHandle returns the first matching slot's stable handle at this
// view's instant.
func (v *ReadView[N, W]) FirstEdgeHandle(src, dst N) (uint64, bool) {
	return v.g.FirstEdgeHandleAsOf(src, dst, v.snap)
}

// OutDegreeBoundedByID counts id's live out-edges at this view's instant,
// capped at limit.
func (v *ReadView[N, W]) OutDegreeBoundedByID(id graph.NodeID, limit int) (int, bool) {
	return v.g.OutDegreeBoundedByIDAsOf(id, limit, v.snap)
}

// OutDegreeByTypeBoundedByID counts id's live typed out-edges at this view's
// instant, capped at limit.
func (v *ReadView[N, W]) OutDegreeByTypeBoundedByID(id graph.NodeID, relType LabelID, limit int) (int, bool) {
	return v.g.OutDegreeByTypeBoundedByIDAsOf(id, relType, limit, v.snap)
}

// OutDegreeMatchingBoundedByID counts id's live out-edges whose far endpoint
// satisfies farOK at this view's instant, capped at limit.
func (v *ReadView[N, W]) OutDegreeMatchingBoundedByID(
	id graph.NodeID, relType LabelID, typed bool, limit int, farOK func(graph.NodeID) bool,
) (int, bool) {
	return v.g.OutDegreeMatchingBoundedByIDAsOf(id, relType, typed, limit, farOK, v.snap)
}

// ── candidate structures: NOT yet versioned (rmp #2290, MVCC P4c) ────────────
//
// Each of these answers which objects a scan should consider, not what an
// object contains, and each reads the PRESENT. They are listed together, and
// separately from everything above, so the gap is a visible property of the
// type rather than something a reader has to reconstruct.

// AdjList returns the adjacency backend, for the mapper and the configuration
// flags. Topology reads go through the versioned methods above.
func (v *ReadView[N, W]) AdjList() *adjlist.AdjList[N, W] { return v.g.AdjList() }

// IsTombstoned reports whether id is CURRENTLY deleted. Not versioned: a node
// deleted after this view's instant already reads as absent.
func (v *ReadView[N, W]) IsTombstoned(id graph.NodeID) bool { return v.g.IsTombstoned(id) }

// LiveNodeFilter returns the CURRENT liveness predicate. Not versioned; see
// [ReadView.IsTombstoned].
func (v *ReadView[N, W]) LiveNodeFilter() func(graph.NodeID) bool { return v.g.LiveNodeFilter() }

// LiveOrder returns the CURRENT live node count. Not versioned.
func (v *ReadView[N, W]) LiveOrder() uint64 { return v.g.LiveOrder() }

// EdgeCreateCount returns the CURRENT per-pair CREATE multiplicity. Not
// versioned.
func (v *ReadView[N, W]) EdgeCreateCount(src, dst N) int64 { return v.g.EdgeCreateCount(src, dst) }

// NodeIndex returns the label bitmap index, which is a candidate source read at
// the PRESENT and re-checked against the versioned label bags above.
func (v *ReadView[N, W]) NodeIndex() *label.Index { return v.g.NodeIndex() }

// IndexManager returns the secondary-index manager, a candidate source read at
// the PRESENT.
func (v *ReadView[N, W]) IndexManager() *index.Manager { return v.g.IndexManager() }

// ── immutable metadata ───────────────────────────────────────────────────────
//
// Registries and counters that are not per-object state and have no version.

// Registry returns the label registry.
func (v *ReadView[N, W]) Registry() *LabelRegistry { return v.g.Registry() }

// PropertyKeys returns the property-key registry.
func (v *ReadView[N, W]) PropertyKeys() *PropertyKeyRegistry { return v.g.PropertyKeys() }

// HasConstraints reports whether any schema constraint is registered.
func (v *ReadView[N, W]) HasConstraints() bool { return v.g.HasConstraints() }

// StoreConstraints returns the store-direct constraint declarations.
func (v *ReadView[N, W]) StoreConstraints() []StoreConstraint { return v.g.StoreConstraints() }

// TopoGeneration returns the topology epoch, which keys the derived caches.
func (v *ReadView[N, W]) TopoGeneration() uint64 { return v.g.TopoGeneration() }
