package search

import (
	"sort"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestKruskalMST_EqualWeight verifies KruskalMST on K6 with all edge
// weights equal to 1.0. The MST must have n-1 = 5 edges with total
// weight 5.0. Two successive runs on the same CSR must return
// identical edge lists (determinism under equal weights).
func TestKruskalMST_EqualWeight(t *testing.T) {
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

	edges0, total0, err := KruskalMST[float64](c)
	if err != nil {
		t.Fatalf("KruskalMST (run 1): %v", err)
	}
	if len(edges0) != n-1 {
		t.Fatalf("MST edge count = %d, want %d", len(edges0), n-1)
	}
	if total0 != 5.0 {
		t.Fatalf("MST total weight = %g, want 5.0", total0)
	}

	// Second run — result must be identical (same CSR, deterministic sort).
	edges1, total1, err := KruskalMST[float64](c)
	if err != nil {
		t.Fatalf("KruskalMST (run 2): %v", err)
	}
	if total1 != total0 {
		t.Fatalf("non-deterministic total: run1=%g run2=%g", total0, total1)
	}
	sortEdges := func(es []MSTEdge[float64]) {
		sort.Slice(es, func(i, j int) bool {
			if es[i].From != es[j].From {
				return es[i].From < es[j].From
			}
			return es[i].To < es[j].To
		})
	}
	sortEdges(edges0)
	sortEdges(edges1)
	for i := range edges0 {
		if edges0[i] != edges1[i] {
			t.Fatalf("non-deterministic edge[%d]: run1=%v run2=%v", i, edges0[i], edges1[i])
		}
	}

	// All returned edges must connect distinct CSR NodeIDs.
	seen := make(map[graph.NodeID]bool)
	for _, e := range edges0 {
		seen[e.From] = true
		seen[e.To] = true
	}
	if len(seen) != n {
		t.Fatalf("MST does not span all %d vertices (saw %d)", n, len(seen))
	}
}
