package search

// Task 775: BCC on trees — every edge is a bridge, every internal vertex
// is an articulation point.
//
// Two fixtures are tested:
//  1. Undirected P_5 (path, 5 nodes): 4 bridges, 3 articulation points
//     (internal nodes 1, 2, 3); leaves 0 and 4 are not articulations.
//  2. Undirected balanced binary tree of depth=2 (7 nodes, 6 edges):
//     6 bridges; root (0) and internal nodes (1, 2) are articulations;
//     leaves 3, 4, 5, 6 are not.
//
// shapegen.BalancedBinary forces cfg.Directed=true, so the undirected
// binary tree is built manually via adjlist.New.

import (
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestHopcroftTarjanBCC_Tree_Path5 tests BCC on an undirected P_5.
//
// Expected:
//   - 4 biconnected components (each single edge is its own BCC)
//   - 4 bridges: (0,1), (1,2), (2,3), (3,4)
//   - 3 articulation points: {1, 2, 3}
func TestHopcroftTarjanBCC_Tree_Path5(t *testing.T) {
	t.Parallel()

	const n = 5
	g, err := shapegen.Path(n, false).Build(defaultCfg())
	if err != nil {
		t.Fatalf("Path(%d).Build: %v", n, err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)

	// Each of the n-1 edges is its own BCC in a tree.
	if len(res.Components) != n-1 {
		t.Errorf("Components: got %d, want %d", len(res.Components), n-1)
	}

	// All n-1 edges are bridges.
	if len(res.Bridges) != n-1 {
		t.Errorf("Bridges: got %d, want %d", len(res.Bridges), n-1)
	}

	// Articulation points are exactly the internal nodes {1, 2, 3}.
	wantArtic := []int{1, 2, 3}
	gotArtic := resolveNodeIDs(a.Mapper(), res.Articulation)
	sort.Ints(gotArtic)
	if !intsEqual(gotArtic, wantArtic) {
		t.Errorf("Articulation: got %v, want %v", gotArtic, wantArtic)
	}
}

// TestHopcroftTarjanBCC_Tree_BinaryDepth2 tests BCC on an undirected
// balanced binary tree of depth 2 (7 nodes, 6 edges).
//
// Tree structure (BFS order):
//
//	    0
//	   / \
//	  1   2
//	 / \ / \
//	3  4 5  6
//
// Expected:
//   - 6 biconnected components (one per edge)
//   - 6 bridges
//   - 3 articulation points: {0, 1, 2}
func TestHopcroftTarjanBCC_Tree_BinaryDepth2(t *testing.T) {
	t.Parallel()

	// Build undirected binary tree manually because shapegen.BalancedBinary
	// forces cfg.Directed=true and does not expose an undirected variant.
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	// BFS-order parent edges: node i has parent (i-1)/2 for i in [1, 6].
	const n = 7
	for i := 1; i < n; i++ {
		parent := (i - 1) / 2
		if err := a.AddEdge(parent, i, 0); err != nil {
			t.Fatalf("AddEdge(%d->%d): %v", parent, i, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)

	// All n-1 edges are bridges in a tree.
	if len(res.Components) != n-1 {
		t.Errorf("Components: got %d, want %d", len(res.Components), n-1)
	}
	if len(res.Bridges) != n-1 {
		t.Errorf("Bridges: got %d, want %d", len(res.Bridges), n-1)
	}

	// Articulation points are exactly root and internal nodes {0, 1, 2}.
	wantArtic := []int{0, 1, 2}
	gotArtic := resolveNodeIDs(a.Mapper(), res.Articulation)
	sort.Ints(gotArtic)
	if !intsEqual(gotArtic, wantArtic) {
		t.Errorf("Articulation: got %v, want %v", gotArtic, wantArtic)
	}
}

// resolveNodeIDs converts a slice of NodeIDs to their integer keys via
// the mapper. Unknown IDs are silently omitted (should not occur in
// well-formed tests).
func resolveNodeIDs(m *graph.Mapper[int], ids []graph.NodeID) []int {
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		v, ok := m.Resolve(id)
		if ok {
			out = append(out, v)
		}
	}
	return out
}

// intsEqual reports whether two sorted int slices are identical.
func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
