package search

// Task 884: WCC on a directed graph with exactly three weakly-connected
// components.
//
// The graph has 10 vertices in three isolated groups:
//   - Component A (3 nodes): directed cycle  0->1->2->0
//   - Component B (4 nodes): directed path   3->4->5->6
//   - Component C (3 nodes): directed path   7->8->9
//
// Because WCC treats edges as undirected (symmetric closure), each
// group forms one weakly-connected component regardless of edge
// orientation. Expected output: k=3, component sizes {3, 4, 3}.

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestWCC_DisconnectedForest verifies that WCC correctly identifies
// three weakly-connected components in a directed graph whose three
// edge-disjoint subgraphs share no vertices.
func TestWCC_DisconnectedForest(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})

	addEdge := func(u, v int) {
		t.Helper()
		if err := a.AddEdge(u, v, 1); err != nil {
			t.Fatalf("AddEdge(%d->%d): %v", u, v, err)
		}
	}

	// Component A: directed cycle 0->1->2->0 (3 vertices).
	addEdge(0, 1)
	addEdge(1, 2)
	addEdge(2, 0)

	// Component B: directed path 3->4->5->6 (4 vertices).
	addEdge(3, 4)
	addEdge(4, 5)
	addEdge(5, 6)

	// Component C: directed path 7->8->9 (3 vertices).
	addEdge(7, 8)
	addEdge(8, 9)

	c := csr.BuildFromAdjList(a)
	comp, k, err := WCC(c)
	if err != nil {
		t.Fatalf("WCC: %v", err)
	}
	if k != 3 {
		t.Fatalf("k = %d, want 3", k)
	}

	mapper := a.Mapper()
	nodeID := func(key int) int {
		t.Helper()
		id, ok := mapper.Lookup(key)
		if !ok {
			t.Fatalf("key %d not found in mapper", key)
		}
		return int(id)
	}

	// All vertices in each component must share the same component label.
	compA := comp[nodeID(0)]
	for _, key := range []int{1, 2} {
		if comp[nodeID(key)] != compA {
			t.Errorf("component A: node %d has label %d, want %d",
				key, comp[nodeID(key)], compA)
		}
	}

	compB := comp[nodeID(3)]
	for _, key := range []int{4, 5, 6} {
		if comp[nodeID(key)] != compB {
			t.Errorf("component B: node %d has label %d, want %d",
				key, comp[nodeID(key)], compB)
		}
	}

	compC := comp[nodeID(7)]
	for _, key := range []int{8, 9} {
		if comp[nodeID(key)] != compC {
			t.Errorf("component C: node %d has label %d, want %d",
				key, comp[nodeID(key)], compC)
		}
	}

	// All three labels must be distinct.
	if compA == compB || compA == compC || compB == compC {
		t.Fatalf("component labels not distinct: A=%d B=%d C=%d", compA, compB, compC)
	}

	// Sizes per component label.
	sizes := make(map[int]int)
	for _, cid := range comp {
		if cid >= 0 {
			sizes[cid]++
		}
	}
	if len(sizes) != 3 {
		t.Fatalf("distinct component labels = %d, want 3", len(sizes))
	}
	wantSizes := map[int]bool{3: true, 4: true} // {3, 4, 3} — two entries of 3
	sizeFreq := make(map[int]int)
	for _, sz := range sizes {
		sizeFreq[sz]++
	}
	if sizeFreq[3] != 2 || sizeFreq[4] != 1 {
		t.Fatalf("component sizes = %v, want {3:2, 4:1}", sizeFreq)
	}
	_ = wantSizes

	// Determinism: a second call on the same CSR must return identical
	// component assignments and the same k.
	comp2, k2, err2 := WCC(c)
	if err2 != nil {
		t.Fatalf("WCC (second call): %v", err2)
	}
	if k2 != k {
		t.Fatalf("second call k = %d, want %d", k2, k)
	}
	if len(comp2) != len(comp) {
		t.Fatalf("second call len(comp) = %d, want %d", len(comp2), len(comp))
	}
	for i, cid := range comp {
		if comp2[i] != cid {
			t.Fatalf("comp[%d]: first=%d second=%d (non-deterministic)", i, cid, comp2[i])
		}
	}
}
