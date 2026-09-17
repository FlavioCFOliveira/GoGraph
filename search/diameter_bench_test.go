package search

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// benchDiameterCycle builds an undirected cycle of n vertices. On a cycle
// no level improves the 2-sweep bound, so DiameterCtx runs its iFUB
// refinement over every level down to convergence — two full
// eccentricity BFS sweeps per level. It is the shape that charges the
// per-sweep working set the most times per call.
func benchDiameterCycle(b *testing.B, n int) *csr.CSR[struct{}] {
	b.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < n; i++ {
		if err := a.AddEdge(i, (i+1)%n, struct{}{}); err != nil {
			b.Fatalf("AddEdge(%d,%d): %v", i, (i+1)%n, err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// benchDiameterSpider builds a hub with legs of equal length. Each BFS
// level of the refinement walk holds one vertex per leg, so with enough
// legs the walk takes the parallel per-worker arm.
func benchDiameterSpider(b *testing.B, legs, length int) *csr.CSR[struct{}] {
	b.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
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
	return csr.BuildFromAdjList(a)
}

// BenchmarkDiameter_CycleSerialLevels drives the serial arm of the
// refinement walk (two vertices per level, below
// diameterParallelMinLevel), which is where the per-sweep working set is
// charged once per BFS source.
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

// BenchmarkDiameter_SpiderParallelLevels drives the per-worker arm: each
// level holds one vertex per leg, so a 16-leg spider clears
// diameterParallelMinLevel.
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
