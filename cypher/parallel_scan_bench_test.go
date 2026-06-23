package cypher_test

// parallel_scan_bench_test.go — scaling and small-query evidence for the
// morsel-parallel-reduce count fast path (#1672).
//
// The parallel count path spreads a large group-by-less count over up to
// GOMAXPROCS worker goroutines, summing per-worker partial counters. These
// benchmarks measure two things the acceptance criteria require:
//
//   - SCALING: big-graph count latency falls as cores are added. Run the
//     parallel benchmarks under -cpu=1,2,4,8 and compare ns/op across the sweep;
//     the serial control (DisableParallelScan) stays flat, isolating the win.
//   - SMALL-QUERY: a graph at or below the threshold takes the serial path
//     regardless of the flag, so its latency is unchanged. The *_Small pair
//     shows the parallel-enabled and parallel-disabled engines are within noise.
//
// The full-node scan itself is not parallelised in the planner (the full-scan
// funnel benchmarked as a regression), so there is no scan benchmark here.
//
// Layer: short. Run with, e.g.:
//
//	go test -run=^$ -bench='BenchmarkParallelScan' -benchmem -cpu=1,2,4,8 ./cypher/
//	go test -run=^$ -bench='BenchmarkParallelScan_.*Small' -benchmem -count=8 ./cypher/

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedGraphDirect inserts n bare nodes straight onto the graph (bypassing the
// engine write path), so a large benchmark fixture builds quickly.
func seedGraphDirect(b *testing.B, n int) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		k := "n" + itoaBench(i)
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
	}
	return g
}

// itoaBench is a tiny non-allocating-enough integer formatter for node keys.
func itoaBench(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// runDrain executes q to completion once, failing the benchmark on error.
func runDrain(b *testing.B, eng *cypher.Engine, q string) {
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		b.Fatal(err)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if err := res.Err(); err != nil {
		b.Fatal(err)
	}
	_ = res.Close()
}

// benchParallel runs q over a graph of n nodes with the parallel scan enabled
// (low threshold so it engages) or disabled, isolating the parallel win.
func benchParallel(b *testing.B, n int, parallel bool, q string) {
	g := seedGraphDirect(b, n)
	opts := cypher.EngineOptions{ParallelScanThreshold: 1} // engage on any non-trivial graph
	if !parallel {
		opts = cypher.EngineOptions{DisableParallelScan: true}
	}
	eng := cypher.NewEngineWithOptions(g, opts)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, q)
	}
}

// ── Count reduce: big graph, parallel vs serial (run under -cpu=1,2,4,8) ──

const benchBigN = 200_000

func BenchmarkParallelScan_CountBig_Parallel(b *testing.B) {
	benchParallel(b, benchBigN, true, "MATCH (n) RETURN count(*)")
}
func BenchmarkParallelScan_CountBig_Serial(b *testing.B) {
	benchParallel(b, benchBigN, false, "MATCH (n) RETURN count(*)")
}

// ── Small query: at the default threshold boundary, both paths stay serial ──
//
// The graph is far below DefaultParallelScanThreshold, so the parallel-enabled
// engine takes the serial path too. The two benchmarks must be within noise,
// proving the threshold gate leaves small-query latency unaffected.

const benchSmallN = 200

func BenchmarkParallelScan_CountSmall_DefaultEnabled(b *testing.B) {
	// Default options: parallel enabled, default threshold (50k) → stays serial.
	g := seedGraphDirect(b, benchSmallN)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, "MATCH (n) RETURN count(*)")
	}
}
func BenchmarkParallelScan_CountSmall_Disabled(b *testing.B) {
	benchParallel(b, benchSmallN, false, "MATCH (n) RETURN count(*)")
}

// ── Fused scan→project: big graph, parallel vs serial (run under -cpu=1,2,4,8) ──
//
// These exercise the morsel-parallel fused scan→filter→project (#1682). The scan
// (`RETURN n.v`) and the scan+filter (`WHERE n.v >= half RETURN n.v`) latency must
// FALL as cores are added on the *_Parallel variants; the *_Serial controls
// (DisableParallelScan) stay flat, isolating the parallel win. Result rows are
// materialised, so these reflect the real end-to-end pushed-down filter/projection.
//
//	go test -run=^$ -bench='BenchmarkParallelScanProject' -benchmem -cpu=1,2,4,8 ./cypher/

// seedGraphWithProp inserts n bare nodes each carrying an integer "v" property, so
// scan+filter benchmarks have a property to project and filter on.
func seedGraphWithProp(b *testing.B, n int) *lpg.Graph[string, float64] {
	b.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		k := "n" + itoaBench(i)
		if err := g.AddNode(k); err != nil {
			b.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			b.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

// benchParallelProp runs q over an n-node property graph with the parallel scan
// enabled (low threshold) or disabled.
func benchParallelProp(b *testing.B, n int, parallel bool, q string) {
	g := seedGraphWithProp(b, n)
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

func BenchmarkParallelScanProject_ScanBig_Parallel(b *testing.B) {
	benchParallelProp(b, benchBigN, true, "MATCH (n) RETURN n.v")
}
func BenchmarkParallelScanProject_ScanBig_Serial(b *testing.B) {
	benchParallelProp(b, benchBigN, false, "MATCH (n) RETURN n.v")
}

func BenchmarkParallelScanProject_ScanFilterBig_Parallel(b *testing.B) {
	benchParallelProp(b, benchBigN, true, "MATCH (n) WHERE n.v >= 100000 RETURN n.v")
}
func BenchmarkParallelScanProject_ScanFilterBig_Serial(b *testing.B) {
	benchParallelProp(b, benchBigN, false, "MATCH (n) WHERE n.v >= 100000 RETURN n.v")
}

// ── Small query: below threshold, the parallel-enabled engine stays serial ──

func BenchmarkParallelScanProject_ScanSmall_DefaultEnabled(b *testing.B) {
	g := seedGraphWithProp(b, benchSmallN)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{}) // default 50k threshold → serial
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, "MATCH (n) RETURN n.v")
	}
}
func BenchmarkParallelScanProject_ScanSmall_Disabled(b *testing.B) {
	benchParallelProp(b, benchSmallN, false, "MATCH (n) RETURN n.v")
}
