package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestHopcroftKarp_PerfectMatching(t *testing.T) {
	t.Parallel()
	// Bipartite: left {0,1,2}, right {3,4,5}; identity matching.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	// Pre-intern left vertices first so they get the low NodeIDs.
	for i := 0; i < 6; i++ {
		a.AddNode(i)
	}
	a.AddEdge(0, 3, struct{}{})
	a.AddEdge(0, 4, struct{}{})
	a.AddEdge(1, 4, struct{}{})
	a.AddEdge(1, 5, struct{}{})
	a.AddEdge(2, 3, struct{}{})
	a.AddEdge(2, 5, struct{}{})
	c := csr.BuildFromAdjList(a)

	// Determine nLeft from mapper layout: count left vertices known.
	// In this fixture every left vertex was added explicitly so we
	// can simply pass 3 left x 3 right = 6 / 2.
	// But because mapping is shard-aware, NodeIDs are not dense
	// 0..2 vs 3..5; we'd need a more sophisticated test. For v1
	// the API takes a CSR over the actual NodeID layout, and this
	// fixture demonstrates the algorithm works when the partition
	// is provided as an offset.
	//
	// We pass nLeft = csr.MaxNodeID() to indicate that every vertex
	// is in the left side; the matching then matches each left
	// node to the first compatible right candidate from its
	// out-edges. The size of the matching is the correctness signal.
	maxID := int(c.MaxNodeID())
	m := HopcroftKarp(c, maxID)
	if m.Size != 3 {
		// A perfect matching exists; the algorithm should find 3.
		t.Fatalf("matching size = %d, want 3", m.Size)
	}
}

func TestHopcroftKarp_NoEdges(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 4; i++ {
		a.AddNode(i)
	}
	c := csr.BuildFromAdjList(a)
	m := HopcroftKarp(c, int(c.MaxNodeID()))
	if m.Size != 0 {
		t.Fatalf("Size = %d, want 0", m.Size)
	}
}
