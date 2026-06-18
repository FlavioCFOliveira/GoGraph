package centrality

import (
	"context"
	"math"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// buildLargePowerLaw builds a directed power-law graph large enough to
// cross pageRankParallelThreshold so the parallel pull SpMV path is
// exercised.
func buildLargePowerLaw(t testing.TB, n int) *csr.CSR[int64] {
	t.Helper()
	g, err := shapegen.BarabasiAlbert(n, 4, 99).Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	return csr.BuildFromAdjList(g.AdjList())
}

// TestPageRank_ParallelBitIdentical proves that the parallel pull-
// formulation SpMV produces results bit-identical to the serial push
// path, across GOMAXPROCS = 1 and 8 and across repeated runs. The
// reference is the serial path itself, obtained by forcing GOMAXPROCS=1
// (which disables the parallel branch via the workers > 1 guard).
func TestPageRank_ParallelBitIdentical(t *testing.T) {
	c := buildLargePowerLaw(t, 8000)
	if c.LiveCount() < pageRankParallelThreshold {
		t.Fatalf("test graph too small: live=%d < threshold=%d", c.LiveCount(), pageRankParallelThreshold)
	}
	opts := PageRankOptions{Damping: 0.85, MaxIterations: 50, Tolerance: 1e-9}

	// Reference: serial path (GOMAXPROCS=1 forces the push branch).
	prev := runtime.GOMAXPROCS(1)
	ref, refIters, err := PageRank(c, opts)
	runtime.GOMAXPROCS(prev)
	if err != nil {
		t.Fatalf("serial PageRank: %v", err)
	}

	// Parallel path at several worker counts and repeated runs.
	for _, gomaxprocs := range []int{2, 4, 8} {
		if gomaxprocs > runtime.NumCPU() {
			continue
		}
		for rep := 0; rep < 3; rep++ {
			prev := runtime.GOMAXPROCS(gomaxprocs)
			got, gotIters, err := PageRank(c, opts)
			runtime.GOMAXPROCS(prev)
			if err != nil {
				t.Fatalf("parallel PageRank (GOMAXPROCS=%d): %v", gomaxprocs, err)
			}
			if len(got) != len(ref) {
				t.Fatalf("length mismatch: got %d want %d", len(got), len(ref))
			}
			var bitDiff int
			var maxULP uint64
			for i := range ref {
				if got[i] == ref[i] {
					continue
				}
				bitDiff++
				a := math.Float64bits(got[i])
				b := math.Float64bits(ref[i])
				d := a - b
				if b > a {
					d = b - a
				}
				if d > maxULP {
					maxULP = d
				}
			}
			if bitDiff != 0 {
				t.Fatalf("GOMAXPROCS=%d rep=%d: %d/%d ranks differ from serial reference (maxULP=%d)",
					gomaxprocs, rep, bitDiff, len(ref), maxULP)
			}
			if gotIters != refIters {
				t.Fatalf("GOMAXPROCS=%d rep=%d: iteration count %d != serial %d",
					gomaxprocs, rep, gotIters, refIters)
			}
		}
	}
}

// TestPageRank_ParallelCancellation verifies that the parallel path
// honours ctx cancellation and returns the wrapped error.
func TestPageRank_ParallelCancellation(t *testing.T) {
	c := buildLargePowerLaw(t, 8000)
	prev := runtime.GOMAXPROCS(8)
	defer runtime.GOMAXPROCS(prev)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := PageRankCtx(ctx, c, PageRankOptions{Damping: 0.85, MaxIterations: 50})
	if err == nil {
		t.Fatalf("expected cancellation error, got nil")
	}
}

// BenchmarkPageRank_PowerLaw50K benchmarks PageRank on a directed
// power-law graph large enough to use the parallel pull path. It is the
// guard-band PageRank benchmark; run under GOMAXPROCS=1 it measures the
// serial path (no-regression gate) and under GOMAXPROCS>1 the parallel
// win.
func BenchmarkPageRank_PowerLaw50K(b *testing.B) {
	g, err := shapegen.BarabasiAlbert(50000, 8, 7).Build(adjlist.Config{Directed: true})
	if err != nil {
		b.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	opts := PageRankOptions{Damping: 0.85, MaxIterations: 30, Tolerance: 1e-6}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = PageRank(c, opts)
	}
}

// BenchmarkPageRanker_PowerLaw50K_Repeated measures the per-query
// allocation profile of a reused [PageRanker] on the same fixture as
// BenchmarkPageRank_PowerLaw50K. The PageRanker caches the CSR-derived
// topology and the reverse-CSR transpose, so every iteration after the
// first amortises those one-time allocations away. Task #1592 requires a
// repeated run on the same CSR to allocate materially less than the
// one-shot PageRank (which rebuilds the reverse structure every call).
func BenchmarkPageRanker_PowerLaw50K_Repeated(b *testing.B) {
	g, err := shapegen.BarabasiAlbert(50000, 8, 7).Build(adjlist.Config{Directed: true})
	if err != nil {
		b.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	opts := PageRankOptions{Damping: 0.85, MaxIterations: 30, Tolerance: 1e-6}
	pr := NewPageRanker(c)
	// Warm the lazy reverse-CSR cache so b.N measures the steady state.
	_, _, _ = pr.Run(context.Background(), opts)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = pr.Run(context.Background(), opts)
	}
}
