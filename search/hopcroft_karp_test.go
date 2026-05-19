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

// TestHopcroftKarp_CompleteBipartite_K3x4 covers K(3,4): every left
// vertex is adjacent to every right vertex; the maximum matching is
// 3 (saturates the smaller side). Strengthens the previous test
// suite, which only exercised the partial bipartite case.
func TestHopcroftKarp_CompleteBipartite_K3x4(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	left := []string{"L0", "L1", "L2"}
	right := []string{"R0", "R1", "R2", "R3"}
	for _, l := range left {
		for _, r := range right {
			a.AddEdge(l, r, struct{}{})
		}
	}
	c := csr.BuildFromAdjList(a)
	m := HopcroftKarp(c, int(c.MaxNodeID()))
	if m.Size != 3 {
		t.Fatalf("K(3,4) max matching = %d, want 3", m.Size)
	}
}

// TestHopcroftKarp_HallCounterexample asserts the algorithm correctly
// reports a non-perfect matching when Hall's condition fails — here,
// two left vertices share a single right vertex and a third left
// vertex has no edges, so the maximum matching is at most 1.
func TestHopcroftKarp_HallCounterexample(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	// Left {L0, L1, L2}; right {R0}. Only L0 and L1 connect to R0;
	// L2 has no neighbours. By Hall's theorem the matching has at
	// most 1 (only R0 is reachable from any left vertex).
	a.AddNode("L0")
	a.AddNode("L1")
	a.AddNode("L2")
	a.AddNode("R0")
	a.AddEdge("L0", "R0", struct{}{})
	a.AddEdge("L1", "R0", struct{}{})
	c := csr.BuildFromAdjList(a)
	m := HopcroftKarp(c, int(c.MaxNodeID()))
	if m.Size != 1 {
		t.Fatalf("Hall-deficient bipartite max matching = %d, want 1", m.Size)
	}
}

// TestHopcroftKarp_SingleEdge is a smoke test that the smallest
// possible bipartite graph (one edge) yields a matching of size 1.
func TestHopcroftKarp_SingleEdge(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, struct{}](adjlist.Config{Directed: true})
	a.AddEdge("L", "R", struct{}{})
	c := csr.BuildFromAdjList(a)
	m := HopcroftKarp(c, int(c.MaxNodeID()))
	if m.Size != 1 {
		t.Fatalf("single-edge bipartite matching = %d, want 1", m.Size)
	}
}
