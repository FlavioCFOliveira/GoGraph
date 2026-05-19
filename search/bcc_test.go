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
