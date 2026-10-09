package csr

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// benchSymmetricSource is the 50k-vertex, 400k-arc weighted adjacency both
// symmetric-projection benchmarks build from.
func benchSymmetricSource(b *testing.B) *adjlist.AdjList[int, float64] {
	b.Helper()
	const n, m = 50000, 400000
	a := adjlist.New[int, float64](adjlist.Config{})
	r := rand.New(rand.NewPCG(3, 5)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < m; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), r.Float64()); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	return a
}

// BenchmarkSymmetricProjection_OnePass measures BuildSymmetricFromAdjList.
func BenchmarkSymmetricProjection_OnePass(b *testing.B) {
	a := benchSymmetricSource(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildSymmetricFromAdjList(a)
	}
}

// BenchmarkSymmetricProjection_TwoStep measures the equivalent
// BuildFromAdjList(adj).BuildSymmetric(), the reference for the one-pass
// build's documented trade-off.
func BenchmarkSymmetricProjection_TwoStep(b *testing.B) {
	a := benchSymmetricSource(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildFromAdjList(a).BuildSymmetric()
	}
}
