package centrality

import (
	"math"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestBetweenness_Path(t *testing.T) {
	t.Parallel()
	// Undirected path 0-1-2-3-4. Centre node 2 has the highest
	// betweenness; nodes 0/4 are zero.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		a.AddEdge(i, i+1, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	bc := Betweenness(c)
	id0, _ := a.Mapper().Lookup(0)
	id2, _ := a.Mapper().Lookup(2)
	id4, _ := a.Mapper().Lookup(4)
	if bc[uint64(id0)] != 0 || bc[uint64(id4)] != 0 {
		t.Fatalf("endpoint betweenness must be 0")
	}
	if bc[uint64(id2)] <= bc[uint64(id0)] {
		t.Fatalf("centre betweenness must exceed endpoints")
	}
}

func TestBetweenness_Star(t *testing.T) {
	t.Parallel()
	// Star: hub 0 connected to 1..4. Hub has max betweenness;
	// every leaf has 0.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 1; i <= 4; i++ {
		a.AddEdge(0, i, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	bc := Betweenness(c)
	hub, _ := a.Mapper().Lookup(0)
	if math.Abs(bc[uint64(hub)]-12) > 1e-9 { // 4*3 ordered pairs through hub
		t.Fatalf("hub betweenness = %f, want 12", bc[uint64(hub)])
	}
}
