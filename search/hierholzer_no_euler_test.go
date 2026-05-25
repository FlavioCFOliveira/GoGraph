package search

import (
	"errors"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestHierholzerUndirected_NoEulerian verifies that HierholzerUndirected
// returns ErrNoEulerian for graphs that have no Eulerian circuit or path.
func TestHierholzerUndirected_NoEulerian(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		edges [][2]int
	}{
		{
			// Star K_{1,3}: centre 0 has degree 3; leaves 1,2,3 have
			// degree 1. Four odd-degree vertices — no Eulerian circuit
			// or path (would require exactly 0 or 2 odd-degree vertices).
			name:  "star_K1_3",
			edges: [][2]int{{0, 1}, {0, 2}, {0, 3}},
		},
		{
			// Complete K4 minus one edge (0,1,2,3 — drop 2-3):
			// remaining: 0-1, 0-2, 0-3, 1-2, 1-3.
			// Degrees: 0→3, 1→3, 2→2, 3→2 — two odd-degree vertices,
			// so an Eulerian path exists but NOT an Eulerian circuit.
			// HierholzerUndirected accepts Eulerian paths, so this
			// graph should succeed; test it does not return ErrNoEulerian.
			// (Excluded from the error cases — see below.)
		},
		{
			// Pentagon (C5) plus a chord that creates 4 odd-degree vertices:
			// edges 0-1, 1-2, 2-3, 3-4, 4-0, 0-2.
			// Degrees: 0→3, 1→2, 2→3, 3→2, 4→2 — vertices 0 and 2
			// are odd: exactly 2 → Eulerian path exists (no error).
			// Also excluded; use a different structure.
		},
	}
	// Only the star_K1_3 case has 4 odd-degree vertices and should error.
	for _, tc := range tests {
		if len(tc.edges) == 0 {
			continue
		}
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := adjlist.New[int, int64](adjlist.Config{Directed: false})
			for _, e := range tc.edges {
				if err := a.AddEdge(e[0], e[1], int64(1)); err != nil {
					t.Fatalf("AddEdge(%d,%d): %v", e[0], e[1], err)
				}
			}
			c := csr.BuildFromAdjList(a)
			_, err := HierholzerUndirected(c)
			if !errors.Is(err, ErrNoEulerian) {
				t.Fatalf("%s: expected ErrNoEulerian, got %v", tc.name, err)
			}
		})
	}

	// Wheel W4: centre 0 connected to all of 1,2,3,4; rim 1-2-3-4-1.
	// Degrees: 0→4, 1→3, 2→3, 3→3, 4→3 — four odd-degree vertices.
	t.Run("wheel_W4", func(t *testing.T) {
		t.Parallel()
		a := adjlist.New[int, int64](adjlist.Config{Directed: false})
		rimEdges := [][2]int{{1, 2}, {2, 3}, {3, 4}, {4, 1}}
		spokeEdges := [][2]int{{0, 1}, {0, 2}, {0, 3}, {0, 4}}
		for _, e := range append(rimEdges, spokeEdges...) {
			if err := a.AddEdge(e[0], e[1], int64(1)); err != nil {
				t.Fatalf("AddEdge(%d,%d): %v", e[0], e[1], err)
			}
		}
		c := csr.BuildFromAdjList(a)
		_, err := HierholzerUndirected(c)
		if !errors.Is(err, ErrNoEulerian) {
			t.Fatalf("wheel W4 has 4 odd-degree vertices: expected ErrNoEulerian, got %v", err)
		}
	})
}
