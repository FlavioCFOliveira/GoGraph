package cypher

// join_reorder_stats_bench_test.go — the end-to-end measurement for the
// property-statistics-driven reorder (rmp #2766).
//
// BenchmarkJoinReorderStatsSkewed is the A/B: the SAME query on the SAME graph,
// once with the reorder enabled (the filtered component drives) and once with it
// disabled (the written order drives, re-scanning the large label once per row of
// the small one). Run both arms interleaved and compare with benchstat; running
// all of one arm and then all of the other biases the result with thermal drift.
//
// BenchmarkReorderDrainCost measures the modelling assumption the cost rule rests
// on: that a Selection over a label scan drains the WHOLE label. It compares a
// full filtered scan against a full bare scan of the same label, so the
// per-drained-row predicate surcharge the cost model folds into its margin is a
// measured number rather than an assumed one.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	benchReorderA    = 20000 // :A nodes
	benchReorderB    = 100   // :B nodes
	benchReorderHits = 3     // :A nodes with x = 1
)

func benchReorderEngine(b *testing.B, reorder bool, g *lpg.Graph[string, float64]) *Engine {
	b.Helper()
	var e *Engine
	if reorder {
		e = NewEngine(g)
	} else {
		e = NewEngineWithOptions(g, EngineOptions{DisableJoinReorder: true})
	}
	if err := e.RefreshStatistics(context.Background()); err != nil {
		b.Fatal(err)
	}
	return e
}

func BenchmarkJoinReorderStatsSkewed(b *testing.B) {
	g := buildSkewGraph(b, benchReorderA, benchReorderB, benchReorderHits)
	const q = "MATCH (b:B), (a:A {x: 1}) RETURN a.x AS ax, b.y AS bv"
	for _, arm := range []struct {
		name    string
		reorder bool
	}{{"on", true}, {"off", false}} {
		b.Run("reorder="+arm.name, func(b *testing.B) {
			e := benchReorderEngine(b, arm.reorder, g)
			// One untimed execution so the plan cache and the statistics snapshot are
			// warm in both arms; the measurement is of execution, not of first-parse.
			drainBenchQuery(b, e, q)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				drainBenchQuery(b, e, q)
			}
		})
	}
}

func BenchmarkReorderDrainCost(b *testing.B) {
	g := buildSkewGraph(b, benchReorderA, 1, benchReorderHits)
	e := NewEngine(g)
	if err := e.RefreshStatistics(context.Background()); err != nil {
		b.Fatal(err)
	}
	for _, arm := range []struct {
		name, query string
	}{
		// sum(a.x) rather than count(a): count over a bare label scan is served by
		// the O(1) LabelCountScan pushdown and drains nothing, so it would have
		// measured the pushdown instead of a scan. Both arms read the same property,
		// so the only difference between them is the predicate evaluation.
		{"bare", "MATCH (a:A) RETURN sum(a.x) AS s"},
		{"filtered", "MATCH (a:A) WHERE a.x = 1 RETURN sum(a.x) AS s"},
	} {
		b.Run("scan="+arm.name, func(b *testing.B) {
			drainBenchQuery(b, e, arm.query)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				drainBenchQuery(b, e, arm.query)
			}
		})
	}
}

func drainBenchQuery(b *testing.B, e *Engine, q string) {
	b.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		b.Fatalf("Run(%q): %v", q, err)
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		b.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		b.Fatalf("Close(%q): %v", q, err)
	}
	if n == 0 {
		b.Fatalf("%q returned no rows; the benchmark is measuring nothing", q)
	}
	_ = fmt.Sprint(n)
}

// BenchmarkJoinReorderStatsLiveHistory is the measurement rmp #2771 exists for: the
// SAME query on the SAME graph, with MVCC version records deliberately LIVE at plan
// time.
//
// That state is not exotic. [lpg.Graph.LabelCountExact] declines the moment any
// node-life or label-delta record is unreclaimed, records are reclaimed by an
// ASYNCHRONOUS vacuum, and under a concurrent writer they are essentially always
// present. Before rmp #2771 the range estimator read that decline as the number
// zero, demoted its estimate, and the reorder did not fire — so the query paid the
// written order's repeated large scan. The estimator now distinguishes a declined
// count from an empty label, and the same plan is chosen with or without history.
//
// The graph is rebuilt per arm rather than shared, because the open reader that
// keeps history live is bound to the graph it was taken from.
func BenchmarkJoinReorderStatsLiveHistory(b *testing.B) {
	const q = "MATCH (a:A) WHERE a.x > 4900 MATCH (b:B {y: 7}) RETURN a.x AS ax, b.y AS bv"
	for _, arm := range []struct {
		name string
		live bool
	}{{"history=quiet", false}, {"history=live", true}} {
		b.Run(arm.name, func(b *testing.B) {
			g := buildRangeSkewGraph(b, 5000, 2000)
			e := NewEngine(g)
			if err := e.RefreshStatistics(context.Background()); err != nil {
				b.Fatal(err)
			}
			if arm.live {
				// A reader FIRST, so the reclamation watermark cannot pass the write
				// that follows; the record it pushes then stays live for the whole
				// measurement instead of being vacuumed away mid-run.
				held := g.BeginRead()
				defer g.EndRead(held)
				mustNode(b, g, "zz-bench-live", "ZZUnrelated", "zz", 1)
				if g.NodeLifeVersionCount() == 0 {
					b.Fatal("no live node-life record survived; this arm is not measuring " +
						"what it claims to")
				}
			}
			drainBenchQuery(b, e, q)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				drainBenchQuery(b, e, q)
			}
		})
	}
}
