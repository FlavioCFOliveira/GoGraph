package cypher_test

// parallel_scan_gate_cost_bench_test.go — the cost of the parallel-scan gate itself
// (rmp #2380).
//
// [tryBuildParallelScanProject] decides whether a single-label leaf is worth
// running in parallel by comparing the label's cardinality against
// DefaultParallelScanThreshold (50 000, strict >). It used to obtain that
// cardinality by materialising the label's bitmap, and materialising it CLONES
// the label's live set — so every graph below the threshold paid a full clone
// for an answer that was always "no", then discarded it.
//
// Found by profiling examples/35_mvcc_mixed_workload, which has 3000 nodes and
// therefore can never admit: roaring's arrayContainer.clone was 47.9% of ALL
// allocation in the process, reached 100% through this gate.
//
// These arms hold the graph BELOW the threshold on purpose, so the gate always
// declines and the benchmark measures exactly the cost of asking:
//
//	go test -run=^$ -bench='BenchmarkParallelScanGate' -benchmem -count=6 ./cypher/
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

// gateBenchN is deliberately far BELOW DefaultParallelScanThreshold (50 000), so
// useParallelScanForRows can never admit and the gate's verdict is fixed. It is
// still large enough that cloning its bitmap is clearly measurable.
const gateBenchN = 20_000

// seedGateBenchGraph builds gateBenchN :P nodes each carrying an int64 v.
func seedGateBenchGraph(tb testing.TB) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := range gateBenchN {
		k := "n" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

// runGateQuery executes q once and drains it, returning the row count.
func runGateQuery(tb testing.TB, eng *cypher.Engine, q string) int {
	tb.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		tb.Fatalf("run %q: %v", q, err)
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("iterate %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("close %q: %v", q, err)
	}
	return n
}

// gateBenchQueries are the two shapes the gate screens, chosen to separate the
// two halves of the win:
//
//   - "scan" ends on a serial NodeByLabelScan, which resolves the label bitmap
//     itself. Removing the gate's clone removes one of the two.
//   - "seek" ends on a plan that never materialises the label bitmap at all, so
//     the gate's clone was the ONLY one. This is the shape that dominates
//     example 35, and where the whole cost disappears.
var gateBenchQueries = []struct{ name, query string }{
	{"scan", "MATCH (n:P) RETURN n.v"},
	{"seek", "MATCH (n:P) WHERE n.v = 17 RETURN n.v"},
}

func BenchmarkParallelScanGate(b *testing.B) {
	for _, tc := range gateBenchQueries {
		b.Run(tc.name, func(b *testing.B) {
			silenceBenchLogs(b)
			g := seedGateBenchGraph(b)
			eng := cypher.NewEngine(g)
			// Warm: the first execution builds one-off state that would otherwise
			// be attributed to the path under test.
			runGateQuery(b, eng, tc.query)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				runGateQuery(b, eng, tc.query)
			}
		})
	}
}
