package search

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// BenchmarkBiBFS_PowerLawSymmetric runs BiBFSCtx on the BuildSymmetric
// projection of a 20k-vertex Barabasi-Albert graph, the undirected input
// for which BiBFSCtx walks c in both directions.
func BenchmarkBiBFS_PowerLawSymmetric(b *testing.B) {
	g, err := shapegen.BarabasiAlbert(20000, 4, 11).Build(adjlist.Config{})
	if err != nil {
		b.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList()).BuildSymmetric()
	n := graph.NodeID(c.MaxNodeID())
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		src := graph.NodeID(uint64(i)*7919) % n
		dst := (graph.NodeID(uint64(i)*104729) + n/2) % n
		if _, err := BiBFSCtx(ctx, c, src, dst); err != nil && !errors.Is(err, ErrNoPath) {
			b.Fatalf("BiBFSCtx(%d,%d): %v", src, dst, err)
		}
	}
}
