//go:build !race

package cypher_test

// row_eval_alloc_test.go — path-engagement gates for rmp #2890, #2891 and #2892.
//
// Each of the three changes removes a per-row allocation from a per-row
// expression-evaluation path, and each is result-identical by construction, so
// no result-level test can tell whether it is still engaged. Allocations per
// input row can: every gate below runs a query over allocGateRows nodes and
// holds its allocations per row under a ceiling set between the figure the
// change produces and the figure the path produced before it (both measured at
// a1c7bb9f and after the change; recorded beside each ceiling).
//
// Gated !race because the race detector inflates heap accounting and makes
// sync.Pool drop items at random, which would void the counts.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// allocsPerRowWith is allocsPerRow with parameters, run through run.
func allocsPerRowWith(t *testing.T, run func() (*cypher.Result, error)) float64 {
	t.Helper()
	drain := func() {
		res, err := run()
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		for res.Next() { // intentional full drain
		}
		if err := res.Err(); err != nil {
			t.Fatalf("Err: %v", err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	drain() // warm the plan cache and pooled state
	return testing.AllocsPerRun(3, drain) / float64(allocGateRows)
}

// TestAggregationPreProjection_RecyclesRowContext gates rmp #2890: a grouped
// aggregate whose argument is not a bare property is evaluated by the
// aggregation pre-projection, which used to allocate a fresh RowContext map per
// row (the map and its first group).
func TestAggregationPreProjection_RecyclesRowContext(t *testing.T) {
	g := seedAllocGateGraph(t)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableParallelScan: true})
	const q = "MATCH (n) RETURN n.g AS g, sum(coalesce(n.v, n.v)) AS a"
	got := allocsPerRowWith(t, func() (*cypher.Result, error) { return eng.Run(context.Background(), q, nil) })
	// Measured on this machine: a1c7bb9f 12.94/row; after the change 8.94/row.
	const ceiling = 11.0
	if got > ceiling {
		t.Errorf("%s: %.2f allocs/row, want <= %.1f (the per-row RowContext map is back)", q, got, ceiling)
	}
}

// TestStatementConstantCall_EvaluatedOnce gates rmp #2891: date($ref) cannot
// change within a statement, so it is resolved and evaluated once, not once
// per row.
func TestStatementConstantCall_EvaluatedOnce(t *testing.T) {
	g := seedAllocGateGraph(t)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableParallelScan: true})
	const q = "MATCH (n) RETURN count(date($ref)) AS c"
	params := map[string]expr.Value{"ref": expr.StringValue("2025-01-01")}
	got := allocsPerRowWith(t, func() (*cypher.Result, error) { return eng.Run(context.Background(), q, params) })
	// Measured on this machine: a1c7bb9f 4.99/row; after the change 0.99/row.
	const ceiling = 3.0
	if got > ceiling {
		t.Errorf("%s: %.2f allocs/row, want <= %.1f (date($ref) is evaluated per row again)", q, got, ceiling)
	}
}

// TestRowFilter_TypedPredicateUnboxed gates rmp #2892: a write statement's row
// Filter over `n.v = <int>` is decided by the typed row predicate, without the
// RowContext, the lazy node and the boxed operands of the boxed path.
func TestRowFilter_TypedPredicateUnboxed(t *testing.T) {
	g := seedAllocGateGraph(t)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableParallelScan: true})
	const q = "MATCH (n) WHERE n.v = 123456789 SET n.x = 1"
	got := allocsPerRowWith(t, func() (*cypher.Result, error) { return eng.RunInTx(context.Background(), q, nil) })
	// Measured on this machine: a1c7bb9f 3.97/row; after the change 1.98/row.
	const ceiling = 3.0
	if got > ceiling {
		t.Errorf("%s: %.2f allocs/row, want <= %.1f (the row Filter is boxed again)", q, got, ceiling)
	}
}
