package csr

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestCSR_IsSymmetric_Undirected verifies that an undirected
// AdjList produces a symmetric CSR.
func TestCSR_IsSymmetric_Undirected(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 0, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := BuildFromAdjList(a)
	if !c.IsSymmetric() {
		t.Fatal("undirected AdjList must produce a symmetric CSR")
	}
}

// TestCSR_IsSymmetric_Directed_Asymmetric verifies that a directed
// AdjList with a one-way edge yields a non-symmetric CSR.
func TestCSR_IsSymmetric_Directed_Asymmetric(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := BuildFromAdjList(a)
	if c.IsSymmetric() {
		t.Fatal("directed one-way graph must not be symmetric")
	}
}

// TestCSR_IsSymmetric_Directed_BothWays verifies that adding both
// directed edges (u, v) and (v, u) for every pair restores symmetry.
func TestCSR_IsSymmetric_Directed_BothWays(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	pairs := [][2]int{{0, 1}, {1, 2}, {2, 0}}
	for _, p := range pairs {
		if err := a.AddEdge(p[0], p[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := a.AddEdge(p[1], p[0], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := BuildFromAdjList(a)
	if !c.IsSymmetric() {
		t.Fatal("directed graph with mirrored edges must be symmetric")
	}
}

// TestCSR_IsSymmetric_SelfLoopsOnly is the trivial symmetric case:
// every edge is a self-loop, so the symmetry check trivially holds.
func TestCSR_IsSymmetric_SelfLoopsOnly(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 0, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := BuildFromAdjList(a)
	if !c.IsSymmetric() {
		t.Fatal("self-loops only must be symmetric")
	}
}

// TestCSR_IsSymmetric_Empty trivially holds: an empty edge set is
// symmetric.
func TestCSR_IsSymmetric_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddNode(0); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := a.AddNode(1); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	c := BuildFromAdjList(a)
	if !c.IsSymmetric() {
		t.Fatal("empty edge set must be reported symmetric")
	}
}
