package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestDiameter_Path(t *testing.T) {
	t.Parallel()
	// Path 0-1-2-3-4: diameter = 4.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		a.AddEdge(i, i+1, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	lo, hi, exact := Diameter(c)
	if lo != 4 || hi != 4 || !exact {
		t.Fatalf("Diameter = (%d, %d, %v), want (4, 4, true)", lo, hi, exact)
	}
}

func TestDiameter_Cycle(t *testing.T) {
	t.Parallel()
	// Cycle 0-1-2-3-4-0: diameter = floor(5/2) = 2.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 5; i++ {
		a.AddEdge(i, (i+1)%5, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	lo, _, _ := Diameter(c)
	if lo != 2 {
		t.Fatalf("Cycle5 diameter lo = %d, want 2", lo)
	}
}

func TestDiameter_Star(t *testing.T) {
	t.Parallel()
	// Star: hub 0 connected to 1..4. Diameter = 2 (any leaf to any leaf).
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 1; i <= 4; i++ {
		a.AddEdge(0, i, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	lo, _, _ := Diameter(c)
	if lo != 2 {
		t.Fatalf("Star diameter lo = %d, want 2", lo)
	}
}

func TestDiameter_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	c := csr.BuildFromAdjList(a)
	lo, hi, exact := Diameter(c)
	if lo != 0 || hi != 0 || !exact {
		t.Fatalf("Empty diameter = (%d, %d, %v), want (0, 0, true)", lo, hi, exact)
	}
}
