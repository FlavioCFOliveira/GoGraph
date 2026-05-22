package search

import (
	"math/rand/v2"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestKruskalMST_CLRS(t *testing.T) {
	t.Parallel()
	// CLRS-style undirected graph (fig. 23.1). Vertices 0..8.
	// Expected MST weight = 37.
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
		if err := a.AddEdge(e.u, e.v, e.w); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	mst, total, err := KruskalMST(c)
	if err != nil {
		t.Fatalf("KruskalMST: %v", err)
	}
	if total != 37 {
		t.Fatalf("MST weight = %d, want 37", total)
	}
	if len(mst) != 8 {
		t.Fatalf("MST edges = %d, want 8 (n-1 for n=9)", len(mst))
	}
}

func TestKruskalMST_Disconnected(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	if err := a.AddEdge(0, 1, 5); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 3, 7); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	mst, total, err := KruskalMST(c)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if total != 12 {
		t.Fatalf("MST weight = %d, want 12 (5+7)", total)
	}
	if len(mst) != 2 {
		t.Fatalf("MST edges = %d, want 2", len(mst))
	}
}

// TestKruskalMST_RandomCardinality fuzzes Kruskal on connected random
// graphs and asserts the resulting forest has exactly n-1 edges.
func TestKruskalMST_RandomCardinality(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(137, 139)) //nolint:gosec // deterministic
	for seed := 0; seed < 10; seed++ {
		const n = 32
		a := adjlist.New[int, int64](adjlist.Config{Directed: false})
		// Spanning chain so the graph is guaranteed connected.
		for i := 0; i < n-1; i++ {
			if err := a.AddEdge(i, i+1, int64(r.IntN(100)+1)); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		// Plus extra random edges.
		for i := 0; i < 4*n; i++ {
			if err := a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(100)+1)); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		mst, _, err := KruskalMST(c)
		if err != nil {
			t.Fatalf("seed=%d: %v", seed, err)
		}
		if len(mst) != n-1 {
			t.Fatalf("seed=%d: MST edges = %d, want %d", seed, len(mst), n-1)
		}
	}
}

func BenchmarkKruskalMST_RandomGraph(b *testing.B) {
	const n = 4096
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	r := rand.New(rand.NewPCG(149, 151)) //nolint:gosec // deterministic
	for i := 0; i < 4*n; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(100)+1)); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = KruskalMST(c)
	}
}
