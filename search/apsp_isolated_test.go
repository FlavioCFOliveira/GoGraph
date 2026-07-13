package search

// apsp_isolated_test.go — regression for the APSP isolated-node self-distance
// finding (2026-07-13 audit, graph F4). An APSP matrix is indexed only over
// live NodeIDs (nodes with >= 1 incident edge), so At(x,x) for a degree-0 node
// returned (0,false) — unreachable — where textbook APSP (and BellmanFord for
// the same source) gives dist(x,x)=0. At now returns (0,true) for a self-query
// on an isolated node, while preserving the negative-cycle diagonal for live
// nodes.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestFloydWarshall_IsolatedNodeSelfDistance(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for _, e := range []weightedEdge{{0, 1, 5}, {1, 2, 3}} {
		if err := a.AddEdge(e.from, e.to, e.w); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	if err := a.AddNode(99); err != nil { // a degree-0 (isolated) node
		t.Fatalf("AddNode: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	apsp := FloydWarshall(c)

	iso, ok := a.Mapper().Lookup(99)
	if !ok {
		t.Fatal("isolated node was not interned")
	}
	// dist(x,x) = 0, reachable — even for a degree-0 node.
	d, reach := apsp.At(iso, iso)
	if !reach || d != 0 {
		t.Fatalf("At(isolated,isolated) = (%v,%v), want (0,true)", d, reach)
	}
	// Distance from the isolated node to a connected node is unreachable.
	other, _ := a.Mapper().Lookup(0)
	if _, r := apsp.At(iso, other); r {
		t.Fatal("At(isolated, connected) reachable, want unreachable")
	}
}
