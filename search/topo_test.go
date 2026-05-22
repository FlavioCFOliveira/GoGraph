package search

import (
	"errors"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestTopologicalSort_LinearDAG(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for i := 0; i < 5; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	order, err := TopologicalSort(c)
	if err != nil {
		t.Fatalf("topo: %v", err)
	}
	if len(order) != 6 {
		t.Fatalf("len = %d, want 6", len(order))
	}
	// position[id] must be strictly less than position[succ(id)]
	pos := map[int]int{}
	for i, id := range order {
		v, _ := a.Mapper().Resolve(id)
		pos[v] = i
	}
	for i := 0; i < 5; i++ {
		if pos[i] >= pos[i+1] {
			t.Fatalf("pos[%d]=%d, pos[%d]=%d (not topo-ordered)", i, pos[i], i+1, pos[i+1])
		}
	}
}

func TestTopologicalSort_DiamondDAG(t *testing.T) {
	t.Parallel()
	edges := [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}}
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for _, e := range edges {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	order, err := TopologicalSort(c)
	if err != nil {
		t.Fatalf("topo: %v", err)
	}
	pos := map[int]int{}
	for i, id := range order {
		v, _ := a.Mapper().Resolve(id)
		pos[v] = i
	}
	for _, e := range edges {
		if pos[e[0]] >= pos[e[1]] {
			t.Fatalf("edge %d->%d not respected", e[0], e[1])
		}
	}
}

func TestTopologicalSort_DetectsCycle(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 0, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	_, err := TopologicalSort(c)
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("expected ErrCycle, got %v", err)
	}
}
