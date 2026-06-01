package search

// Task 633: BellmanFord detects negative cycles.
//
// Three directed graphs, each containing a negative-weight cycle
// reachable from the source. In all cases BellmanFord must return
// ErrNegativeCycle. BellmanFord returns (nil, error) — it does NOT
// return a cycle vertex list.

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestBellmanFord_NegCycle_SimpleTriangle tests a 3-node cycle
// 0→1→2→0 where every edge carries weight -1.0.
func TestBellmanFord_NegCycle_SimpleTriangle(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	for _, e := range [][3]float64{{0, 1, -1.0}, {1, 2, -1.0}, {2, 0, -1.0}} {
		if err := a.AddEdge(int(e[0]), int(e[1]), e[2]); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	_, err := BellmanFord(c, src)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("3-cycle: expected ErrNegativeCycle, got %v", err)
	}
}

// TestBellmanFord_NegCycle_FiveCycle tests a 5-node cycle
// 0→1→2→3→4→0 where every edge carries weight -1.0.
func TestBellmanFord_NegCycle_FiveCycle(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	for i := 0; i < 5; i++ {
		if err := a.AddEdge(i, (i+1)%5, -1.0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	_, err := BellmanFord(c, src)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("5-cycle: expected ErrNegativeCycle, got %v", err)
	}
}

// TestBellmanFord_NegCycle_ReachableViaPath tests a graph where the
// source (0) reaches the negative cycle via a positive-weight path
// 0→1→2→3, and then the cycle 3→4→5→3 has all -1.0 weights.
func TestBellmanFord_NegCycle_ReachableViaPath(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	// Path to the cycle.
	for _, e := range [][3]float64{
		{0, 1, 1.0},
		{1, 2, 1.0},
		{2, 3, 1.0},
	} {
		if err := a.AddEdge(int(e[0]), int(e[1]), e[2]); err != nil {
			t.Fatalf("AddEdge path: %v", err)
		}
	}
	// Negative cycle 3→4→5→3.
	for _, e := range [][3]float64{
		{3, 4, -1.0},
		{4, 5, -1.0},
		{5, 3, -1.0},
	} {
		if err := a.AddEdge(int(e[0]), int(e[1]), e[2]); err != nil {
			t.Fatalf("AddEdge cycle: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	_, err := BellmanFord(c, src)
	if !errors.Is(err, ErrNegativeCycle) {
		t.Fatalf("reachable-cycle: expected ErrNegativeCycle, got %v", err)
	}
}
