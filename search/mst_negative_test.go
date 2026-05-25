package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestMST_NegativeWeights verifies that both KruskalMST and PrimMST
// correctly handle negative edge weights and produce the same total
// MST cost. The hand-crafted graph has 5 vertices and mixed-sign
// weights; the expected MST is computed analytically.
//
// Edges and weights:
//
//	0-1: -3
//	0-2:  1
//	1-2: -1
//	1-3:  2
//	2-3:  0
//	2-4:  1
//	3-4: -1
//	0-4:  2
//
// Kruskal selection (edges sorted ascending):
//
//	-3 (0-1): joins {0,1}
//	-1 (1-2): joins {0,1,2}
//	-1 (3-4): joins {3,4}
//	 0 (2-3): joins {0,1,2,3,4}  ← completes the spanning tree
//
// Total: -3 + (-1) + (-1) + 0 = -5
func TestMST_NegativeWeights(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	type wEdge struct {
		u, v int
		w    float64
	}
	edges := []wEdge{
		{0, 1, -3},
		{0, 2, 1},
		{1, 2, -1},
		{1, 3, 2},
		{2, 3, 0},
		{2, 4, 1},
		{3, 4, -1},
		{0, 4, 2},
	}
	for _, e := range edges {
		if err := a.AddEdge(e.u, e.v, e.w); err != nil {
			t.Fatalf("AddEdge(%d,%d,%.0f): %v", e.u, e.v, e.w, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	const n = 5
	const wantTotal = -5.0

	// Kruskal
	kEdges, kTotal, err := KruskalMST[float64](c)
	if err != nil {
		t.Fatalf("KruskalMST: %v", err)
	}
	if len(kEdges) != n-1 {
		t.Fatalf("KruskalMST edge count = %d, want %d", len(kEdges), n-1)
	}
	if kTotal != wantTotal {
		t.Fatalf("KruskalMST total = %g, want %g", kTotal, wantTotal)
	}

	// Prim (rooted at vertex 0)
	src, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatal("Lookup(0) failed")
	}
	_, _, pTotal, err := PrimMST[float64](c, src)
	if err != nil {
		t.Fatalf("PrimMST: %v", err)
	}
	if pTotal != wantTotal {
		t.Fatalf("PrimMST total = %g, want %g", pTotal, wantTotal)
	}
	if kTotal != pTotal {
		t.Fatalf("Kruskal=%g Prim=%g: MST cost must agree", kTotal, pTotal)
	}
}
