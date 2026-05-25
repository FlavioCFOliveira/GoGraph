package search

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestTarjanSCC_Condensation verifies that TarjanSCC produces 3 SCCs of the
// correct sizes and that the condensation DAG (built from cross-SCC edges)
// admits a valid topological sort with no cycle detected.
//
// Graph layout:
//
//	SCC A (size 4): 0->1->2->3->0
//	SCC B (size 2): 4->5->4
//	SCC C (size 3): 6->7->8->6
//	Cross edges: 3->4 (A->B), 3->6 (A->C), 5->6 (B->C)
func TestTarjanSCC_Condensation(t *testing.T) {
	t.Parallel()

	edges := [][2]int{
		// SCC A
		{0, 1}, {1, 2}, {2, 3}, {3, 0},
		// SCC B
		{4, 5}, {5, 4},
		// SCC C
		{6, 7}, {7, 8}, {8, 6},
		// Cross edges
		{3, 4}, {3, 6}, {5, 6},
	}

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for _, e := range edges {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge(%d->%d): %v", e[0], e[1], err)
		}
	}
	c := csr.BuildFromAdjList(a)
	sccs := TarjanSCC(c)

	// Assertion 1: exactly 3 SCCs.
	if len(sccs) != 3 {
		t.Fatalf("SCC count = %d, want 3", len(sccs))
	}

	// Verify the SCC sizes form the expected multiset {4, 2, 3}.
	sizeCounts := map[int]int{}
	for _, comp := range sccs {
		sizeCounts[len(comp)]++
	}
	wantSizeCounts := map[int]int{4: 1, 2: 1, 3: 1}
	for sz, wantCnt := range wantSizeCounts {
		if sizeCounts[sz] != wantCnt {
			t.Fatalf("SCC size multiset: got %v, want %v", sizeCounts, wantSizeCounts)
		}
	}

	// Build the condensation: assign each NodeID to its SCC index.
	sccOf := make(map[graph.NodeID]int)
	for i, comp := range sccs {
		for _, id := range comp {
			sccOf[id] = i
		}
	}

	// Build condensation adjlist (deduplicated via a seen map).
	ca := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	seen := make(map[[2]int]bool)
	origVerts := c.VerticesSlice()
	origEdges := c.EdgesSlice()
	maxID := uint64(c.MaxNodeID())
	for from := uint64(0); from < maxID; from++ {
		fromSCC, fromLive := sccOf[graph.NodeID(from)]
		if !fromLive {
			continue
		}
		for k := origVerts[from]; k < origVerts[from+1]; k++ {
			to := uint64(origEdges[k])
			toSCC, toLive := sccOf[graph.NodeID(to)]
			if !toLive || fromSCC == toSCC {
				continue
			}
			key := [2]int{fromSCC, toSCC}
			if seen[key] {
				continue
			}
			seen[key] = true
			if err := ca.AddEdge(fromSCC, toSCC, struct{}{}); err != nil {
				t.Fatalf("condensation AddEdge(%d->%d): %v", fromSCC, toSCC, err)
			}
		}
	}

	cc := csr.BuildFromAdjList(ca)

	// Assertion 2: TopologicalSort on the condensation succeeds (no cycle).
	topoOrder, err := TopologicalSort(cc)
	if err != nil {
		t.Fatalf("TopologicalSort on condensation: %v", err)
	}

	// Assertion 3: topological order has exactly 3 elements (one per SCC).
	if len(topoOrder) != 3 {
		t.Fatalf("condensation topo order length = %d, want 3", len(topoOrder))
	}
}
