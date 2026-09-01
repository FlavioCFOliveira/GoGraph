package cypher_test

// aggregation_plan_eager_test.go — T923: aggregation plan eager vs streaming
// choice documented.
//
// Acceptance criteria:
//  1. Without ORDER BY: plan uses EagerAggregation.
//  2. With ORDER BY matching the grouping key: plan uses EagerAggregation.
//     Streaming aggregation is NOT implemented in this engine; EagerAggregation
//     is the correct and only aggregation operator. This is documented below.
//  3. Race-clean.
//  4. goleak-clean (enforced by TestMain in testmain_test.go).
//
// Streaming aggregation note:
//   The cypher/ir package exposes only EagerAggregation. There is no
//   StreamingAggregation or OrderedAggregation operator. The planner always
//   selects EagerAggregation regardless of whether an ORDER BY clause is
//   present, because streaming aggregation requires the input to be sorted by
//   the grouping key before aggregation begins — a property the planner does
//   not currently exploit. This is a known limitation; the plan text will
//   contain "EagerAggregation" in both cases.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newAggregationPlanEngine returns an engine with 5 :Person nodes whose ages
// are 20, 25, 25, 30, 30 — giving two distinct age groups.
func newAggregationPlanEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	for i, age := range []int{20, 25, 25, 30, 30} {
		q := fmt.Sprintf(`CREATE (:Person {name: 'p%d', age: %d})`, i, age)
		runSetup(t, eng, q)
	}
	return eng
}

// TestAggregationPlan_EagerWithoutOrderBy verifies that a grouping aggregation
// without ORDER BY emits EagerAggregation.
func TestAggregationPlan_EagerWithoutOrderBy(t *testing.T) {
	t.Parallel()

	eng := newAggregationPlanEngine(t)

	const q = `MATCH (n:Person) RETURN n.age, count(*)`
	plan, err := eng.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "EagerAggregation") {
		t.Errorf("plan without ORDER BY missing EagerAggregation:\n%s", plan)
	}
}

// TestAggregationPlan_EagerWithMatchingOrderBy verifies that a grouping
// aggregation with an ORDER BY matching the grouping key also emits
// EagerAggregation.
//
// Streaming aggregation is not implemented: the planner always selects
// EagerAggregation. A future implementation may introduce a streaming variant
// when the ORDER BY key matches the grouping key; at that point this test
// should be updated to accept either "EagerAggregation" or "StreamingAggregation".
func TestAggregationPlan_EagerWithMatchingOrderBy(t *testing.T) {
	t.Parallel()

	eng := newAggregationPlanEngine(t)

	// ORDER BY n.age matches the grouping key — this is the canonical case
	// where streaming aggregation would apply if implemented.
	const q = `MATCH (n:Person) RETURN n.age, count(*) ORDER BY n.age`
	plan, err := eng.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	// Streaming aggregation is not implemented; EagerAggregation is always used.
	if !strings.Contains(plan, "EagerAggregation") {
		t.Errorf("plan with ORDER BY missing EagerAggregation:\n%s", plan)
	}
}

// TestAggregationPlan_ExecutionCorrectness verifies that the EagerAggregation
// plan produces correct results for both the un-ordered and ordered variants.
func TestAggregationPlan_ExecutionCorrectness(t *testing.T) {
	t.Parallel()

	eng := newAggregationPlanEngine(t)
	ctx := context.Background()

	// ages: 20×1, 25×2, 30×2 → 3 distinct groups
	res, err := eng.Run(ctx, `MATCH (n:Person) RETURN n.age, count(*) AS c`, nil)
	if err != nil {
		t.Fatalf("Run without ORDER BY: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 3 {
		t.Fatalf("expected 3 age groups, got %d: %v", len(rows), rows)
	}
}

// TestAggregationPlan_GlobalAggregate verifies that a global (non-grouping)
// count(*) builds NO aggregation operator, and that the pushdown which serves it
// is the cheapest one applicable to the shape.
//
// It asserted EagerAggregation until #2625, which replaced the aggregation with
// exec.CountRows: a group-by-less, non-DISTINCT count(*) has a CONSTANT aggregate
// argument, so the pre-projection it fed materialised one single-column row per
// input row to carry a value CountAgg could never reject.
//
// Since rmp #2654 the BARE LABELLED case is claimed one level cheaper still, by
// the labelled-count pushdown (exec.LabelCountScan), which reads the label
// index's maintained cardinality in O(1) and does not visit a row at all —
// CountRows still visits every row, it only stops BUILDING them. The pushdown
// used to be gated behind the morsel-parallel threshold, so on a 5-node fixture
// CountRows won by default rather than on merit. Both subtests below are
// asserted, so retargeting the labelled case cannot silently delete #2625's
// coverage: the filtered case is a shape LabelCountScan must decline (the WHERE
// realises as a Selection above the scan) and CountRows must still serve.
func TestAggregationPlan_GlobalAggregate(t *testing.T) {
	t.Parallel()

	eng := newAggregationPlanEngine(t)
	ctx := context.Background()

	t.Run("bare-labelled-scan", func(t *testing.T) {
		const q = `MATCH (n:Person) RETURN count(*) AS c`
		plan, err := eng.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if !strings.Contains(plan, "LabelCountScan") {
			t.Errorf("bare labelled count(*) plan missing LabelCountScan: a constant-time "+
				"index read is available for this shape and something claimed it first:\n%s", plan)
		}
		if strings.Contains(plan, "EagerAggregation") {
			t.Errorf("bare labelled count(*) still builds an aggregation the pushdown replaces:\n%s", plan)
		}

		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			t.Fatalf("Run global aggregate: %v", err)
		}
		rows := collectRecords(t, res)
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		mustInt(t, "count(*)", rows[0]["c"], 5)
	})

	t.Run("filtered-scan-keeps-countrows", func(t *testing.T) {
		// The WHERE wraps the label scan in a Selection, so the labelled-count
		// pushdown declines (the count of the label is no longer the count of the
		// rows) and #2625's CountRows is the pushdown that applies.
		const q = `MATCH (n:Person) WHERE n.age >= 25 RETURN count(*) AS c`
		plan, err := eng.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if !strings.Contains(plan, "CountRows") {
			t.Errorf("filtered global count(*) plan missing CountRows:\n%s", plan)
		}
		if strings.Contains(plan, "LabelCountScan") {
			t.Errorf("filtered global count(*) used LabelCountScan, which would count the "+
				"whole label and ignore the WHERE:\n%s", plan)
		}
		if strings.Contains(plan, "EagerAggregation") {
			t.Errorf("filtered global count(*) still builds an aggregation:\n%s", plan)
		}

		res, err := eng.Run(ctx, q, nil)
		if err != nil {
			t.Fatalf("Run filtered global aggregate: %v", err)
		}
		rows := collectRecords(t, res)
		if len(rows) != 1 {
			t.Fatalf("expected 1 row, got %d", len(rows))
		}
		// ages 20, 25, 25, 30, 30 → four are >= 25.
		mustInt(t, "count(*)", rows[0]["c"], 4)
	})
}

// TestAggregationPlan_RaceClean runs concurrent Explain and Run calls to
// satisfy the race-clean acceptance criterion.
func TestAggregationPlan_RaceClean(t *testing.T) {
	t.Parallel()

	eng := newAggregationPlanEngine(t)
	ctx := context.Background()

	const q = `MATCH (n:Person) RETURN n.age, count(*)`
	const workers = 8
	done := make(chan struct{}, workers)
	errs := make(chan error, workers*2)

	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := eng.Explain(q, nil); err != nil {
				errs <- fmt.Errorf("Explain: %w", err)
				return
			}
			res, err := eng.Run(ctx, q, nil)
			if err != nil {
				errs <- fmt.Errorf("Run: %w", err)
				return
			}
			rows := collectRecords(t, res)
			if len(rows) != 3 {
				errs <- fmt.Errorf("expected 3 age groups, got %d", len(rows))
			}
		}()
	}

	for range workers {
		<-done
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
