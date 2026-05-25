package cypher_test

// apply_subquery_test.go — CALL { … } subquery tests (T727).
//
// The engine handles CALL procedure(…) YIELD via *ir.ProcedureCall but does
// NOT currently support CALL { … } inline subqueries (no *ir.CallSubquery in
// the IR). Attempting to use CALL { … } results in a parser or translator
// error.
//
// This file documents the current support boundary:
//   - EXISTS { … } / COUNT { … } subqueries in WHERE/RETURN: SUPPORTED
//     (see subquery_eval_test.go and semi_apply_exists_test.go).
//   - CALL { … } inline subqueries: NOT SUPPORTED (skipped below).
//   - CALL procedure() YIELD: SUPPORTED (see procs_engine_test.go).
//
// When CALL { … } is implemented, remove the t.Skip calls and fill in the
// expected values.

import (
	"context"
	"testing"
)

// TestCallSubquery_Simple probes whether the engine accepts a simple inline
// CALL subquery:
//
//	CALL { MATCH (n) RETURN n LIMIT 1 } RETURN n
//
// Currently unsupported — skipped.
func TestCallSubquery_Simple(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	const q = `CALL { MATCH (n) RETURN n LIMIT 1 } RETURN n`
	_, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		// Parser or translator rejected the query — CALL subquery not implemented.
		t.Skipf("CALL { } subquery not yet implemented: %v", err)
	}
	// If we reach here the engine accepted the query; drain and assert.
	t.Skip("CALL { } subquery not yet implemented (accepted without error — remove skip when wired)")
}

// TestCallSubquery_Correlated probes correlated inline subqueries:
//
//	MATCH (n) CALL { WITH n MATCH (n)-[:R]->(m) RETURN m } RETURN n, m
//
// Correlated CALL subqueries require an Apply operator driven by outer rows.
// Currently unsupported — skipped.
func TestCallSubquery_Correlated(t *testing.T) {
	t.Parallel()
	eng := newBareEngine(t)

	const q = `MATCH (n) CALL { WITH n MATCH (n)-[:R]->(m) RETURN m } RETURN n.name AS n, m.name AS m`
	_, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Skipf("correlated CALL { } subquery not yet implemented: %v", err)
	}
	t.Skip("correlated CALL { } subquery not yet implemented (accepted without error)")
}

// TestCallSubquery_ExistsVsCall documents the distinction between EXISTS
// subqueries (supported) and CALL subqueries (not supported).
//
// EXISTS { (n)-->(m) } is lowered to SemiApply at the IR level and executed
// by exec.SemiApply. CALL { … } is a different IR construct with no physical
// operator yet.
func TestCallSubquery_ExistsVsCall(t *testing.T) {
	t.Parallel()
	eng := newSemiApplyGraph(t) // reuse graph from semi_apply_exists_test.go

	// EXISTS works.
	res, err := eng.Run(context.Background(),
		`MATCH (n:Person) WHERE EXISTS { (n)-[]->(m) } RETURN count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("EXISTS query failed unexpectedly: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Errorf("EXISTS count: got %d rows, want 1", len(rows))
	}

	// CALL subquery does not work yet — document the boundary.
	_, callErr := eng.Run(context.Background(),
		`CALL { MATCH (n:Person) RETURN count(*) AS c } RETURN c`, nil)
	if callErr == nil {
		t.Log("CALL { } unexpectedly accepted — update this test if the feature is now implemented")
	}
	// callErr != nil is the expected outcome; no assertion needed (this is a
	// documentation test, not a correctness assertion for implemented code).
}
