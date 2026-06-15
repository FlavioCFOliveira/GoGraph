package bulk

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// benchEdges builds a deterministic edge stream for the load benchmarks.
func benchEdges(n, nNodes int) []Edge {
	rng := rand.New(rand.NewPCG(20260615, 11)) //nolint:gosec // deterministic bench RNG
	es := make([]Edge, n)
	for i := range es {
		es[i] = Edge{
			Src:    fmt.Sprintf("n-%d", rng.IntN(nNodes)),
			Dst:    fmt.Sprintf("n-%d", rng.IntN(nNodes)),
			Weight: int64(i),
		}
	}
	return es
}

// runLoad drives one full in-memory load+CSR build (no csrfile output,
// to isolate ingest/build cost from disk I/O).
func runLoad(b *testing.B, edges []Edge, opts Options) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := New(opts)
		if err := l.AddBatch(edges); err != nil {
			b.Fatalf("AddBatch: %v", err)
		}
		if _, _, err := l.Finalise(); err != nil {
			b.Fatalf("Finalise: %v", err)
		}
	}
}

// BenchmarkLoad_Large_Baseline is the pre-existing behaviour: sequential
// ingest, no pre-size hint (MaxRows unset).
func BenchmarkLoad_Large_Baseline(b *testing.B) {
	edges := benchEdges(500_000, 50_000)
	runLoad(b, edges, Options{Directed: true})
}

// BenchmarkLoad_Large_Presize isolates the calibrated pre-size win:
// sequential ingest with ExpectNodes set to the true distinct-node count
// as a capacity hint for the interning table.
func BenchmarkLoad_Large_Presize(b *testing.B) {
	edges := benchEdges(500_000, 50_000)
	runLoad(b, edges, Options{Directed: true, ExpectNodes: 50_000})
}

// BenchmarkLoad_Large_Parallel measures the partitioned-parallel build
// on top of the calibrated pre-size hint — the full optimisation.
func BenchmarkLoad_Large_Parallel(b *testing.B) {
	edges := benchEdges(500_000, 50_000)
	runLoad(b, edges, Options{Directed: true, ExpectNodes: 50_000, MaxRows: len(edges), Parallel: true})
}

// BenchmarkLoad_Small_Sequential and BenchmarkLoad_Small_Parallel guard
// the small-load no-regression requirement: a Parallel load below the
// threshold must fall back to the sequential build and not pay
// goroutine overhead.
func BenchmarkLoad_Small_Sequential(b *testing.B) {
	edges := benchEdges(2_000, 500)
	runLoad(b, edges, Options{Directed: true, ExpectNodes: 500})
}

func BenchmarkLoad_Small_Parallel(b *testing.B) {
	edges := benchEdges(2_000, 500)
	runLoad(b, edges, Options{Directed: true, ExpectNodes: 500, MaxRows: len(edges), Parallel: true})
}
