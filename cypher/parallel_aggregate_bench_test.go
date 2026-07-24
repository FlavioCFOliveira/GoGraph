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
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
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

// ── #2115 concurrent load-test: parallel vs serial under the REAL governor ──
//
// This is the no-regression-under-saturation evidence for the budget==1 inline
// short-circuit. It fires `conc` parallel min queries concurrently on ONE shared
// Engine, so they contend for the SAME [exec.ParallelGovernor] exactly as
// concurrent client queries would. One op = one batch of `conc` queries, so the
// parallel and serial arms do identical work at each conc level and their ratio is
// the pure path effect.
//
//   - conc=1: a single query in flight gets the full GOMAXPROCS budget → the
//     intra-query parallel win (idle-core-bound).
//   - conc=8/64: the governor throttles every query toward budget 1, where the
//     short-circuit runs the reduce inline (no goroutine/channel/context/pprof).
//     The parallel arm must be at least as fast as the serial arm — no regression.
//
// Read the ratio per conc level, not the geomean (the per-op work scales with
// conc). Compare the two impl columns with:
//
//	go test -run=^$ -bench='BenchmarkParallelAggregate_Concurrent' -benchmem -count=10 ./cypher/ > new.txt
//	benchstat -col /impl new.txt
//
// Layer: short.

func BenchmarkParallelAggregate_Concurrent(b *testing.B) {
	silenceBenchLogs(b)
	const q = "MATCH (n) RETURN min(n.v) AS m"

	// One property graph, seeded once and shared read-only by both arms (concurrent
	// reads are safe under the visibility barrier), so the fixture cost is untimed
	// and identical across arms.
	g := seedGraphWithProp(b, benchBigN)
	parEng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{ParallelScanThreshold: 1}) // engage the parallel path
	serEng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableParallelScan: true})

	for _, conc := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("conc=%d/impl=parallel", conc), func(b *testing.B) {
			benchConcurrentAgg(b, parEng, conc, q)
		})
		b.Run(fmt.Sprintf("conc=%d/impl=serial", conc), func(b *testing.B) {
			benchConcurrentAgg(b, serEng, conc, q)
		})
	}
}

// benchConcurrentAgg times b.N batches of `conc` concurrent drains of q on eng. A
// warm-up drain runs on the benchmark goroutine first, so any query error surfaces
// via b.Fatal legally (b.Fatal off the benchmark goroutine is forbidden); the timed
// goroutines only record the first error, reported after the loop.
func benchConcurrentAgg(b *testing.B, eng *cypher.Engine, conc int, q string) {
	b.Helper()
	if err := drainConcurrentQuery(eng, q); err != nil {
		b.Fatalf("warm-up query: %v", err)
	}

	var (
		mu       sync.Mutex
		firstErr error
	)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		wg.Add(conc)
		for w := 0; w < conc; w++ {
			go func() {
				defer wg.Done()
				if err := drainConcurrentQuery(eng, q); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
	b.StopTimer()
	if firstErr != nil {
		b.Fatalf("concurrent query failed: %v", firstErr)
	}
}

// drainConcurrentQuery runs q to completion once and returns the first error, if
// any. Unlike runDrain it never calls b.Fatal, so it is safe to invoke from a
// spawned goroutine.
func drainConcurrentQuery(eng *cypher.Engine, q string) error {
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		return err
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}
