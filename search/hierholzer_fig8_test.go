package search

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestHierholzerUndirected_FigureEight verifies that HierholzerUndirected
// correctly traverses a figure-eight graph: two triangles sharing vertex 0.
// All vertices have even degree (vertex 0 has degree 4; vertices 1–4 have
// degree 2), so an Eulerian circuit must exist with length |E|+1 = 7.
func TestHierholzerUndirected_FigureEight(t *testing.T) {
	t.Parallel()
	// Triangle 1: 0-1, 1-2, 0-2
	// Triangle 2: 0-3, 3-4, 0-4
	// Degrees: 0→4, 1→2, 2→2, 3→2, 4→2 — all even.
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	type edge struct{ u, v int }
	edges := []edge{
		{0, 1}, {1, 2}, {0, 2},
		{0, 3}, {3, 4}, {0, 4},
	}
	for _, e := range edges {
		if err := a.AddEdge(e.u, e.v, int64(1)); err != nil {
			t.Fatalf("AddEdge(%d,%d): %v", e.u, e.v, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	circuit, err := HierholzerUndirected(c)
	if err != nil {
		t.Fatalf("HierholzerUndirected: %v", err)
	}
	// |E|+1 = 6+1 = 7
	if len(circuit) != 7 {
		t.Fatalf("circuit length = %d, want 7 (6 edges + 1)", len(circuit))
	}
	if circuit[0] != circuit[6] {
		t.Fatalf("circuit must close: circuit[0]=%d circuit[6]=%d", circuit[0], circuit[6])
	}

	// Verify every undirected edge appears exactly once.
	// Normalise each CSR NodeID pair to (min, max).
	type normEdge struct{ lo, hi graph.NodeID }
	counts := make(map[normEdge]int)
	for i := 0; i+1 < len(circuit); i++ {
		u, v := circuit[i], circuit[i+1]
		if u > v {
			u, v = v, u
		}
		counts[normEdge{u, v}]++
	}
	// Build expected edge set from the adjlist mapper.
	mapper := a.Mapper()
	for _, e := range edges {
		uID, ok := mapper.Lookup(e.u)
		if !ok {
			t.Fatalf("Lookup(%d) failed", e.u)
		}
		vID, ok := mapper.Lookup(e.v)
		if !ok {
			t.Fatalf("Lookup(%d) failed", e.v)
		}
		lo, hi := uID, vID
		if lo > hi {
			lo, hi = hi, lo
		}
		key := normEdge{lo, hi}
		if counts[key] != 1 {
			t.Errorf("edge (%d,%d) appeared %d times, want 1", e.u, e.v, counts[key])
		}
	}
}
