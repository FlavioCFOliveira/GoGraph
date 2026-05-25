package cypher_test

// using_index_hint_test.go — T915: USING INDEX hint and automatic index
// selection with a hash index.
//
// The USING INDEX hint syntax is not implemented in the parser (no grammar
// rule for the USING clause). The tests in this file therefore:
//
//   1. Document the gap with a skipped sub-test.
//   2. Verify that the planner picks NodeByIndexSeek automatically when a hash
//      index is present, which is the observable contract users depend on
//      while explicit hints remain unimplemented.
//
// AC1: Hinted plan is explicitly skipped (hint not parsed) with a clear
//
//	message referencing the divergence log.
//
// AC2: Without a hint, the planner uses NodeByIndexSeek when a matching hash
//
//	index exists — verifying automatic index selection is live.
//
// AC3: race-clean (t.Parallel on all runnable sub-tests).
// AC4: goleak-clean (enforced by TestMain in testmain_test.go).

import (
	"strings"
	"testing"
)

// TestUsingIndexHint_NotImplemented documents that the USING INDEX hint syntax
// is not supported by the parser. The test is unconditionally skipped with an
// explanatory message so the gap is visible in the test log.
//
// When the hint is eventually implemented, remove the t.Skip call and replace
// this test body with an assertion on the plan output.
func TestUsingIndexHint_NotImplemented(t *testing.T) {
	t.Parallel()
	t.Skip("USING INDEX hint not implemented in the parser — see docs/tck/DIVERGENCES.md")
}

// TestUsingIndexHint_AutomaticSelectionWithHashIndex verifies that, in the
// absence of an explicit hint, the planner automatically selects
// NodeByIndexSeek when a hash index on the queried property exists.
//
// This is the observable contract that callers rely on. Once USING INDEX is
// implemented, this test remains valid: the planner should pick the index both
// with and without the hint.
func TestUsingIndexHint_AutomaticSelectionWithHashIndex(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(50, true /* withIndex */)

	plan, err := eng.Explain(`MATCH (n:Person {name: "Alice"}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("expected NodeByIndexSeek in plan with hash index present; got:\n%s", plan)
	}
}

// TestUsingIndexHint_NoIndexFallsBackToScan verifies the complementary case:
// without an index the planner does NOT use NodeByIndexSeek. This guards
// against false positives in TestUsingIndexHint_AutomaticSelectionWithHashIndex.
func TestUsingIndexHint_NoIndexFallsBackToScan(t *testing.T) {
	t.Parallel()

	_, eng := newPersonGraph(50, false /* withIndex */)

	plan, err := eng.Explain(`MATCH (n:Person {name: "Alice"}) RETURN n`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if strings.Contains(plan, "NodeByIndexSeek") {
		t.Errorf("unexpected NodeByIndexSeek without hash index; got:\n%s", plan)
	}
}
