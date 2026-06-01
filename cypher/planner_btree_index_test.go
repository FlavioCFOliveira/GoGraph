package cypher_test

// planner_btree_index_test.go — T917: planner behaviour for range predicates
// when a btree index is installed on the queried property.
//
// Known limitation (documented here):
//
//	NodeByIndexRangeScan is defined in the IR (ir.NodeByIndexRangeScan) and
//	in the physical layer (exec.NodeByIndexRangeScan), and the cost model in
//	plan.ScanStrategy selects ScanKindIndexRangeScan when a btree index exists.
//	However, buildOperator in api.go does not yet contain a case for
//	*ir.NodeByIndexRangeScan, so the IR node is never emitted during query
//	compilation. Range predicates therefore fall through to
//	Selection + NodeByLabelScan at runtime.
//
//	EXPLAIN reflects this: a range query with a btree index present produces
//	"Selection" and "NodeByLabelScan" rather than "NodeByIndexRangeScan".
//	The tests below assert the actual current behaviour and will need updating
//	when NodeByIndexRangeScan is wired into buildOperator.
//
// AC1: Plan for a range query documents the current fallback
//
//	(Selection + NodeByLabelScan, NOT NodeByIndexRangeScan).
//
// AC2: race-clean (t.Parallel on all sub-tests).
// AC3: goleak-clean (enforced by TestMain in testmain_test.go).

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newCityEngineRaw creates a cypher.Engine for the given graph without
// installing any btree index. Used to exercise the no-index plan path.
func newCityEngineRaw(t *testing.T, g *lpg.Graph[string, float64]) *cypher.Engine {
	t.Helper()
	return cypher.NewEngine(g)
}

// TestPlannerBtreeIndex_RangeFallsBackToLabelScan documents that a range
// predicate on a btree-indexed property currently produces
// Selection + NodeByLabelScan in EXPLAIN, not NodeByIndexRangeScan.
//
// This test pins the current (unoptimised) behaviour. When buildOperator gains
// a *ir.NodeByIndexRangeScan case, change the assertion to expect
// "NodeByIndexRangeScan" instead of "NodeByLabelScan".
func TestPlannerBtreeIndex_RangeFallsBackToLabelScan(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)

	plan, err := eng.Explain(
		`MATCH (n:City) WHERE n.population >= 300000 AND n.population <= 700000 RETURN n.name`,
		nil,
	)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// NodeByIndexRangeScan is not wired in buildOperator yet — document the
	// current plan shape without asserting the optimised form.
	if strings.Contains(plan, "NodeByIndexRangeScan") {
		// If this fires, buildOperator was updated. Update assertions below.
		t.Logf("NOTE: NodeByIndexRangeScan is now present in plan — update this test to assert it positively:\n%s", plan)
		return
	}

	// Current fallback: range is evaluated via Selection over NodeByLabelScan.
	if !strings.Contains(plan, "NodeByLabelScan") {
		t.Errorf("expected NodeByLabelScan (fallback) for range query; got:\n%s", plan)
	}
}

// TestPlannerBtreeIndex_RangeReturnsCorrectRows verifies end-to-end
// correctness: even though the plan falls back to Selection + NodeByLabelScan,
// the query must still return only nodes whose population falls within the
// specified range.
func TestPlannerBtreeIndex_RangeReturnsCorrectRows(t *testing.T) {
	t.Parallel()

	_, eng := newCityEngine(t)

	res, err := eng.Run(context.Background(),
		`MATCH (n:City) WHERE n.population >= 400000 AND n.population <= 600000 RETURN n.name`,
		nil,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	rows := collectRecords(t, res)
	want := map[string]bool{"Delta": true, "Epsilon": true, "Zeta": true}
	if len(rows) != len(want) {
		t.Fatalf("want %d rows, got %d", len(want), len(rows))
	}
	for _, row := range rows {
		sv, ok := row["n.name"].(expr.StringValue)
		if !ok {
			t.Fatalf("n.name: expected StringValue, got %T", row["n.name"])
		}
		if !want[string(sv)] {
			t.Errorf("unexpected city in range result: %q", string(sv))
		}
	}
}

// TestPlannerBtreeIndex_NoIndexAllNodesScanOrLabelScan verifies that without
// any index a range query still falls back gracefully to AllNodesScan or
// NodeByLabelScan (not a plan error), and returns the correct result.
func TestPlannerBtreeIndex_NoIndexAllNodesScanOrLabelScan(t *testing.T) {
	t.Parallel()

	// Build city graph WITHOUT installing the btree index.
	g := buildCityGraph(t, testCities)
	eng := newCityEngineRaw(t, g)

	plan, err := eng.Explain(
		`MATCH (n:City) WHERE n.population >= 500000 RETURN n.name`,
		nil,
	)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	// Without a label the planner may choose AllNodesScan; with a label it
	// chooses NodeByLabelScan. Either is acceptable.
	if !strings.Contains(plan, "NodeByLabelScan") && !strings.Contains(plan, "AllNodesScan") {
		t.Errorf("expected NodeByLabelScan or AllNodesScan without index; got:\n%s", plan)
	}
}
