package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestCountTriangles_K5(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 5; i++ {
		for j := i + 1; j < 5; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	c := csr.BuildFromAdjList(a)
	total, perNode := CountTriangles(c)
	// K5 has C(5,3) = 10 triangles. Each vertex is in 6 of them.
	if total != 10 {
		t.Fatalf("K5 total triangles = %d, want 10", total)
	}
	m := a.Mapper()
	for i := 0; i < 5; i++ {
		id, _ := m.Lookup(i)
		if perNode[id] != 6 {
			t.Fatalf("K5 perNode[%d] = %d, want 6", i, perNode[id])
		}
	}
}

func TestCountTriangles_NoTriangle(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Path 0-1-2-3: no triangle.
	a.AddEdge(0, 1, struct{}{})
	a.AddEdge(1, 2, struct{}{})
	a.AddEdge(2, 3, struct{}{})
	c := csr.BuildFromAdjList(a)
	total, _ := CountTriangles(c)
	if total != 0 {
		t.Fatalf("path triangles = %d, want 0", total)
	}
}

func TestCountTriangles_OneTriangle(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	a.AddEdge(0, 1, struct{}{})
	a.AddEdge(1, 2, struct{}{})
	a.AddEdge(0, 2, struct{}{})
	c := csr.BuildFromAdjList(a)
	total, perNode := CountTriangles(c)
	if total != 1 {
		t.Fatalf("triangle count = %d, want 1", total)
	}
	m := a.Mapper()
	for i := 0; i < 3; i++ {
		id, _ := m.Lookup(i)
		if perNode[id] != 1 {
			t.Fatalf("perNode[%d] = %d, want 1", i, perNode[id])
		}
	}
}
