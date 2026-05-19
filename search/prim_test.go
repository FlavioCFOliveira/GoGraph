package search

import (
	"math/rand/v2"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestPrimMST_CLRS(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	type ueg struct {
		u, v int
		w    int64
	}
	edges := []ueg{
		{0, 1, 4}, {0, 7, 8},
		{1, 2, 8}, {1, 7, 11},
		{2, 3, 7}, {2, 5, 4}, {2, 8, 2},
		{3, 4, 9}, {3, 5, 14},
		{4, 5, 10},
		{5, 6, 2},
		{6, 7, 1}, {6, 8, 6},
		{7, 8, 7},
	}
	for _, e := range edges {
		a.AddEdge(e.u, e.v, e.w)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	_, _, total, err := PrimMST(c, src)
	if err != nil {
		t.Fatalf("PrimMST: %v", err)
	}
	if total != 37 {
		t.Fatalf("PrimMST weight = %d, want 37", total)
	}
}

// TestPrimMST_VsKruskal asserts that Prim and Kruskal yield trees of
// equal total weight on random connected graphs (the MST weight is
// uniquely determined; the trees themselves may differ on tied
// weights, but the cost cannot).
func TestPrimMST_VsKruskal(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(157, 163)) //nolint:gosec // deterministic
	for seed := 0; seed < 10; seed++ {
		const n = 32
		a := adjlist.New[int, int64](adjlist.Config{Directed: false})
		// Spanning chain so the graph is guaranteed connected.
		for i := 0; i < n-1; i++ {
			a.AddEdge(i, i+1, int64(r.IntN(50)+1))
		}
		for i := 0; i < 4*n; i++ {
			a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(50)+1))
		}
		c := csr.BuildFromAdjList(a)
		src, _ := a.Mapper().Lookup(0)
		_, _, primTotal, err := PrimMST(c, src)
		if err != nil {
			t.Fatalf("seed=%d PrimMST: %v", seed, err)
		}
		_, kruskalTotal, err := KruskalMST(c)
		if err != nil {
			t.Fatalf("seed=%d KruskalMST: %v", seed, err)
		}
		if primTotal != kruskalTotal {
			t.Fatalf("seed=%d: PrimMST=%d KruskalMST=%d", seed, primTotal, kruskalTotal)
		}
	}
}

func BenchmarkPrimMST_RandomGraph(b *testing.B) {
	const n = 4096
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	r := rand.New(rand.NewPCG(167, 173)) //nolint:gosec // deterministic
	for i := 0; i < 4*n; i++ {
		a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(100)+1))
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _, _ = PrimMST(c, src)
	}
}
