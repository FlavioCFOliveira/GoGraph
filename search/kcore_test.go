package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestKCore_Clique(t *testing.T) {
	t.Parallel()
	// K5: every vertex has coreness 4.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 5; i++ {
		for j := i + 1; j < 5; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	c := csr.BuildFromAdjList(a)
	coreness := KCore(c)
	for i := 0; i < 5; i++ {
		id, _ := a.Mapper().Lookup(i)
		if coreness[id] != 4 {
			t.Fatalf("K5: coreness[%d] = %d, want 4", i, coreness[id])
		}
	}
}

func TestKCore_Path(t *testing.T) {
	t.Parallel()
	// Path 0-1-2-3-4: every vertex belongs to the 1-core. Each
	// has degree 1 (endpoints) or 2 (interior), but peeling drops
	// all to coreness 1.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		a.AddEdge(i, i+1, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	coreness := KCore(c)
	for i := 0; i < 5; i++ {
		id, _ := a.Mapper().Lookup(i)
		if coreness[id] != 1 {
			t.Fatalf("Path: coreness[%d] = %d, want 1", i, coreness[id])
		}
	}
}

// TestKCore_Mixed exercises a graph that combines a triangle (3-core
// subgraph: each member has coreness 2) with a pendant tail (each
// pendant has coreness 1).
func TestKCore_Mixed(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Triangle 0-1-2.
	a.AddEdge(0, 1, struct{}{})
	a.AddEdge(1, 2, struct{}{})
	a.AddEdge(0, 2, struct{}{})
	// Tail 2-3-4.
	a.AddEdge(2, 3, struct{}{})
	a.AddEdge(3, 4, struct{}{})
	c := csr.BuildFromAdjList(a)
	coreness := KCore(c)
	m := a.Mapper()
	expected := map[int]int{0: 2, 1: 2, 2: 2, 3: 1, 4: 1}
	for k, v := range expected {
		id, _ := m.Lookup(k)
		if coreness[id] != v {
			t.Fatalf("Mixed: coreness[%d] = %d, want %d", k, coreness[id], v)
		}
	}
}
