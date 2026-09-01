package cypher_test

// count_block_form_bench_test.go — rmp #2648's permanent benchmark: what the
// block-form normalisation is worth, and the proof that it is a CONSTANT-FACTOR
// reduction rather than a change of complexity.
//
// # What is being compared
//
// Four arms per shape, over ONE shared graph, so nothing but the access path
// differs:
//
//	block/rewritten     COUNT { MATCH (a)-[:K]->(:Q) }  on a default engine
//	block/unrewritten   the same text, with the adjacency rewrites forbidden
//	pattern/rewritten   COUNT { (a)-[:K]->(:Q) }        on a default engine
//	pattern/unrewritten the same text, with the adjacency rewrites forbidden
//
// block/unrewritten is the PRE-#2648 BASELINE, and it is faithful rather than
// approximate. Before #2648 the block form reached the recognisers as a nil
// pattern and was rejected by their first clause, so it compiled an inner plan
// and drove it once per outer row. With [cypher.EngineOptions.DisableAdjacencyCountRewrites]
// set, [subqueryEvaluator.degreeShapeFor] returns nil before the memo and the
// same inner plan is driven the same way. The two differ by one boolean test, and
// they differ by NOTHING in the work that dominates. Measuring the baseline this
// way keeps both arms in one process and one build, which is what makes the pair
// interleavable — the alternative, two builds, put the arms in different
// processes and left every difference confounded with build and machine state.
//
// pattern/rewritten is the NULL CONTROL. The pattern form already took the fast
// path before this change, and the normalisation is reached only when
// [ast.CountSubquery].Pattern is nil, so this arm must not move. It is also where
// a regression would show up if the derivation had been left at the per-row
// dispatch site instead of inside the per-occurrence memo.
//
// # Why the row counts are parameterised
//
// #2646 established that every one of these shapes is LINEAR in outer rows; this
// change reduces the per-row constant and must not be reported as anything more.
// Each shape therefore runs at two row counts an order of magnitude apart. The
// evidence for "constant factor" is that block/unrewritten ÷ block/rewritten is
// the SAME ratio at both sizes while each arm's own ns/op scales with the rows.
// A complexity change would show the ratio growing with the size.
//
// Run with:
//
//	go test -run=^$ -bench='BenchmarkBlockFormCount' -benchmem -count=10 ./cypher/
//
// The counters the correctness suite reads are process-global, so this file
// deliberately asserts on nothing: which arm takes which path is pinned by
// TestBlockFormNormalisation_ReachesTheAdjacencyRewrites, not here.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// blockFormFanout is each anchor's out-degree. Four is the property-graph degree
// the round-4 audit used for the numbers this task is measured against, and it is
// deliberately SMALL: the cost being removed is a per-outer-row constant, and a
// large fan-out would bury it under the walk itself.
const blockFormFanout = 4

// blockFormRowCounts are the outer-row counts every shape is measured at. Two
// sizes an order of magnitude apart is the minimum that can distinguish a
// constant-factor gain from a complexity change.
var blockFormRowCounts = []int{250, 2500}

// blockFormGraphs memoises one graph per row count for the whole process.
// Rebuilding per benchmark function per -count repetition was measured, on the
// #2265 benchmark in this package, to move untouched control arms by more than
// 100% through GC pressure alone.
var blockFormGraphs sync.Map // int -> *lpg.Graph[string, float64]

// blockFormGraph returns the shared graph for rows anchors, building it once.
//
// Each anchor carries :P and gets blockFormFanout out-edges of type :K. Half the
// far nodes carry :Q, so the labelled-hop walk has something to discriminate on
// and a recogniser that ignored the label would return a visibly wrong count
// rather than merely a different time.
func blockFormGraph(tb testing.TB, rows int) *lpg.Graph[string, float64] {
	tb.Helper()
	if g, ok := blockFormGraphs.Load(rows); ok {
		return g.(*lpg.Graph[string, float64])
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())

	for i := 0; i < rows; i++ {
		a := fmt.Sprintf("a%d", i)
		if err := g.AddNode(a); err != nil {
			tb.Fatalf("AddNode(%s): %v", a, err)
		}
		if err := g.SetNodeLabel(a, "P"); err != nil {
			tb.Fatalf("SetNodeLabel(%s): %v", a, err)
		}
		if err := g.SetNodeProperty(a, "id", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty(%s): %v", a, err)
		}
		for j := 0; j < blockFormFanout; j++ {
			d := fmt.Sprintf("d%d_%d", i, j)
			if err := g.AddNode(d); err != nil {
				tb.Fatalf("AddNode(%s): %v", d, err)
			}
			// Half the targets qualify, so the label test is load-bearing.
			if j%2 == 0 {
				if err := g.SetNodeLabel(d, "Q"); err != nil {
					tb.Fatalf("SetNodeLabel(%s): %v", d, err)
				}
			}
			if err := g.AddEdge(a, d, 1); err != nil {
				tb.Fatalf("AddEdge(%s,%s): %v", a, d, err)
			}
			g.SetEdgeLabel(a, d, "K")
		}
	}
	actual, _ := blockFormGraphs.LoadOrStore(rows, g)
	return actual.(*lpg.Graph[string, float64])
}

// blockFormEngines returns the two arms for one graph: a default engine, and one
// that may not answer a count from the adjacency.
func blockFormEngines(g *lpg.Graph[string, float64]) (rewritten, unrewritten *cypher.Engine) {
	return cypher.NewEngine(g),
		cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableAdjacencyCountRewrites: true})
}

// drainBlockFormQuery runs q to completion, touching every column so no lazy value is
// left unmaterialised and the measurement covers the whole answer.
func drainBlockFormQuery(b *testing.B, eng *cypher.Engine, q string) {
	b.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		b.Fatalf("run %q: %v", q, err)
	}
	for res.Next() {
		for i := range res.Columns() {
			_ = res.ValueAt(i)
		}
	}
	if err := res.Err(); err != nil {
		b.Fatalf("run %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		b.Fatalf("close %q: %v", q, err)
	}
}

// BenchmarkBlockFormCount is the measurement for rmp #2648.
//
// Sub-benchmark names are `<shape>/<rows>/<arm>`, so benchstat can be pointed at
// one arm across row counts (is each arm linear?) or at one row count across arms
// (what is the ratio?).
func BenchmarkBlockFormCount(b *testing.B) {
	shapes := []struct {
		name    string
		block   string
		pattern string
		note    string
	}{
		{
			name:    "labelled_hop",
			block:   "MATCH (a:P) RETURN COUNT { MATCH (a)-[:K]->(:Q) }",
			pattern: "MATCH (a:P) RETURN COUNT { (a)-[:K]->(:Q) }",
			note:    "served by countLabelledHop — one adjacency walk per outer row",
		},
		{
			name:    "typed_degree",
			block:   "MATCH (a:P) RETURN COUNT { MATCH (a)-[:K]->() }",
			pattern: "MATCH (a:P) RETURN COUNT { (a)-[:K]->() }",
			note:    "served by the degree rewrite — one bounded degree read per outer row",
		},
		{
			name:    "bounded_comparison",
			block:   "MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(:Q) } > 0 RETURN count(a)",
			pattern: "MATCH (a:P) WHERE COUNT { (a)-[:K]->(:Q) } > 0 RETURN count(a)",
			note:    "the EvalCountBounded entry point, where the walk short-circuits at the cap",
		},
	}

	for _, rows := range blockFormRowCounts {
		g := blockFormGraph(b, rows)
		rewritten, unrewritten := blockFormEngines(g)

		for _, sh := range shapes {
			b.Run(fmt.Sprintf("%s/rows=%d/block_rewritten", sh.name, rows), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					drainBlockFormQuery(b, rewritten, sh.block)
				}
			})
			b.Run(fmt.Sprintf("%s/rows=%d/block_unrewritten", sh.name, rows), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					drainBlockFormQuery(b, unrewritten, sh.block)
				}
			})
			// The null control: already fast before this change, and reached
			// without the normalisation, so it must not move.
			b.Run(fmt.Sprintf("%s/rows=%d/pattern_rewritten", sh.name, rows), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					drainBlockFormQuery(b, rewritten, sh.pattern)
				}
			})
			b.Run(fmt.Sprintf("%s/rows=%d/pattern_unrewritten", sh.name, rows), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					drainBlockFormQuery(b, unrewritten, sh.pattern)
				}
			})
		}

		// A second null control that shares the engine and the fixture but no part
		// of the subquery path at all. If this moves between two runs, the run is
		// noise and no ratio above may be believed.
		b.Run(fmt.Sprintf("bare_scan/rows=%d", rows), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				drainBlockFormQuery(b, rewritten, "MATCH (a:P) RETURN a.id")
			}
		})
	}
}
