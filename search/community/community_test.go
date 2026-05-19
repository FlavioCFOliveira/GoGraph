package community

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestLeiden_TwoCliques(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Two K4 cliques joined by a single bridge.
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	for i := 4; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	a.AddEdge(3, 4, struct{}{})
	c := csr.BuildFromAdjList(a)
	p := Leiden(c, DefaultLeidenOptions())
	// Traag-Waltman Leiden separates two strongly-internally-connected
	// cliques across a single bridge edge: modularity gain on moving
	// any K4 node to the opposite clique is negative.
	if p.NumCommunities != 2 {
		t.Fatalf("Leiden TwoCliques found %d communities, want 2", p.NumCommunities)
	}
	// Verify the partition correctly groups the two K4s.
	mask := c.LiveMask()
	for id, m := range mask {
		if m {
			if p.Community[id] < 0 {
				t.Fatalf("live NodeID %d got sentinel community", id)
			}
		} else if p.Community[id] != -1 {
			t.Fatalf("ghost NodeID %d got community %d, want -1", id, p.Community[id])
		}
	}
	id0, _ := a.Mapper().Lookup(0)
	id3, _ := a.Mapper().Lookup(3)
	id4, _ := a.Mapper().Lookup(4)
	id7, _ := a.Mapper().Lookup(7)
	if p.Community[id0] != p.Community[id3] {
		t.Fatalf("left clique split: c(0)=%d c(3)=%d", p.Community[id0], p.Community[id3])
	}
	if p.Community[id4] != p.Community[id7] {
		t.Fatalf("right clique split: c(4)=%d c(7)=%d", p.Community[id4], p.Community[id7])
	}
	if p.Community[id0] == p.Community[id4] {
		t.Fatalf("Leiden merged left and right cliques: c(0)=c(4)=%d", p.Community[id0])
	}
}

// TestLeiden_DisconnectedComponents stresses the post-pass that
// splits any disconnected community into its connected components —
// the Leiden-vs-Louvain guarantee that the v1 simplification still
// keeps.
func TestLeiden_DisconnectedComponents(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	// Two fully-disjoint K3 cliques.
	for i := 0; i < 3; i++ {
		for j := i + 1; j < 3; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	for i := 3; i < 6; i++ {
		for j := i + 1; j < 6; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	c := csr.BuildFromAdjList(a)
	p := Leiden(c, DefaultLeidenOptions())
	if p.NumCommunities != 2 {
		t.Fatalf("Leiden on disjoint K3+K3 found %d communities, want 2", p.NumCommunities)
	}
}

func TestLabelPropagation_TwoCliques(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		for j := i + 1; j < 4; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	for i := 4; i < 8; i++ {
		for j := i + 1; j < 8; j++ {
			a.AddEdge(i, j, struct{}{})
		}
	}
	a.AddEdge(3, 4, struct{}{})
	c := csr.BuildFromAdjList(a)
	p := LabelPropagation(c, DefaultLabelPropagationOptions())
	if p.NumCommunities < 1 {
		t.Fatalf("LabelPropagation found 0 communities")
	}
}

func TestLeiden_Empty(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	c := csr.BuildFromAdjList(a)
	p := Leiden(c, DefaultLeidenOptions())
	if p.NumCommunities != 0 {
		t.Fatalf("empty: %+v", p)
	}
}
