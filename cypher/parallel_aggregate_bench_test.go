package cypher_test

// parallel_aggregate_bench_test.go — scaling and no-regression evidence for the
// morsel-parallel aggregate scan (#2111: min/max/group-by) and the O(1) serial
// full-node count pushdown (#2113 / #2066).
//
// SCALING (#2111): big-graph min/max latency falls as cores are added on the
// *_Parallel variants; the *_Serial controls (DisableParallelScan) stay flat,
// isolating the parallel win. Bench min/max over a real per-row PROPERTY (which
// actually parallelises) rather than count (near-free).
//
//	go test -run=^$ -bench='BenchmarkParallelAggregate' -benchmem -cpu=1,2,4,8 ./cypher/
//
// NO-REGRESSION (#2111): a graph below the threshold takes the serial path
// regardless of the flag, so the *_Small pair must be within noise.
//
//	go test -run=^$ -bench='BenchmarkParallelAggregate_.*Small' -benchmem -count=8 ./cypher/
//
// O(1) COUNT PUSHDOWN (#2113): count(*) over a bare scan reads LiveOrder() in O(1)
// (BenchmarkCountPushdown_O1), versus the full O(N) scan it replaces — a bare-scan
// count forced through a trivially-true Selection (BenchmarkCountPushdown_FullScan).
// The O(1) variant's ns/op is independent of N; the full-scan variant scales with N.
//
//	go test -run=^$ -bench='BenchmarkCountPushdown' -benchmem -count=8 ./cypher/
//
// Layer: short.

import (
	"io"
	"log/slog"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// silenceBenchLogs discards the engine's slog output for the benchmark's duration
// so the per-construction "non-multigraph" WARN does not interleave into stdout and
// corrupt the benchmark lines benchstat parses. Restored via b.Cleanup.
func silenceBenchLogs(b *testing.B) {
	b.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(prev) })
}

// ── #2111 parallel min / max: big graph, parallel vs serial (run -cpu=1,2,4,8) ──

func BenchmarkParallelAggregate_MinBig_Parallel(b *testing.B) {
	silenceBenchLogs(b)
	benchParallelProp(b, benchBigN, true, "MATCH (n) RETURN min(n.v) AS m")
}
func BenchmarkParallelAggregate_MinBig_Serial(b *testing.B) {
	silenceBenchLogs(b)
	benchParallelProp(b, benchBigN, false, "MATCH (n) RETURN min(n.v) AS m")
}

func BenchmarkParallelAggregate_MaxBig_Parallel(b *testing.B) {
	silenceBenchLogs(b)
	benchParallelProp(b, benchBigN, true, "MATCH (n) RETURN max(n.v) AS m")
}
func BenchmarkParallelAggregate_MaxBig_Serial(b *testing.B) {
	silenceBenchLogs(b)
	benchParallelProp(b, benchBigN, false, "MATCH (n) RETURN max(n.v) AS m")
}

// Grouped min over a computed key (n.v % 100 → 100 groups) exercises the group-by
// partial-map merge under the exact grouping comparator.
func BenchmarkParallelAggregate_GroupMinBig_Parallel(b *testing.B) {
	silenceBenchLogs(b)
	benchParallelProp(b, benchBigN, true, "MATCH (n) RETURN n.v % 100 AS g, min(n.v) AS m")
}
func BenchmarkParallelAggregate_GroupMinBig_Serial(b *testing.B) {
	silenceBenchLogs(b)
	benchParallelProp(b, benchBigN, false, "MATCH (n) RETURN n.v % 100 AS g, min(n.v) AS m")
}

// ── #2111 no-regression: below threshold both paths stay serial ──

func BenchmarkParallelAggregate_MinSmall_DefaultEnabled(b *testing.B) {
	silenceBenchLogs(b)
	g := seedGraphWithProp(b, benchSmallN)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{}) // default 50k threshold → serial
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runDrain(b, eng, "MATCH (n) RETURN min(n.v) AS m")
	}
}
func BenchmarkParallelAggregate_MinSmall_Disabled(b *testing.B) {
	silenceBenchLogs(b)
	benchParallelProp(b, benchSmallN, false, "MATCH (n) RETURN min(n.v) AS m")
}

// ── #2113 O(1) count pushdown vs the full O(N) scan it replaces ──
//
// Both run on the same big property graph with the parallel path DISABLED, so the
// O1 variant is served by the serial AllNodesCountScan (LiveOrder in O(1)) and the
// FullScan variant — whose Selection makes the child a non-bare scan — is served by
// a full node scan that counts every surviving row.

func BenchmarkCountPushdown_O1(b *testing.B) {
	silenceBenchLogs(b)
	benchParallelProp(b, benchBigN, false, "MATCH (n) RETURN count(*) AS c")
}
func BenchmarkCountPushdown_FullScan(b *testing.B) {
	silenceBenchLogs(b)
	benchParallelProp(b, benchBigN, false, "MATCH (n) WHERE n.v >= 0 RETURN count(*) AS c")
}
