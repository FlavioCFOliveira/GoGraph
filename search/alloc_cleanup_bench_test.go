package search

import (
	"context"
	"testing"
)

// BenchmarkKShortestPathsLoopless drives the loopless k-shortest-paths search
// over a layered DAG dense enough to keep a large heap frontier, so the queue
// push/pop path dominates. The priority queue is now a monomorphic heap
// (pushItem/popItem) rather than container/heap, so each push no longer boxes
// the path-carrying item into an any.
func BenchmarkKShortestPathsLoopless(b *testing.B) {
	const layers, width = 6, 6
	id := func(l, w int) int { return l*width + w }
	var edges []weightedEdge
	for l := 0; l < layers-1; l++ {
		for a := 0; a < width; a++ {
			for d := 0; d < width; d++ {
				edges = append(edges, weightedEdge{id(l, a), id(l+1, d), int64((a+d)%7 + 1)})
			}
		}
	}
	c, adj := buildWeightedCSR(b, edges)
	src, _ := adj.Mapper().Lookup(id(0, 0))
	dst, _ := adj.Mapper().Lookup(id(layers-1, width-1))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = KShortestPathsLoopless(c, src, dst, 20)
	}
}

// BenchmarkHungarian drives the rectangular assignment solver on a dense
// square cost matrix. The per-row augmenting-path scratch (minv/used) is now
// allocated once and reset per row rather than reallocated, so the inner-loop
// allocation no longer scales with the matrix dimension.
func BenchmarkHungarian(b *testing.B) {
	const n = 80
	cost := make([]float64, n*n)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			cost[i*n+j] = float64((i*7 + j*13) % 97)
		}
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := HungarianCtx(ctx, cost, n, n); err != nil {
			b.Fatalf("HungarianCtx: %v", err)
		}
	}
}
