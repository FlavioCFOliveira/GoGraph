package cypher_test

// parallel_label_scan_bench_test.go — scaling evidence for the partitioned label scan
// (#2187).
//
// The round-3 audit measured the cost of adding a label that every node already carries
// at 2.0x to 3.7x, and confirmed causality by toggling DisableParallelScan: parallelism
// simply never engaged, because every parallel leaf required a bare unlabelled
// AllNodesScan. These benchmarks pair each labelled query with its unlabelled twin over
// the SAME graph, where every node carries the label, so the pair returns an identical
// multiset and the only difference is which leaf the planner could parallelise.
//
// Run at several core counts: the *_Parallel arms must fall as cores are added, while
// the *_Serial controls (DisableParallelScan) stay flat.
//
//	go test -run=^$ -bench='BenchmarkParallelLabelScan' -benchmem -cpu=1,10 -count=6 ./cypher/
//
// Layer: short.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// labelBenchN is well above the default 50 000 parallel threshold so the labelled leaf
// engages on its own cardinality.
const labelBenchN = 200_000

// seedLabelBenchGraph builds labelBenchN nodes ALL carrying label P, with an int64 v.
// Because every node carries P, the labelled and unlabelled queries return the identical
// multiset — which is what makes the pair a controlled comparison.
func seedLabelBenchGraph(b *testing.B) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < labelBenchN; i++ {
		k := "n" + strconv.Itoa(i)
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			b.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			b.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

func benchLabelScan(b *testing.B, parallel bool, q string) {
	silenceBenchLogs(b)
	g := seedLabelBenchGraph(b)
	opts := cypher.EngineOptions{ParallelScanThreshold: 1}
	if !parallel {
		opts = cypher.EngineOptions{DisableParallelScan: true}
	}
	eng := cypher.NewEngineWithOptions(g, opts)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, q)
	}
}

// ── projection leaf: labelled vs unlabelled, parallel vs serial ──

func BenchmarkParallelLabelScan_LabelledProject_Parallel(b *testing.B) {
	benchLabelScan(b, true, "MATCH (n:P) RETURN n.v + 0 AS v")
}
func BenchmarkParallelLabelScan_LabelledProject_Serial(b *testing.B) {
	benchLabelScan(b, false, "MATCH (n:P) RETURN n.v + 0 AS v")
}
func BenchmarkParallelLabelScan_UnlabelledProject_Parallel(b *testing.B) {
	benchLabelScan(b, true, "MATCH (n) RETURN n.v + 0 AS v")
}

// ── aggregate leaf ──

func BenchmarkParallelLabelScan_LabelledMin_Parallel(b *testing.B) {
	benchLabelScan(b, true, "MATCH (n:P) RETURN min(n.v) AS m")
}
func BenchmarkParallelLabelScan_LabelledMin_Serial(b *testing.B) {
	benchLabelScan(b, false, "MATCH (n:P) RETURN min(n.v) AS m")
}
func BenchmarkParallelLabelScan_UnlabelledMin_Parallel(b *testing.B) {
	benchLabelScan(b, true, "MATCH (n) RETURN min(n.v) AS m")
}

// UnlabelledMin_Serial completes the 2x2: it is the serial columnar aggregate over a
// bare scan, the arm the parallel aggregate scan replaces when it engages.
func BenchmarkParallelLabelScan_UnlabelledMin_Serial(b *testing.B) {
	benchLabelScan(b, false, "MATCH (n) RETURN min(n.v) AS m")
}

// UnlabelledProject_Serial completes the projection 2x2, so the single-core behaviour of
// the parallel projection leaf can be read as a pre-existing property rather than
// something the label introduced.
func BenchmarkParallelLabelScan_UnlabelledProject_Serial(b *testing.B) {
	benchLabelScan(b, false, "MATCH (n) RETURN n.v + 0 AS v")
}
