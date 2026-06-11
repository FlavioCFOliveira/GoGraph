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
	t.Parallel()

	const (
		small = 1_000
		large = 10_000
		ratio = 20 // maximum tolerated ratio: anything < this confirms sub-quadratic
		reps  = 5  // repeat each measurement and take the minimum to reduce noise
	)

	measure := func(n int) time.Duration {
		best := time.Duration(1<<62 - 1)
		for range reps {
			start := time.Now()
			buildHub(n)
			elapsed := time.Since(start)
			if elapsed < best {
				best = elapsed
			}
		}
		return best
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
