package cypher_ldbc_test

// min_label_scan_bench_test.go — benchmark demonstrating the min-cardinality
// multi-label anchor scan (#2077) win: a multi-label node pattern anchored on
// the FIRST syntactic label (a full NodeByLabelScan of the large label +
// residual LabelPredicate Filter, O(|first|)) is re-anchored onto the
// smallest-cardinality label (O(|smallest|)).
//
// The query `MATCH (n:Common:Rare) RETURN n.k` matches the nodes carrying BOTH
// labels. :Common is written first and is the large label; :Rare is a small
// subset of it. With the peephole DISABLED the engine scans every :Common node
// and refilters on :Rare (|Common| candidate rows); with it ENABLED the scan is
// re-anchored on the smaller :Rare label (|Rare| candidate rows) with :Common
// re-checked as the residual filter. Both plans emit the identical row multiset
// (the label conjunction is commutative — proven by the differential test in
// cypher/min_label_scan_diff_test.go); the delta is the pure scan-cardinality
// win. The two benchmarks below measure the same query on the same graph with
// the optimisation on and off, so the delta is attributable to it alone.
//
// Run:
//
//	go test -run='^$' -bench=BenchmarkMinLabelScan -benchmem -count=10 ./bench/cypher_ldbc/...

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mlsCommonPop is the :Common population — the large label the pattern lists
// first. Large enough that scanning all of it dominates a re-anchored scan of
// the small label, while staying quick to build and run.
const mlsCommonPop = 100_000

// mlsRarePop is the :Rare population — a strict subset of :Common (every :Rare
// node is also :Common). 1% of :Common: a clear 100× cardinality skew, and well
// above a trivial size so the small-label scan is a realistic amount of work,
// not a corner case.
const mlsRarePop = 1_000

// minLabelScanBenchQuery lists the large label FIRST, so the default IR anchors
// the scan on :Common and re-checks :Rare — exactly the shape the peephole
// re-anchors onto :Rare.
const minLabelScanBenchQuery = "MATCH (n:Common:Rare) RETURN n.k AS k"

// buildMinLabelScanBenchGraph seeds mlsCommonPop nodes all carrying :Common, of
// which the first mlsRarePop also carry :Rare. Every node has a unique integer
// key "k" so the projection does real per-row work under both plans.
func buildMinLabelScanBenchGraph() *lpg.Graph[string, float64] {
	// Multigraph:true is behaviour-neutral here — the fixture creates no edges,
	// let alone parallel ones — and silences the engine's non-multigraph
	// construction warning so the benchmark output stays clean.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < mlsCommonPop; i++ {
		k := fmt.Sprintf("n%d", i)
		_ = g.AddNode(k)
		_ = g.SetNodeLabel(k, "Common")
		if i < mlsRarePop {
			_ = g.SetNodeLabel(k, "Rare")
		}
		_ = g.SetNodeProperty(k, "k", lpg.Int64Value(int64(i)))
	}
	g.SetIndexManager(index.NewManager())
	return g
}

// benchMinLabelScan runs the multi-label query with the min-label anchor scan
// either enabled or disabled, on a freshly-seeded graph.
func benchMinLabelScan(b *testing.B, enabled bool) {
	b.Helper()
	g := buildMinLabelScanBenchGraph()
	engine := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		DisableMinLabelScan: !enabled,
		// The result is |Rare| = 1000 rows; lift the row cap so neither variant
		// trips it.
		MaxResultRows: cypher.MaxResultRowsUnlimited,
	})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := engine.Run(ctx, minLabelScanBenchQuery, nil)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		for res.Next() {
		}
		if e := res.Err(); e != nil {
			b.Fatalf("Err: %v", e)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}

// BenchmarkMinLabelScanSelective_MinLabel measures the optimised plan: the scan
// re-anchored on the smaller :Rare label.
func BenchmarkMinLabelScanSelective_MinLabel(b *testing.B) {
	benchMinLabelScan(b, true)
}

// BenchmarkMinLabelScanSelective_FirstLabel measures the legacy plan for the
// same query — a full NodeByLabelScan of the first (large) :Common label plus a
// residual :Rare filter, the baseline the peephole replaces.
func BenchmarkMinLabelScanSelective_FirstLabel(b *testing.B) {
	benchMinLabelScan(b, false)
}

// TestMinLabelScanBench_ResultsMatch is a fast guard run as part of the normal
// test layer: it confirms the benchmark query returns the identical row count
// under both plans, so the benchmark compares like with like.
func TestMinLabelScanBench_ResultsMatch(t *testing.T) {
	g := buildMinLabelScanBenchGraph()
	on := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: cypher.MaxResultRowsUnlimited})
	off := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableMinLabelScan: true, MaxResultRows: cypher.MaxResultRowsUnlimited})
	count := func(e *cypher.Engine) int {
		res, err := e.Run(context.Background(), minLabelScanBenchQuery, nil)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for res.Next() {
			n++
		}
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		_ = res.Close()
		return n
	}
	non := count(on)
	noff := count(off)
	if non != noff {
		t.Fatalf("row-count mismatch: minlabel=%d firstlabel=%d", non, noff)
	}
	// Every :Rare node is also :Common, so the conjunction :Common ∩ :Rare = :Rare.
	if non != mlsRarePop {
		t.Fatalf("unexpected row count %d, want %d", non, mlsRarePop)
	}
}
