package exec

// query_counters.go — per-statement write-effect counters (rmp #2212).
//
// A client needs to know what a write actually did: whether a MERGE created or
// matched, how many properties a SET wrote, whether anything changed at all. The
// engine could not answer that. The graph maintains four counters for the openCypher
// TCK side-effect comparator ([lpg.Graph.SideEffectCounters]) but they are
// GRAPH-scoped running totals with a snapshot/reset protocol, not per-statement
// attribution, and they cover only nodes and relationships — not properties, labels or
// schema objects.
//
// # The counted set is openCypher's, not Bolt's
//
// openCypher defines EIGHT side effects, and the vendored TCK feature files use exactly
// that vocabulary in their `Then the side effects should be` tables: +nodes, -nodes,
// +relationships, -relationships, +properties, -properties, +labels, -labels. A
// property REMOVE is a distinct effect there — `REMOVE n.num` declares `-properties 1`
// (cypher/tck/features/clauses/remove/Remove1.feature) — so this type models the eight
// separately rather than pre-folding them.
//
// Bolt's counter set has no properties-removed: the protocol carries only
// properties-set. Folding `-properties` into it is therefore a mapping decision, and it
// belongs at the protocol boundary (the bolt package), not here. Modelling the spec and
// mapping at the edge keeps this type usable by the TCK comparator too, and keeps the
// lossy step visible in one documented place.
//
// Schema effects (indexes and constraints added/removed) are counted here because Bolt
// reports them, even though openCypher's side-effect vocabulary does not name them.

// QueryCounters holds the write effects one Cypher statement actually applied.
//
// # Semantics
//
// Every counter records an effect that was ACTUALLY APPLIED, incremented by the write
// adapter at the point it already discriminates a real change from a no-op:
//
//   - a re-intern of an existing node is not a creation, but re-creating a TOMBSTONED
//     key is (the adapter's existing rule);
//   - REMOVE of an absent property, and removal of an absent label, count nothing;
//   - a MERGE that matched counts nothing, because it never reaches a create.
//
// A statement that fails or is rolled back must report nothing: the counters live on
// the per-statement adapter, so an aborted statement's instance is simply discarded.
//
// # Concurrency
//
// QueryCounters is NOT safe for concurrent use, and deliberately holds plain integers
// rather than atomics: the Cypher write path is single-writer (the engine serialises
// writers), and each physical operator tree owns exactly one write adapter, so no
// synchronisation is required and adding any would cost the write path for nothing.
type QueryCounters struct {
	NodesCreated         int64
	NodesDeleted         int64
	RelationshipsCreated int64
	RelationshipsDeleted int64
	PropertiesSet        int64
	PropertiesRemoved    int64
	LabelsAdded          int64
	LabelsRemoved        int64
	IndexesAdded         int64
	IndexesRemoved       int64
	ConstraintsAdded     int64
	ConstraintsRemoved   int64
}

// ContainsUpdates reports whether the statement changed anything at all — the value
// Bolt's contains-updates carries and the driver surfaces as
// ResultSummary.Counters().ContainsUpdates().
//
// It is derived rather than tracked so it cannot disagree with the counters: any
// non-zero effect makes it true.
func (c *QueryCounters) ContainsUpdates() bool {
	if c == nil {
		return false
	}
	return c.NodesCreated != 0 || c.NodesDeleted != 0 ||
		c.RelationshipsCreated != 0 || c.RelationshipsDeleted != 0 ||
		c.PropertiesSet != 0 || c.PropertiesRemoved != 0 ||
		c.LabelsAdded != 0 || c.LabelsRemoved != 0 ||
		c.IndexesAdded != 0 || c.IndexesRemoved != 0 ||
		c.ConstraintsAdded != 0 || c.ConstraintsRemoved != 0
}

// Add folds other into c. It is used to accumulate an explicit transaction's effects
// across the statements it contains. A nil receiver or argument is a no-op.
func (c *QueryCounters) Add(other *QueryCounters) {
	if c == nil || other == nil {
		return
	}
	c.NodesCreated += other.NodesCreated
	c.NodesDeleted += other.NodesDeleted
	c.RelationshipsCreated += other.RelationshipsCreated
	c.RelationshipsDeleted += other.RelationshipsDeleted
	c.PropertiesSet += other.PropertiesSet
	c.PropertiesRemoved += other.PropertiesRemoved
	c.LabelsAdded += other.LabelsAdded
	c.LabelsRemoved += other.LabelsRemoved
	c.IndexesAdded += other.IndexesAdded
	c.IndexesRemoved += other.IndexesRemoved
	c.ConstraintsAdded += other.ConstraintsAdded
	c.ConstraintsRemoved += other.ConstraintsRemoved
}

// EffectCountingSuppressor is implemented by a write adapter whose write-effect
// counting can be paused for a span of internal teardown (#2212).
//
// It exists because deleting a node is implemented as: strip its incident edges, strip
// its labels, strip its properties, then tombstone it. Those strips go through the same
// RemoveNodeLabel / DelNodeProperty calls a user's REMOVE does, but they are NOT
// user-visible side effects — openCypher declares `DELETE n` and `DETACH DELETE n` as
// `-nodes 1` and nothing else (cypher/tck/features/clauses/delete/Delete1.feature
// scenarios [1] and [2]); a deleted node's labels and properties vanish WITH the node
// rather than being separately removed. Counting them would report effects the spec says
// did not happen.
//
// It is a separate optional interface rather than a GraphMutator method so the many test
// stubs implementing GraphMutator need no change; a mutator that does not implement it
// simply cannot suppress, which is safe because only the counting adapters do.
type EffectCountingSuppressor interface {
	// SuppressEffectCounting stops (true) or resumes (false) write-effect counting.
	// Calls do not nest: a paired true/false around one teardown span is the contract.
	SuppressEffectCounting(on bool)
}

// suppressEffectCounting pauses effect counting on m when m supports it, and returns the
// function that resumes it. When m does not support suppression the returned function is
// a no-op, so a caller can always defer it unconditionally.
func suppressEffectCounting(m GraphMutator) func() {
	s, ok := m.(EffectCountingSuppressor)
	if !ok {
		return func() {}
	}
	s.SuppressEffectCounting(true)
	return func() { s.SuppressEffectCounting(false) }
}
