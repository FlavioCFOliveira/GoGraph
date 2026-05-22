package search

import (
	"errors"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestHierholzerUndirected_K4_NoEulerian(t *testing.T) {
	t.Parallel()
	// K4: every vertex has degree 3 (odd). No Eulerian path/circuit.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			if err := a.AddEdge(i, j, struct{}{}); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
	}
	c := csr.BuildFromAdjList(a)
	_, err := HierholzerUndirected(c)
	if !errors.Is(err, ErrNoEulerian) {
		t.Fatalf("K4 should have no Eulerian path: %v", err)
	}
}

func TestHierholzerUndirected_Cycle(t *testing.T) {
	t.Parallel()
	// 4-cycle: every vertex has even degree. Eulerian circuit exists.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, (i+1)%4, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	trail, err := HierholzerUndirected(c)
	if err != nil {
		t.Fatalf("4-cycle: %v", err)
	}
	if len(trail) != 5 {
		t.Fatalf("trail length = %d, want 5 (4 edges + 1)", len(trail))
	}
	if trail[0] != trail[len(trail)-1] {
		t.Fatalf("Eulerian circuit must close: %v", trail)
	}
}

func TestHierholzerUndirected_PathEndpoints(t *testing.T) {
	t.Parallel()
	// Path 0-1-2-3: vertices 0 and 3 have degree 1 (odd),
	// vertices 1 and 2 have degree 2 (even). Eulerian path exists.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 3; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	trail, err := HierholzerUndirected(c)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if len(trail) != 4 {
		t.Fatalf("trail length = %d, want 4", len(trail))
	}
}
