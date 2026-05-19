package search

import (
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestBFSDirectionOpt_Tree(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	edges := [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 4}, {1, 5}, {3, 6}}
	for _, e := range edges {
		a.AddEdge(e[0], e[1], struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	depths := map[int]int{}
	BFSDirectionOpt(c, src, func(node graph.NodeID, d int) bool {
		v, _ := a.Mapper().Resolve(node)
		depths[v] = d
		return true
	})
	want := map[int]int{0: 0, 1: 1, 2: 1, 3: 1, 4: 2, 5: 2, 6: 2}
	for k, v := range want {
		if depths[k] != v {
			t.Fatalf("depth[%d] = %d, want %d", k, depths[k], v)
		}
	}
}

func TestBFSDirectionOpt_AllReachable(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 9; i++ {
		a.AddEdge(0, i+1, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	visited := 0
	BFSDirectionOpt(c, src, func(_ graph.NodeID, _ int) bool {
		visited++
		return true
	})
	if visited != 10 {
		t.Fatalf("visited = %d, want 10", visited)
	}
}

func TestBFSDirectionOpt_EarlyStop(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 100; i++ {
		a.AddEdge(0, i+1, struct{}{})
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	visited := 0
	BFSDirectionOpt(c, src, func(_ graph.NodeID, _ int) bool {
		visited++
		return visited < 5
	})
	if visited != 5 {
		t.Fatalf("early-stop visited = %d", visited)
	}
}
