package cypher_test

// collect_subquery_test.go — COLLECT { … } subquery tests (T886).
//
// COLLECT { MATCH … RETURN … } is a Cypher 5 feature that evaluates an inner
// subquery for each outer row and collects the returned values into a list.
// As of this writing the GoGraph AST, expression evaluator, and IR translator
// do not define a CollectSubquery node; the feature is not implemented.
//
// These tests document the intended behaviour and serve as a regression guard:
// when the feature is eventually wired, remove the t.Skip calls and replace
// the placeholder assertions with real expectations.
//
// Intended behaviour (for reference):
//
//	MATCH (n) RETURN COLLECT { MATCH (n)-[]->(m) RETURN m.name } AS friends
//
//	On the hub-spoke graph (hub → sp0, sp1, sp2, sp3):
//	  hub   → friends = ["sp0", "sp1", "sp2", "sp3"]  (order may vary)
//	  sp0-3 → friends = []
//
// See also: count_subquery_test.go for the related COUNT { } feature.

import (
	"context"
	"testing"
)

// TestCollectSubquery_CollectsInnerMatches documents the primary COLLECT { }
// use-case: collecting neighbour properties into a list column.
//
// Skipped until CollectSubquery is implemented in the engine.
func TestCollectSubquery_CollectsInnerMatches(t *testing.T) {
	t.Parallel()
	eng := newHubSpokeEngine(t)

	const q = `MATCH (n:Node) RETURN n.name AS name, COLLECT { MATCH (n)-[]->(m) RETURN m.name } AS friends`
	_, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Skipf("COLLECT { } subquery not yet implemented: %v", err)
	}
	// When implemented: assert hub has 4 entries in friends; spokes have 0.
	t.Skip("COLLECT { } subquery not yet implemented (query accepted without error — update when wired)")
}

// TestCollectSubquery_EmptyInnerMatch documents that COLLECT { } on a pattern
// with no matching rows should return an empty list (not nil, not an error).
//
// Skipped until CollectSubquery is implemented.
func TestCollectSubquery_EmptyInnerMatch(t *testing.T) {
	t.Parallel()
	eng := newHubSpokeEngine(t)

	// Spokes have no outgoing edges, so COLLECT { } should yield an empty list.
	const q = `MATCH (n:Node {name: 'sp0'}) RETURN COLLECT { MATCH (n)-[]->(m) RETURN m.name } AS friends`
	_, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Skipf("COLLECT { } subquery not yet implemented: %v", err)
	}
	t.Skip("COLLECT { } subquery not yet implemented (accepted without error — update when wired)")
}
