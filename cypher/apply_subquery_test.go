package cypher_test

// apply_subquery_test.go — the CALL { … } support boundary (T727).
//
// The engine handles CALL procedure(…) YIELD via *ir.ProcedureCall but does
// NOT support CALL { … } inline subqueries: there is no *ir.CallSubquery in
// the IR, and the parser rejects the construct outright.
//
// This file records the current support boundary with an assertion that can
// actually fail:
//   - EXISTS { … } / COUNT { … } subqueries in WHERE/RETURN: SUPPORTED
//     (see subquery_eval_test.go and semi_apply_exists_test.go).
//   - CALL procedure() YIELD: SUPPORTED (see procs_engine_test.go).
//   - CALL { … } inline subqueries: NOT SUPPORTED.
//
// Placeholder tests for the unsupported constructs used to live here and in
// collect_subquery_test.go, call_in_transactions_test.go and
// call_in_transactions_rollback_test.go. Every one of them ended in an
// unconditional t.Skip after a body that never asserted, so none of them could
// fail whatever the engine did — they reported coverage that did not exist.
// They were deleted under rmp #2709; the missing features (CALL { }, correlated
// CALL { }, COLLECT { }, CALL { } IN TRANSACTIONS OF n ROWS and its per-batch
// rollback contract) are backlog items, not green skips.

import (
	"context"
	"testing"
)

// TestCallSubquery_ExistsVsCall asserts the supported half of the boundary:
// EXISTS { … } is lowered to SemiApply at the IR level and executed by
// exec.SemiApply, so the query must run and return exactly one aggregate row.
//
// The CALL { … } half is probed but deliberately not asserted: pinning
// "CALL { } must error" would turn implementing the feature into a test
// failure. The probe is logged so the boundary is visible in a -v run.
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

	// CALL subquery is not wired yet — probe and log, do not pin.
	if _, callErr := eng.Run(context.Background(),
		`CALL { MATCH (n:Person) RETURN count(*) AS c } RETURN c`, nil); callErr == nil {
		t.Log("CALL { } accepted — the feature may now be implemented; see rmp #2709 for the deleted placeholders")
	} else {
		t.Logf("CALL { } still unsupported: %v", callErr)
	}
}
