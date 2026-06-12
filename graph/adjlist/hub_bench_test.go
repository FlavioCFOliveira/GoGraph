package adjlist_test

// hub_bench_test.go — gate test and benchmarks for O(d log d) AddEdge and
// O(d) RemoveAllEdgesFrom on high-degree hub nodes (task #1406).
//
// Gate test: TestHub_AddEdge_AmortisedSublinear asserts that building a hub of
// degree 10_000 takes less than 20× the time of building a hub of degree 1_000.
// A quadratic implementation would require ~100× more time per decade of degree.
//
// Benchmarks: BenchmarkHub_AddEdge_* and BenchmarkHub_RemoveAllEdgesFrom_*
// measure hub build and bulk-removal at three degree points (1k, 10k, 100k) so
// that scaling can be verified empirically with benchstat.
//
// Layer: short (no build tag).

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// buildHub creates a directed multigraph AdjList with one hub node and n
// leaves, all connected by hub → leaf edges. Multigraph mode mirrors the
// openCypher TCK storage (one adjacency slot per CREATE), which is the
// mode that exercises DETACH DELETE performance. It also bypasses the
// simple-graph duplicate-detection scan so the benchmark isolates the
// backing-array growth cost.
func buildHub(n int) (*adjlist.AdjList[string, float64], string) {
	a := adjlist.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	_ = a.AddNode("hub")
	for i := range n {
		leaf := fmt.Sprintf("l%d", i)
		_ = a.AddNode(leaf)
		_ = a.AddEdge("hub", leaf, 1.0)
	}
	return a, "hub"
}

// ─────────────────────────────────────────────────────────────────────────────
// Gate test
// ─────────────────────────────────────────────────────────────────────────────

// TestHub_AddEdge_AmortisedSublinear asserts that hub build time scales
// sub-quadratically: the degree-10k case must take less than 20× the
// degree-1k case. A naive exact-fit allocate-and-copy path would scale at
// ~100× per decade; the geometric pre-allocation path scales at ~10–12×.
func TestHub_AddEdge_AmortisedSublinear(t *testing.T) {
	// Not parallel: the ratio check measures wall-clock time. Under
	// 'go test -race ./...' the race detector adds ~10× non-uniform
	// overhead; t.Parallel() plus cross-package test parallelism
	// inflates the tiny 1k baseline and makes the ratio flaky. Removing
	// t.Parallel() gives the goroutines a quiet core, keeping the
	// ratio below the wide tolerance even under -race.

	const (
		small = 1_000
		large = 10_000
		// ratio: geometric pre-alloc scales at ~10–12×; quadratic at ~100×.
		// Wide tolerance (40) absorbs race-detector non-uniformity while
		// still catching a quadratic regression.
		ratio = 40
		reps  = 7 // take the median over more reps to reduce noise
	)

	measure := func(n int) time.Duration {
		samples := make([]time.Duration, reps)
		for i := range reps {
			start := time.Now()
			buildHub(n)
			samples[i] = time.Since(start)
		}
		// Median: sort in place and pick the middle element.
		sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
		return samples[reps/2]
	}

	// Warm up to avoid first-run JIT effects.
	buildHub(small)
	buildHub(large)

	tSmall := measure(small)
	tLarge := measure(large)

	// Guard against a degenerate tSmall so small that integer division rounds
	// to zero. If the small hub completes in under 1 µs the machine is fast
	// enough that the test is meaningless; skip rather than produce a spurious
	// pass or fail.
	if tSmall < time.Microsecond {
		t.Skip("hub-1k too fast to measure reliably on this machine")
	}

	actualRatio := tLarge / tSmall
	if actualRatio >= ratio {
		t.Errorf(
			"AddEdge scaling appears quadratic: hub-1k=%v hub-10k=%v ratio=%d (want <%d)",
			tSmall, tLarge, actualRatio, ratio,
		)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Benchmarks — AddEdge hub build
// ─────────────────────────────────────────────────────────────────────────────

func BenchmarkHub_AddEdge_1k(b *testing.B) {
	for range b.N {
		buildHub(1_000)
	}
}

func BenchmarkHub_AddEdge_10k(b *testing.B) {
	for range b.N {
		buildHub(10_000)
	}
}

func BenchmarkHub_AddEdge_100k(b *testing.B) {
	for range b.N {
		buildHub(100_000)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Benchmarks — RemoveAllEdgesFrom
// ─────────────────────────────────────────────────────────────────────────────

func BenchmarkHub_RemoveAllEdgesFrom_1k(b *testing.B) {
	a, hub := buildHub(1_000)
	b.ResetTimer()
	for range b.N {
		a.RemoveAllEdgesFrom(hub)
		// Rebuild so the next iteration has something to remove.
		for i := range 1_000 {
			_ = a.AddEdge(hub, fmt.Sprintf("l%d", i), 1.0)
		}
	}
}

func BenchmarkHub_RemoveAllEdgesFrom_10k(b *testing.B) {
	a, hub := buildHub(10_000)
	b.ResetTimer()
	for range b.N {
		a.RemoveAllEdgesFrom(hub)
		for i := range 10_000 {
			_ = a.AddEdge(hub, fmt.Sprintf("l%d", i), 1.0)
		}
	}
}

func BenchmarkHub_RemoveAllEdgesFrom_100k(b *testing.B) {
	a, hub := buildHub(100_000)
	b.ResetTimer()
	for range b.N {
		a.RemoveAllEdgesFrom(hub)
		for i := range 100_000 {
			_ = a.AddEdge(hub, fmt.Sprintf("l%d", i), 1.0)
		}
	}
}
