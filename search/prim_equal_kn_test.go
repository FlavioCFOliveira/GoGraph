package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestPrimMST_EqualWeight verifies PrimMST on K6 with all edge weights
// equal to 1.0. The MST must span all 6 vertices with n-1=5 edges and
// total weight 5.0. Kruskal is also run for cost comparison; both
// algorithms may produce structurally different (but equally-weighted)
// spanning trees on this symmetric graph.
func TestPrimMST_EqualWeight(t *testing.T) {
	t.Parallel()
	const n = 6
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if err := a.AddEdge(i, j, 1.0); err != nil {
				t.Fatalf("AddEdge(%d,%d): %v", i, j, err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)

	src, ok := a.Mapper().Lookup(0)
	if !ok {
		t.Fatal("Lookup(0) failed")
	}

	_, found, total, err := PrimMST[float64](c, src)
	if err != nil {
		t.Fatalf("PrimMST: %v", err)
	}
	if total != float64(n-1) {
		t.Fatalf("PrimMST total weight = %g, want %g", total, float64(n-1))
	}

	// All n vertices must be reachable from src.
	reachCount := 0
	for i := range found {
		if found[i] {
			reachCount++
		}
	}
	if reachCount != n {
		t.Fatalf("PrimMST reached %d vertices, want %d", reachCount, n)
	}
	// n reached vertices → n-1 tree edges (root has no incoming tree edge).
	if reachCount-1 != n-1 {
		t.Fatalf("Prim MST edge count = %d, want %d", reachCount-1, n-1)
	}

	// Kruskal must agree on total cost.
	_, kTotal, err := KruskalMST[float64](c)
	if err != nil {
		t.Fatalf("KruskalMST: %v", err)
	}
	if kTotal != total {
		t.Fatalf("KruskalMST total = %g, PrimMST total = %g: mismatch", kTotal, total)
	}
}
