package search

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestHierholzer_Directed verifies Hierholzer on a directed C4 (four
// vertices forming a single directed cycle). Every vertex has in-degree
// equal to out-degree (both 1), so an Eulerian circuit must exist with
// length |E|+1 = 5.
func TestHierholzer_Directed(t *testing.T) {
	t.Parallel()
	// Directed C4: 0→1→2→3→0
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	edges := [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 0}}
	for _, e := range edges {
		if err := a.AddEdge(e[0], e[1], int64(1)); err != nil {
			t.Fatalf("AddEdge(%d→%d): %v", e[0], e[1], err)
		}
	}
	c := csr.BuildFromAdjList(a)
	circuit, err := Hierholzer(c)
	if err != nil {
		t.Fatalf("Hierholzer: %v", err)
	}
	// |E|+1 = 4+1 = 5
	if len(circuit) != 5 {
		t.Fatalf("circuit length = %d, want 5 (4 edges + 1)", len(circuit))
	}
	if circuit[0] != circuit[4] {
		t.Fatalf("circuit must close: circuit[0]=%d circuit[4]=%d", circuit[0], circuit[4])
	}

	// Verify every directed edge appears exactly once.
	mapper := a.Mapper()
	type dirEdge struct{ from, to graph.NodeID }
	counts := make(map[dirEdge]int)
	for i := 0; i+1 < len(circuit); i++ {
		counts[dirEdge{circuit[i], circuit[i+1]}]++
	}
	for _, e := range edges {
		fromID, ok := mapper.Lookup(e[0])
		if !ok {
			t.Fatalf("Lookup(%d) failed", e[0])
		}
		toID, ok := mapper.Lookup(e[1])
		if !ok {
			t.Fatalf("Lookup(%d) failed", e[1])
		}
		key := dirEdge{fromID, toID}
		if counts[key] != 1 {
			t.Errorf("directed edge (%d→%d) appeared %d times, want 1", e[0], e[1], counts[key])
		}
	}
}
