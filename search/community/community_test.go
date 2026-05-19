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
	if p.NumCommunities < 2 {
		t.Fatalf("Leiden found only %d communities, want >= 2", p.NumCommunities)
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
