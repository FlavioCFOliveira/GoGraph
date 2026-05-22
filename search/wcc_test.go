package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestWCC_TwoComponents(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(3, 4, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	comp, k, err := WCC(c)
	if err != nil {
		t.Fatalf("WCC: %v", err)
	}
	if k != 2 {
		t.Fatalf("k = %d, want 2", k)
	}
	id0, _ := a.Mapper().Lookup(0)
	id2, _ := a.Mapper().Lookup(2)
	id3, _ := a.Mapper().Lookup(3)
	id4, _ := a.Mapper().Lookup(4)
	if comp[id0] != comp[id2] {
		t.Fatalf("0 and 2 should be in same WCC: c=%v", comp)
	}
	if comp[id3] != comp[id4] {
		t.Fatalf("3 and 4 should be in same WCC: c=%v", comp)
	}
	if comp[id0] == comp[id3] {
		t.Fatalf("0 and 3 should be in different WCCs")
	}
}

// TestWCC_SymmetricClosure asserts WCC on a DIRECTED graph treats
// edges as undirected — a 0->1 and a 2->0 chain belong to the same
// weakly-connected component even though neither is reachable from
// the other in the directed sense.
func TestWCC_SymmetricClosure(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 0, struct{}{}); err != nil { // 0 cannot reach 2, but symmetrically connected
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	comp, k, err := WCC(c)
	if err != nil {
		t.Fatalf("WCC: %v", err)
	}
	if k != 1 {
		t.Fatalf("k = %d, want 1", k)
	}
	id0, _ := a.Mapper().Lookup(0)
	id2, _ := a.Mapper().Lookup(2)
	if comp[id0] != comp[id2] {
		t.Fatalf("0 and 2 should be in same WCC: c=%v", comp)
	}
}

func TestWCC_Isolated(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddNode(0); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := a.AddNode(1); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := a.AddNode(2); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	_, k, err := WCC(c)
	if err != nil {
		t.Fatalf("WCC: %v", err)
	}
	// All nodes are isolated (no edges) so the live mask is empty.
	if k != 0 {
		t.Fatalf("k = %d, want 0 (no live nodes)", k)
	}
}
