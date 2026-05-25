package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestKruskalPrim_Divergence verifies that Kruskal and Prim agree on
// total MST cost even when tie-breaking produces structurally different
// trees. The test graph is C6 with all-equal weights (1.0): any
// spanning tree of 6 vertices has exactly 5 edges, so both algorithms
// must return total = 5.0, though they may select different edge sets.
func TestKruskalPrim_Divergence(t *testing.T) {
	t.Parallel()
	const n = 6
	// C6: 0-1-2-3-4-5-0 with weight 1.0 on every edge.
	// This graph has exactly two distinct spanning trees reachable by
	// different tie-breaking strategies (every edge has the same weight),
	// giving both algorithms freedom to diverge on edge selection while
	// still having the same total cost.
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	for i := 0; i < n; i++ {
		next := (i + 1) % n
		if err := a.AddEdge(i, next, 1.0); err != nil {
			t.Fatalf("AddEdge(%d,%d): %v", i, next, err)
		}
	}
	c := csr.BuildFromAdjList(a)

	kEdges, kTotal, err := KruskalMST[float64](c)
	if err != nil {
		t.Fatalf("KruskalMST: %v", err)
	}
	if kTotal != float64(n-1) {
		t.Fatalf("KruskalMST total = %g, want %g", kTotal, float64(n-1))
	}
	if len(kEdges) != n-1 {
		t.Fatalf("KruskalMST edge count = %d, want %d", len(kEdges), n-1)
	}

	src, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatal("Lookup(0) failed")
	}
	_, _, pTotal, err := PrimMST[float64](c, src)
	if err != nil {
		t.Fatalf("PrimMST: %v", err)
	}
	if pTotal != float64(n-1) {
		t.Fatalf("PrimMST total = %g, want %g", pTotal, float64(n-1))
	}
	if kTotal != pTotal {
		t.Fatalf("Kruskal total = %g, Prim total = %g: totals must match", kTotal, pTotal)
	}
}
