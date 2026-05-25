package search

// Task 675: Yen k-shortest on a multi-path adversarial fixture.
//
// A hand-built directed graph with src=0, dst=5 and five distinct
// edge-disjoint paths of increasing cost is used to verify that
// YenKShortest:
//
//   - returns exactly 5 paths (all loopless paths exist),
//   - returns them in non-decreasing cost order,
//   - every path starts at src and ends at dst.
//
// Graph topology (float64 weights, directed):
//
//	Path 1:  0→1 (1.0), 1→5 (1.0)                 total 2.0
//	Path 2:  0→2 (1.0), 2→5 (2.0)                 total 3.0
//	Path 3:  0→3 (2.0), 3→5 (2.0)                 total 4.0
//	Path 4:  0→4 (3.0), 4→5 (2.0)                 total 5.0
//	Path 5:  0→1 (1.0), 1→2 (1.0), 2→5 (2.0)      total 4.0
//	         (this reuses 0→1 and 2→5 with an extra 1→2 hop)
//
// Because Yen is loopless and the graph is sparse, paths 3 and 5 both
// cost 4.0; the tie-breaking is left to the algorithm but the
// non-decreasing assertion holds regardless of tie-break order.

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestYen_MultiPath_FivePaths(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, float64](adjlist.Config{Directed: true, Multigraph: true})

	type fe struct {
		u, v int
		w    float64
	}
	edges := []fe{
		{0, 1, 1.0}, // path 1 and path 5 share this edge
		{1, 5, 1.0}, // path 1 terminus
		{0, 2, 1.0}, // path 2 start
		{2, 5, 2.0}, // path 2 terminus (also used by path 5 via 1→2→5)
		{0, 3, 2.0}, // path 3 start
		{3, 5, 2.0}, // path 3 terminus
		{0, 4, 3.0}, // path 4 start
		{4, 5, 2.0}, // path 4 terminus
		{1, 2, 1.0}, // path 5 extra hop (0→1→2→5)
	}
	for _, e := range edges {
		if err := a.AddEdge(e.u, e.v, e.w); err != nil {
			t.Fatalf("AddEdge(%d→%d): %v", e.u, e.v, err)
		}
	}

	c := csr.BuildFromAdjList(a)
	m := a.Mapper()

	src, ok := m.Lookup(0)
	if !ok {
		t.Fatal("key 0 not in mapper")
	}
	dst, ok := m.Lookup(5)
	if !ok {
		t.Fatal("key 5 not in mapper")
	}

	const k = 5
	paths := YenKShortest(c, src, dst, k)

	if len(paths) != k {
		t.Fatalf("YenKShortest returned %d paths, want %d", len(paths), k)
	}

	// Non-decreasing cost order.
	for i := 1; i < len(paths); i++ {
		if paths[i].Cost < paths[i-1].Cost {
			t.Fatalf("paths[%d].Cost=%v < paths[%d].Cost=%v (not sorted)", i, paths[i].Cost, i-1, paths[i-1].Cost)
		}
	}

	// Every path must start at src and end at dst.
	for i, p := range paths {
		if len(p.Nodes) < 2 {
			t.Fatalf("path[%d] has fewer than 2 nodes: %v", i, p.Nodes)
		}
		if p.Nodes[0] != graph.NodeID(src) {
			t.Fatalf("path[%d] starts at %v, want %v", i, p.Nodes[0], src)
		}
		if p.Nodes[len(p.Nodes)-1] != graph.NodeID(dst) {
			t.Fatalf("path[%d] ends at %v, want %v", i, p.Nodes[len(p.Nodes)-1], dst)
		}
	}

	// Cheapest path must cost 2.0 (0→1→5).
	if paths[0].Cost != 2.0 {
		t.Fatalf("cheapest path cost = %v, want 2.0", paths[0].Cost)
	}
}
