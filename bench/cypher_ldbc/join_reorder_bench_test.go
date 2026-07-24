package cypher_ldbc_test

// join_reorder_bench_test.go — benchmark demonstrating the disjoint-component
// ordering peephole (#2091) win: a comma-separated Cartesian `MATCH (a:Big),(b:Small)`
// whose written order drives the LARGER component (Big outer, Small inner) is
// reordered so the SMALLER component drives (Small outer, Big inner).
//
// exec.Apply is a Volcano dependent join: it re-initialises and re-drains the
// inner plan once per OUTER row. Driving the large Big side (the written order)
// re-inits the inner Small scan |Big| times; driving the small Small side (the
// reordered plan) re-inits the inner Big scan only |Small| times. The Cartesian
// product itself (|Big|·|Small| rows) is enumerated identically either way — the
// win is the |Big|−|Small| fewer inner re-initialisations (each a label-bitmap
// Intersect + iterator allocation + outer-row copy). count(*) is an order-blind
// aggregate, so SuppressReorder passes and the swap fires; it consumes the whole
// product without materialising per-row output, isolating the reorder delta from
// result-set materialisation. The result (the product size) is identical under
// both plans, proven by the differential test in cypher/join_reorder_diff_test.go.
//
// Run:
//
//	go test -run='^$' -bench=BenchmarkJoinReorder -benchmem -count=10 ./bench/cypher_ldbc/...

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// jrBenchBigPop and jrBenchSmallPop are the two disjoint component sizes. The
// written order drives Big; the peephole swaps it to drive the far smaller Small,
// turning |Big| inner re-inits into |Small|. The product (|Big|·|Small|) stays
// small enough to run quickly while the re-init asymmetry (|Big| vs |Small|)
// dominates.
const (
	jrBenchBigPop   = 60_000
	jrBenchSmallPop = 3
)

// joinReorderBenchQuery is a disjoint two-component Cartesian aggregated by an
// order-blind count(*), so the reorder is order-safe and fires, and no per-row
// output is materialised.
const joinReorderBenchQuery = "MATCH (a:Big), (b:Small) RETURN count(*) AS c"

// buildJoinReorderBenchGraph seeds jrBenchBigPop :Big nodes and jrBenchSmallPop
// :Small nodes, each with a unique integer key. Node counts are the exact base
// cardinalities the peephole reads from the label index.
func buildJoinReorderBenchGraph() *lpg.Graph[string, float64] {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	k := 0
	for i := 0; i < jrBenchBigPop; i++ {
		key := fmt.Sprintf("n%d", k)
		_ = g.AddNode(key)
		_ = g.SetNodeLabel(key, "Big")
		_ = g.SetNodeProperty(key, "k", lpg.Int64Value(int64(k)))
		k++
	}
	for i := 0; i < jrBenchSmallPop; i++ {
		key := fmt.Sprintf("n%d", k)
		_ = g.AddNode(key)
		_ = g.SetNodeLabel(key, "Small")
		_ = g.SetNodeProperty(key, "k", lpg.Int64Value(int64(k)))
		k++
	}
	g.SetIndexManager(index.NewManager())
	return g
}

// benchJoinReorder runs the disjoint Cartesian query with the reorder either
// enabled or disabled, on a freshly-seeded graph.
func benchJoinReorder(b *testing.B, enabled bool) {
	b.Helper()
	g := buildJoinReorderBenchGraph()
	engine := cypher.NewEngineWithOptions(g, cypher.EngineOptions{
		DisableJoinReorder: !enabled,
		MaxResultRows:      cypher.MaxResultRowsUnlimited,
	})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := engine.Run(ctx, joinReorderBenchQuery, nil)
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

// BenchmarkJoinReorderDisjoint_Reordered measures the optimised plan: the small
// component drives, so the inner (large) side is re-initialised only |Small| times.
func BenchmarkJoinReorderDisjoint_Reordered(b *testing.B) {
	benchJoinReorder(b, true)
}

// BenchmarkJoinReorderDisjoint_Written measures the legacy plan for the same query
// — the large component drives, re-initialising the inner (small) side |Big| times,
// the baseline the peephole reorders.
func BenchmarkJoinReorderDisjoint_Written(b *testing.B) {
	benchJoinReorder(b, false)
}

// TestJoinReorderBench_ResultsMatch is a fast guard run as part of the normal test
// layer: it confirms the benchmark query returns the identical count under both
// plans, so the benchmark compares like with like.
func TestJoinReorderBench_ResultsMatch(t *testing.T) {
	g := buildJoinReorderBenchGraph()
	on := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: cypher.MaxResultRowsUnlimited})
	off := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableJoinReorder: true, MaxResultRows: cypher.MaxResultRowsUnlimited})
	countVal := func(e *cypher.Engine) string {
		res, err := e.Run(context.Background(), joinReorderBenchQuery, nil)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		for res.Next() {
			rec := res.Record()
			got = fmt.Sprint(rec["c"])
		}
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		_ = res.Close()
		return got
	}
	con := countVal(on)
	coff := countVal(off)
	if con != coff {
		t.Fatalf("count mismatch: reorder=%s written=%s", con, coff)
	}
	if want := fmt.Sprint(int64(jrBenchBigPop) * int64(jrBenchSmallPop)); con != want {
		t.Fatalf("unexpected count %s, want %s", con, want)
	}
}
