package centrality

import (
	"math"
	"math/rand/v2"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestBetweenness_Path(t *testing.T) {
	t.Parallel()
	// Undirected path 0-1-2-3-4. Centre node 2 has the highest
	// betweenness; nodes 0/4 are zero.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	bc := Betweenness(c)
	id0, _ := a.Mapper().Lookup(0)
	id2, _ := a.Mapper().Lookup(2)
	id4, _ := a.Mapper().Lookup(4)
	if bc[uint64(id0)] != 0 || bc[uint64(id4)] != 0 {
		t.Fatalf("endpoint betweenness must be 0")
	}
	if bc[uint64(id2)] <= bc[uint64(id0)] {
		t.Fatalf("centre betweenness must exceed endpoints")
	}
}

// BenchmarkBrandes_RandomGraph measures the steady-state allocation
// profile of Betweenness on a moderate undirected random graph. The
// brandesSource per-source loop is the hot path; task #126 requires
// allocs/op to drop by >=50% versus the v1.0 implementation, achieved
// by lifting the queue and stack allocations to the outer loop.
func BenchmarkBrandes_RandomGraph(b *testing.B) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	const n = 512
	r := rand.New(rand.NewPCG(19, 23)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < 3*n; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), struct{}{}); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Betweenness(c)
	}
}

func TestBetweenness_Star(t *testing.T) {
	t.Parallel()
	// Star: hub 0 connected to 1..4. Hub has max betweenness;
	// every leaf has 0.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 1; i <= 4; i++ {
		if err := a.AddEdge(0, i, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	bc := Betweenness(c)
	hub, _ := a.Mapper().Lookup(0)
	if math.Abs(bc[uint64(hub)]-12) > 1e-9 { // 4*3 ordered pairs through hub
		t.Fatalf("hub betweenness = %f, want 12", bc[uint64(hub)])
	}
}
