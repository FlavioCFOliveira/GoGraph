package cypher_test

// label_count_bench_test.go — allocation/throughput evidence for the label-scan
// count pushdown (#2004).
//
// `MATCH (p:Label) RETURN count(*)` / `count(p)` compiles to an aggregation over
// a NodeByLabelScan leaf. Before #2004 the only count fast path served a bare
// full-node scan (ParallelCountScan, #1672), so a labelled count still ran the
// serial EagerAggregation pipeline: one row materialised per labelled node,
// ~1 alloc/node. #2004 recognises the label-scan count shape and reads the
// label bitmap's cardinality directly (LabelCountScan), collapsing the per-node
// cost to a single O(1) read.
//
// The *_Pushdown benchmarks run with the pushdown enabled (low threshold so it
// engages); the *_Serial controls run with DisableParallelScan so they always
// take the serial pipeline. Compare allocs/op and ns/op across the pair — the
// pushdown variant should drop toward a small constant while the serial control
// stays at ~1 alloc/node.
//
// Layer: short. Run with, e.g.:
//
//	go test -run=^$ -bench='BenchmarkCount_Label' -benchmem -count=6 ./cypher/

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedGraphLabeled inserts n bare nodes straight onto the graph, each carrying
// the given label, so a labelled-count benchmark fixture builds quickly.
func seedGraphLabeled(b *testing.B, n int, label string) *lpg.Graph[string, float64] {
	b.Helper()
	// Multigraph so NewEngineWithOptions does not emit its non-multigraph warning
	// (which would pollute stdout and the benchstat input); node-count semantics
	// are identical either way.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		k := "n" + itoaBench(i)
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, label); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
	}
	return g
}

// benchLabelCount runs q over a graph of n labelled nodes with the count
// pushdown enabled (low threshold so it engages) or disabled, isolating the win.
func benchLabelCount(b *testing.B, n int, pushdown bool, q string) {
	g := seedGraphLabeled(b, n, "Item")
	opts := cypher.EngineOptions{ParallelScanThreshold: 1} // engage on any non-trivial graph
	if !pushdown {
		opts = cypher.EngineOptions{DisableParallelScan: true}
	}
	eng := cypher.NewEngineWithOptions(g, opts)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, q)
	}
}

// ── Label count: big graph, pushdown vs serial ──

func BenchmarkCount_LabelStarBig_Pushdown(b *testing.B) {
	benchLabelCount(b, benchBigN, true, "MATCH (p:Item) RETURN count(*)")
}
func BenchmarkCount_LabelStarBig_Serial(b *testing.B) {
	benchLabelCount(b, benchBigN, false, "MATCH (p:Item) RETURN count(*)")
}

func BenchmarkCount_LabelVarBig_Pushdown(b *testing.B) {
	benchLabelCount(b, benchBigN, true, "MATCH (p:Item) RETURN count(p)")
}
func BenchmarkCount_LabelVarBig_Serial(b *testing.B) {
	benchLabelCount(b, benchBigN, false, "MATCH (p:Item) RETURN count(p)")
}
