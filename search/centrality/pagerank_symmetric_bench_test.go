package centrality

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// BenchmarkPageRank_PowerLaw50KSymmetric runs one-shot PageRank on the
// BuildSymmetric projection of the BenchmarkPageRank_PowerLaw50K fixture —
// the input every undirected analytic consumes. Under GOMAXPROCS>1 it takes
// the parallel pull path, which reads the in-edge structure of the CSR.
func BenchmarkPageRank_PowerLaw50KSymmetric(b *testing.B) {
	g, err := shapegen.BarabasiAlbert(50000, 8, 7).Build(adjlist.Config{})
	if err != nil {
		b.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList()).BuildSymmetric()
	opts := PageRankOptions{Damping: 0.85, MaxIterations: 30, Tolerance: 1e-6}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = PageRank(c, opts)
	}
}
