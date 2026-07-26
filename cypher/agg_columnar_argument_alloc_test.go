//go:build !race

package cypher_test

// agg_columnar_argument_alloc_test.go — path-engagement gate for #2185.
//
// agg_columnar_argument_test.go proves the unboxed argument filler produces the SAME
// results as the boxed row path. That differential is only meaningful while the
// columnar path is actually taken: were a future change to make
// tryBuildColumnarAggInput decline the `min(n.v)` shape, every differential case
// would still pass — vacuously, with both arms on the row path.
//
// This gate closes that hole by measuring the property the two paths cannot share.
// The row path boxes the argument once per input row (an expr.Value interface per
// property read); the columnar path lands it in the chunk's typed column with no
// per-row box. So allocations per input row separate them decisively: ~1/row on the
// columnar path (the scan's own NodeID box) versus ~7/row on the row path, measured
// in docs/benchmarks/columnar-agg-argument-2026-07-26.md.
//
// The ceiling is set at 3 allocations per row — comfortably above the columnar
// path's ~1 and comfortably below the row path's ~7 — so the gate is immune to
// incidental churn but fails immediately if the argument column goes back to being
// filled through the boxed row evaluator.
//
// Gated !race because the race detector inflates heap accounting and would void the
// allocation counts.
//
// Layer: short.

import (
	"context"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// allocGateRows is large enough that the per-row cost dominates the fixed query
// cost, so the per-row figure is a clean signal.
const allocGateRows = 20_000

// maxAllocsPerRow is the ceiling described in the file comment: between the
// columnar path's ~1 and the row path's ~7.
const maxAllocsPerRow = 3.0

// seedAllocGateGraph builds an allocGateRows-node graph carrying int64 `v` (the
// aggregate argument) and int64 `g` (a 7-value grouping key).
func seedAllocGateGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < allocGateRows; i++ {
		k := "n" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty v: %v", err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Int64Value(int64(i%7))); err != nil {
			t.Fatalf("SetNodeProperty g: %v", err)
		}
	}
	return g
}

// allocsPerRow runs query once inside testing.AllocsPerRun and returns the mean
// allocation count divided by allocGateRows.
func allocsPerRow(t *testing.T, eng *cypher.Engine, query string) float64 {
	t.Helper()
	// Warm the plan cache and any pooled state so the measured run reflects the
	// steady-state per-row cost, not one-off setup.
	drainOnce(t, eng, query)
	allocs := testing.AllocsPerRun(3, func() { drainOnce(t, eng, query) })
	return allocs / float64(allocGateRows)
}

func drainOnce(t *testing.T, eng *cypher.Engine, query string) {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", query, err)
	}
}

// TestColumnarAggArgument_UnboxedPathEngaged asserts that every aggregate whose
// argument is a bare node property stays within the columnar allocation budget, and
// — as the control that makes the number meaningful — that the same aggregate with a
// coalesce-wrapped argument (which the builder declines) exceeds it.
func TestColumnarAggArgument_UnboxedPathEngaged(t *testing.T) {
	g := seedAllocGateGraph(t)
	// DisableParallelScan keeps the serial columnar EagerAggregation as the path
	// under test; the parallel aggregate scan is a different physical path.
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableParallelScan: true})

	for _, tc := range []struct {
		name       string
		columnar   string
		rowInstead string
	}{
		{
			"grouped min",
			"MATCH (n) RETURN n.g AS g, min(n.v) AS a",
			"MATCH (n) RETURN n.g AS g, min(coalesce(n.v, n.v)) AS a",
		},
		{
			"grouped sum",
			"MATCH (n) RETURN n.g AS g, sum(n.v) AS a",
			"MATCH (n) RETURN n.g AS g, sum(coalesce(n.v, n.v)) AS a",
		},
		{
			"global max",
			"MATCH (n) RETURN max(n.v) AS a",
			"MATCH (n) RETURN max(coalesce(n.v, n.v)) AS a",
		},
		{
			"global avg",
			"MATCH (n) RETURN avg(n.v) AS a",
			"MATCH (n) RETURN avg(coalesce(n.v, n.v)) AS a",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := allocsPerRow(t, eng, tc.columnar)
			ctrl := allocsPerRow(t, eng, tc.rowInstead)
			t.Logf("%s: columnar %.2f allocs/row, boxed control %.2f allocs/row", tc.name, got, ctrl)
			if got > maxAllocsPerRow {
				t.Fatalf("%q costs %.2f allocations per input row (ceiling %.1f): the aggregate "+
					"argument column is being filled through the boxed row evaluator, not "+
					"buildScalarPropertyFiller — see #2185", tc.columnar, got, maxAllocsPerRow)
			}
			if ctrl <= maxAllocsPerRow {
				t.Fatalf("control %q costs only %.2f allocations per input row, at or below the "+
					"%.1f ceiling: the control is no longer on the boxed row path, so this gate "+
					"can no longer distinguish the two paths and must be rewritten",
					tc.rowInstead, ctrl, maxAllocsPerRow)
			}
		})
	}
}
