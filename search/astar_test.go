package search

import (
	"errors"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// zeroH is the trivial admissible heuristic that always returns 0;
// with this heuristic, A* degenerates into Dijkstra.
func zeroH(_ graph.NodeID) int64 { return 0 }

func TestAStar_Trivial(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 2}, {1, 2, 2}, {0, 2, 10},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(2)
	path, cost, err := AStar(c, src, dst, zeroH)
	if err != nil {
		t.Fatalf("AStar: %v", err)
	}
	if cost != 4 {
		t.Fatalf("cost = %d, want 4", cost)
	}
	if len(path) != 3 {
		t.Fatalf("path length = %d, want 3", len(path))
	}
}

func TestAStar_SameSrcDst(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{{0, 1, 1}})
	src, _ := a.Mapper().Lookup(0)
	path, cost, err := AStar(c, src, src, zeroH)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(path) != 1 || cost != 0 {
		t.Fatalf("self-path = %v cost=%d, want [src] 0", path, cost)
	}
}

func TestAStar_Unreachable(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{{0, 1, 1}, {2, 3, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	_, _, err := AStar(c, src, dst, zeroH)
	if !errors.Is(err, ErrNoPath) {
		t.Fatalf("expected ErrNoPath, got %v", err)
	}
}

func TestAStar_NegativeWeight(t *testing.T) {
	t.Parallel()
	c, _ := buildWeightedCSR(t, []weightedEdge{{0, 1, -3}})
	_, _, err := AStar(c, 0, 1, zeroH)
	if !errors.Is(err, ErrNegativeWeight) {
		t.Fatalf("expected ErrNegativeWeight, got %v", err)
	}
}

// TestAStar_VsDijkstraOnGrid builds a grid graph with Manhattan-
// distance heuristic and verifies A* finds the same cost as Dijkstra.
func TestAStar_VsDijkstraOnGrid(t *testing.T) {
	t.Parallel()
	const size = 32 // grid 32x32
	a := adjlist.New[int, int64](adjlist.Config{Directed: false})
	for r := 0; r < size; r++ {
		for col := 0; col < size; col++ {
			cur := r*size + col
			if col+1 < size {
				if err := a.AddEdge(cur, r*size+col+1, 1); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
			if r+1 < size {
				if err := a.AddEdge(cur, (r+1)*size+col, 1); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
		}
	}
	c := csr.BuildFromAdjList(a)

	srcID, _ := a.Mapper().Lookup(0)
	dstID, _ := a.Mapper().Lookup(size*size - 1)

	// Build a reverse map NodeID -> grid coords for the heuristic.
	m := a.Mapper()
	dstR, dstC := (size*size-1)/size, (size*size-1)%size
	h := func(id graph.NodeID) int64 {
		v, ok := m.Resolve(id)
		if !ok {
			return 0
		}
		rr := v / size
		cc := v % size
		dx := dstR - rr
		if dx < 0 {
			dx = -dx
		}
		dy := dstC - cc
		if dy < 0 {
			dy = -dy
		}
		return int64(dx + dy)
	}

	pathA, costA, errA := AStar(c, srcID, dstID, h)
	if errA != nil {
		t.Fatalf("AStar: %v", errA)
	}
	dij, errD := Dijkstra(c, srcID)
	if errD != nil {
		t.Fatalf("Dijkstra: %v", errD)
	}
	costD, ok := dij.Distance(dstID)
	if !ok {
		t.Fatalf("Dijkstra: dst unreachable")
	}
	if costA != costD {
		t.Fatalf("AStar cost = %d, Dijkstra cost = %d", costA, costD)
	}
	if int64(len(pathA)-1) != costA {
		t.Fatalf("path length-1 = %d, want %d (unit weights)", len(pathA)-1, costA)
	}
}
