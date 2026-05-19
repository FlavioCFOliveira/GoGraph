// Package comparison hosts the GoGraph half of the cross-library
// comparison benchmark suite (task #159). The graph topology is
// identical to bench/comparison/networkx_baseline.py so the
// resulting numbers can be compared apples-to-apples.
package comparison

import (
	"math/rand/v2"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search"
	"gograph/search/centrality"
)

const (
	cmpN = 1 << 14 // 16k nodes
	cmpE = 4 * cmpN
)

func buildComparisonGraph() (c *csr.CSR[int64], src graph.NodeID) {
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	r := rand.New(rand.NewPCG(31, 1)) //nolint:gosec // deterministic seed
	for i := 0; i < cmpN; i++ {
		a.AddNode(i)
	}
	for i := 0; i < cmpE; i++ {
		a.AddEdge(r.IntN(cmpN), r.IntN(cmpN), int64(r.IntN(100)+1))
	}
	c = csr.BuildFromAdjList(a)
	src, _ = a.Mapper().Lookup(0)
	return c, src
}

func BenchmarkComparison_BFS(b *testing.B) {
	c, src := buildComparisonGraph()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		search.BFS(c, src, func(_ graph.NodeID, _ int) bool { return true })
	}
}

func BenchmarkComparison_Dijkstra(b *testing.B) {
	c, src := buildComparisonGraph()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = search.Dijkstra(c, src)
	}
}

func BenchmarkComparison_PageRank(b *testing.B) {
	c, _ := buildComparisonGraph()
	opts := centrality.PageRankOptions{Damping: 0.85, MaxIterations: 30, Tolerance: 1e-6}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = centrality.PageRank(c, opts)
	}
}
