package search

import (
	"sort"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestTarjanSCC_ChainOfSCCs verifies that Tarjan correctly identifies four
// isolated SCCs (directed triangles) connected by forward edges. Each
// triangle is its own SCC of size 3, and they form a linear condensation
// DAG. Tarjan emits SCCs in reverse topological order, so triangle 3
// (the leaf) is emitted first and triangle 0 (the root) last.
func TestTarjanSCC_ChainOfSCCs(t *testing.T) {
	t.Parallel()

	// triangle 0: 0->1->2->0
	// triangle 1: 3->4->5->3
	// triangle 2: 6->7->8->6
	// triangle 3: 9->10->11->9
	// forward edges: 2->3, 5->6, 8->9
	edges := [][2]int{
		{0, 1}, {1, 2}, {2, 0},
		{3, 4}, {4, 5}, {5, 3},
		{6, 7}, {7, 8}, {8, 6},
		{9, 10}, {10, 11}, {11, 9},
		{2, 3}, {5, 6}, {8, 9},
	}

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for _, e := range edges {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge(%d->%d): %v", e[0], e[1], err)
		}
	}
	c := csr.BuildFromAdjList(a)
	sccs := TarjanSCC(c)

	// Assertion 1: exactly 4 SCCs.
	if len(sccs) != 4 {
		t.Fatalf("SCC count = %d, want 4", len(sccs))
	}

	// Assertion 2: each SCC has exactly 3 vertices.
	for i, comp := range sccs {
		if len(comp) != 3 {
			t.Fatalf("SCC[%d] size = %d, want 3", i, len(comp))
		}
	}

	// Decode each SCC to a sorted set of int keys.
	decoded := make([][]int, len(sccs))
	for i, comp := range sccs {
		keys := make([]int, len(comp))
		for j, id := range comp {
			v, ok := a.Mapper().Resolve(id)
			if !ok {
				t.Fatalf("Resolve(%d) not found", id)
			}
			keys[j] = v
		}
		sort.Ints(keys)
		decoded[i] = keys
	}

	// Assertion 3: SCCs are emitted in reverse topological order.
	// Expected emission order: triangle 3 first (leaf), triangle 0 last (root).
	want := [][]int{
		{9, 10, 11},
		{6, 7, 8},
		{3, 4, 5},
		{0, 1, 2},
	}
	for i, w := range want {
		got := decoded[i]
		if len(got) != len(w) {
			t.Fatalf("SCC[%d]: got %v, want %v", i, got, w)
		}
		for j := range w {
			if got[j] != w[j] {
				t.Fatalf("SCC[%d]: got %v, want %v", i, got, w)
			}
		}
	}
}
