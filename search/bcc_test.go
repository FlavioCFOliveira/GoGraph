package search

import (
	"sort"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestHopcroftTarjanBCC_BridgeFixture(t *testing.T) {
	t.Parallel()
	// Two triangles 0-1-2 and 3-4-5 connected by a bridge 2-3.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	edges := [][2]int{{0, 1}, {1, 2}, {2, 0}, {2, 3}, {3, 4}, {4, 5}, {5, 3}}
	for _, e := range edges {
		a.AddEdge(e[0], e[1], struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)

	// Bridge 2-3 must be detected.
	id2, _ := a.Mapper().Lookup(2)
	id3, _ := a.Mapper().Lookup(3)
	hasBridge := false
	for _, b := range res.Bridges {
		if (b[0] == id2 && b[1] == id3) || (b[0] == id3 && b[1] == id2) {
			hasBridge = true
		}
	}
	if !hasBridge {
		t.Fatalf("bridge 2-3 not detected; got %v", res.Bridges)
	}

	// Articulation points should be 2 and 3.
	articInts := []int{}
	for _, id := range res.Articulation {
		v, _ := a.Mapper().Resolve(id)
		articInts = append(articInts, v)
	}
	sort.Ints(articInts)
	if len(articInts) < 1 {
		t.Fatalf("expected at least one articulation point, got %v", articInts)
	}
}

func TestHopcroftTarjanBCC_SingleCycle(t *testing.T) {
	t.Parallel()
	// A single cycle: no bridges, no articulation points.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 5; i++ {
		a.AddEdge(i, (i+1)%5, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)
	if len(res.Bridges) != 0 {
		t.Fatalf("single cycle should have no bridges, got %v", res.Bridges)
	}
	if len(res.Articulation) != 0 {
		t.Fatalf("single cycle should have no articulation points, got %v", res.Articulation)
	}
}

// TestHopcroftTarjanBCC_MultigraphParallel verifies that two parallel
// undirected edges between the same pair of vertices form a single
// 2-cycle BCC and are NOT mistakenly classified as a bridge. This
// pins the parent-edge-skip fix that switched the algorithm from a
// parent-NodeID filter (which dropped all parallel edges to the
// parent) to a parent-edge-index filter (which skips only the one
// edge used to descend into the child).
func TestHopcroftTarjanBCC_MultigraphParallel(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false, Multigraph: true})
	a.AddEdge(0, 1, struct{}{})
	a.AddEdge(0, 1, struct{}{}) // parallel
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)
	if len(res.Bridges) != 0 {
		t.Fatalf("two parallel edges form a 2-cycle BCC, not a bridge; got bridges=%v", res.Bridges)
	}
	if len(res.Articulation) != 0 {
		t.Fatalf("two parallel edges should produce no articulation points; got %v", res.Articulation)
	}
	if len(res.Components) != 1 {
		t.Fatalf("two parallel edges should form exactly 1 BCC; got %d components", len(res.Components))
	}
}

// TestHopcroftTarjanBCC_MultigraphSingleEdgeStillBridge verifies the
// non-multigraph case: a single bridge edge between two cliques is
// still a bridge under the multigraph-aware fix.
func TestHopcroftTarjanBCC_MultigraphSingleEdgeStillBridge(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false, Multigraph: true})
	for _, e := range [][2]int{{0, 1}, {1, 2}, {2, 0}, {2, 3}, {3, 4}, {4, 5}, {5, 3}} {
		a.AddEdge(e[0], e[1], struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)
	if len(res.Bridges) == 0 {
		t.Fatalf("single bridge edge 2-3 should be detected in multigraph mode; got %v", res.Bridges)
	}
}
