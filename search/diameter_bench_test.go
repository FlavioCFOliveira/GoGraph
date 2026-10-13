package search

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// benchDiameterCycle builds an undirected cycle of n vertices. Every vertex
// of a cycle has the same eccentricity, so DiameterCtx sweeps from every
// vertex: the worst case of BoundingDiameters.
func benchDiameterCycle(b *testing.B, n int) *csr.CSR[struct{}] {
	b.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{})
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, struct{}{}); err != nil {
			b.Fatalf("AddEdge(%d,%d): %v", i, (i+1)%n, err)
		}
	}
	return csr.BuildFromAdjList(a).BuildSymmetric()
}

// benchDiameterSpider builds a hub with legs of equal length.
func benchDiameterSpider(b *testing.B, legs, length int) *csr.CSR[struct{}] {
	b.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{})
	key := 1
	for l := 0; l < legs; l++ {
		prev := 0
		for i := 0; i < length; i++ {
			if err := a.AddEdge(prev, key, struct{}{}); err != nil {
				b.Fatalf("AddEdge(%d,%d): %v", prev, key, err)
			}
			prev = key
			key++
		}
	}
	return csr.BuildFromAdjList(a).BuildSymmetric()
}

// BenchmarkDiameter_CycleSerialLevels measures a 600-vertex cycle, below
// diameterParallelMinComponent, so every sweep runs serially. The name is
// kept from the iFUB implementation so benchstat pairs old and new runs.
func BenchmarkDiameter_CycleSerialLevels(b *testing.B) {
	c := benchDiameterCycle(b, 600)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lo, hi, exact, err := DiameterCtx(ctx, c)
		if err != nil || !exact || lo != 300 || hi != 300 {
			b.Fatalf("DiameterCtx = (%d, %d, %t), err=%v", lo, hi, exact, err)
		}
	}
}

// BenchmarkDiameter_SpiderParallelLevels measures a 16-leg spider. The name
// is kept from the iFUB implementation, whose level walk ran this shape on
// its parallel arm, so benchstat pairs old and new runs.
func BenchmarkDiameter_SpiderParallelLevels(b *testing.B) {
	c := benchDiameterSpider(b, 16, 20)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lo, hi, exact, err := DiameterCtx(ctx, c)
		if err != nil || !exact || lo != 40 || hi != 40 {
			b.Fatalf("DiameterCtx = (%d, %d, %t), err=%v", lo, hi, exact, err)
		}
	}
}

// benchDiameterSocial builds a deterministic n-vertex small-world graph:
// a Barabási–Albert core (m = 3, half the endpoints chosen by preferential
// attachment) plus a pendant chain of chainLen vertices hung from a random
// vertex every 512, which stretches the diameter the way example 11's
// low-degree fringe does.
func benchDiameterSocial(b *testing.B, n int) *csr.CSR[struct{}] {
	b.Helper()
	r := rand.New(rand.NewPCG(11, uint64(n))) //nolint:gosec // deterministic bench RNG
	a := adjlist.New[int, struct{}](adjlist.Config{})
	ends := make([]int, 0, 6*n)
	for v := 1; v < n; v++ {
		for j := 0; j < min(3, v); j++ {
			u := r.IntN(v)
			if len(ends) > 0 && r.IntN(2) == 0 {
				u = ends[r.IntN(len(ends))]
			}
			if err := a.AddEdge(u, v, struct{}{}); err != nil {
				b.Fatalf("AddEdge(%d,%d): %v", u, v, err)
			}
			ends = append(ends, u, v)
		}
	}
	const chainLen = 4
	key := n
	for v := 0; v < n; v += 512 {
		prev := r.IntN(n)
		for i := 0; i < chainLen; i++ {
			if err := a.AddEdge(prev, key, struct{}{}); err != nil {
				b.Fatalf("AddEdge(%d,%d): %v", prev, key, err)
			}
			prev = key
			key++
		}
	}
	return csr.BuildFromAdjList(a).BuildSymmetric()
}

// BenchmarkDiameter_Social20k measures a 20k-vertex small-world graph, the
// order of example 11's mid-scale run, on DiameterCtx's default worker
// selection.
func BenchmarkDiameter_Social20k(b *testing.B) {
	c := benchDiameterSocial(b, 20000)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lo, hi, exact, err := DiameterCtx(ctx, c)
		if err != nil || !exact || lo != hi || lo == 0 {
			b.Fatalf("DiameterCtx = (%d, %d, %t), err=%v", lo, hi, exact, err)
		}
	}
}
