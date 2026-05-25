package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestMST_Forest verifies Kruskal on a disconnected graph with K=3
// components and n=20 vertices. The minimum spanning forest must have
// n-K = 17 edges and span all three components.
//
// Component A: path 0–1–2–3–4–5–6–7 (8 vertices; edge i→i+1 has weight i+1)
// Component B: star, centre=8, leaves=9,10,11,12 (5 vertices; all weights 1.0)
// Component C: complete K7 on vertices 13–19 (7 vertices; edge weights src+dst)
func TestMST_Forest(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: false})

	// Component A: path 0-1-2-3-4-5-6-7
	for i := 0; i < 7; i++ {
		if err := a.AddEdge(i, i+1, float64(i+1)); err != nil {
			t.Fatalf("AddEdge A (%d,%d): %v", i, i+1, err)
		}
	}

	// Component B: star with centre 8, leaves 9-12
	for leaf := 9; leaf <= 12; leaf++ {
		if err := a.AddEdge(8, leaf, 1.0); err != nil {
			t.Fatalf("AddEdge B (8,%d): %v", leaf, err)
		}
	}

	// Component C: complete K7 on vertices 13-19
	for i := 13; i <= 19; i++ {
		for j := i + 1; j <= 19; j++ {
			if err := a.AddEdge(i, j, float64(i+j)); err != nil {
				t.Fatalf("AddEdge C (%d,%d): %v", i, j, err)
			}
		}
	}

	c := csr.BuildFromAdjList(a)
	mst, _, err := KruskalMST[float64](c)
	if err != nil {
		t.Fatalf("KruskalMST: %v", err)
	}

	// n=20 vertices, K=3 components → MSF must have 20-3=17 edges.
	const wantEdges = 20 - 3
	if len(mst) != wantEdges {
		t.Fatalf("MSF edge count = %d, want %d (n-K)", len(mst), wantEdges)
	}

	// Each component's MST cardinality:
	// A: path(8) → 7 edges (all edges already a tree)
	// B: star(5) → 4 edges (all edges already a tree)
	// C: K7 → 6 edges
	// Total: 7+4+6 = 17. Already verified above.
}
