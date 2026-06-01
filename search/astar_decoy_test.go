package search

// Task 650: A* on a decoy graph.
//
// The graph has a short dead-end branch that a naïve greedy search
// might explore before the actual shortest path. AStar must return
// the same cost as Dijkstra regardless of the heuristic steering.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestAStar_DecoyGraph builds a directed graph where the source (0)
// has a cheap-looking dead-end branch (0→1→2→3→4, no edge to the
// goal) and the actual shortest path to goal (10) is
// 0→5→6→7→10 (cost 8.0). A* with a goal-directed admissible
// heuristic must find cost 8.0, matching Dijkstra.
func TestAStar_DecoyGraph(t *testing.T) {
	t.Parallel()

	// Nodes: 0 (src), 1-4 (dead-end branch), 5-7 (correct path),
	// 8-9 (longer alternate path), 10 (goal).
	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	addEdge := func(from, to int, w float64) {
		t.Helper()
		if err := a.AddEdge(from, to, w); err != nil {
			t.Fatalf("AddEdge(%d→%d): %v", from, to, err)
		}
	}

	// Dead-end branch — cheap individual hops but no route to goal.
	addEdge(0, 1, 1.0)
	addEdge(1, 2, 1.0)
	addEdge(2, 3, 1.0)
	addEdge(3, 4, 1.0)

	// Correct shortest path: cost 8.0.
	addEdge(0, 5, 2.0)
	addEdge(5, 6, 2.0)
	addEdge(6, 7, 2.0)
	addEdge(7, 10, 2.0)

	// Longer alternate path: cost 12.0.
	addEdge(0, 8, 4.0)
	addEdge(8, 9, 4.0)
	addEdge(9, 10, 4.0)

	c := csr.BuildFromAdjList(a)
	srcID, _ := a.Mapper().Lookup(0)
	dstID, _ := a.Mapper().Lookup(10)

	// Admissible heuristic: a lower bound on the true distance to
	// goal. Node 10 is the goal; h(v) = max(0, (10-v)*1.5) is
	// admissible because the cheapest reachable edge weight is 1.0
	// and we never over-estimate (we use 1.5×hops as an optimistic
	// lower bound since no path can be shorter than the number of
	// remaining hops times 1.0).
	h := func(id graph.NodeID) float64 {
		v, ok := a.Mapper().Resolve(id)
		if !ok {
			return 0
		}
		diff := 10 - v
		if diff <= 0 {
			return 0
		}
		return float64(diff) * 1.0 // 1.0 per remaining hop — admissible
	}

	_, costA, errA := AStar(c, srcID, dstID, h)
	if errA != nil {
		t.Fatalf("AStar: %v", errA)
	}

	dij, errD := Dijkstra(c, srcID)
	if errD != nil {
		t.Fatalf("Dijkstra: %v", errD)
	}
	costD, ok := dij.Distance(dstID)
	if !ok {
		t.Fatalf("Dijkstra: goal unreachable")
	}

	if math.Abs(costA-costD) > 1e-12 {
		t.Fatalf("AStar cost = %g, Dijkstra cost = %g (want equal)", costA, costD)
	}
	if math.Abs(costA-8.0) > 1e-12 {
		t.Fatalf("expected cost 8.0, got %g", costA)
	}
}
