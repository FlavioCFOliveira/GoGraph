package main

import (
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// Example pins the deterministic stdout of the routing-alternatives
// example. Go's test framework captures everything run writes to
// os.Stdout and compares it against the // Output: block below, so a
// future change that alters the report — including the A*-vs-Dijkstra
// expansion counts — is caught as a regression.
func Example() {
	_ = run(os.Stdout)
	// Output:
	// Dijkstra lisbon -> berlin: 2746 km
	//   route: lisbon -> madrid -> barcelona -> berlin
	//
	// Yen's 3 shortest paths lisbon -> berlin:
	//   1. 2746 km via lisbon -> madrid -> barcelona -> berlin
	//   2. 2952 km via lisbon -> madrid -> paris -> berlin
	//   3. 3003 km via lisbon -> porto -> madrid -> barcelona -> berlin
	//
	// A* lisbon -> berlin (coordinate-based Euclidean heuristic):
	//   cost = 2746 km, 3 hops
	//   route: lisbon -> madrid -> barcelona -> berlin
	//
	// Nodes expanded (lower is better):
	//   Dijkstra (zero heuristic) : 6
	//   A* (Euclidean heuristic)  : 4
	//   same optimal cost: true (2746 km)
}

// buildRoutingCSR builds the example graph and returns the immutable
// CSR snapshot together with its mapper.
func buildRoutingCSR(t *testing.T) (*csr.CSR[int64], *graph.Mapper[string]) {
	t.Helper()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for _, l := range roads {
		if err := a.AddEdge(l.from, l.to, l.w); err != nil {
			t.Fatalf("AddEdge %s->%s: %v", l.from, l.to, err)
		}
	}
	return csr.BuildFromAdjList(a), a.Mapper()
}

// TestHeuristicAdmissible verifies empirically that the coordinate
// heuristic is admissible: A* must return exactly the same optimal cost
// as Dijkstra for every queried pair. If they ever differ, the
// heuristic overestimates the true remaining distance for some node and
// the layout in main.go must be corrected.
func TestHeuristicAdmissible(t *testing.T) {
	c, mapper := buildRoutingCSR(t)

	pairs := []struct{ from, to string }{
		{"lisbon", "berlin"},
		{"lisbon", "paris"},
		{"lisbon", "barcelona"},
		{"porto", "berlin"},
		{"madrid", "berlin"},
	}
	for _, p := range pairs {
		src, ok := mapper.Lookup(p.from)
		if !ok {
			t.Fatalf("source %q not in graph", p.from)
		}
		dst, ok := mapper.Lookup(p.to)
		if !ok {
			t.Fatalf("destination %q not in graph", p.to)
		}

		d, err := search.Dijkstra(c, src)
		if err != nil {
			t.Fatalf("Dijkstra %s->%s: %v", p.from, p.to, err)
		}
		dijkstraCost, reachable := d.Distance(dst)
		if !reachable {
			t.Fatalf("%s->%s: unreachable under Dijkstra", p.from, p.to)
		}

		h, err := heuristic(mapper, p.to)
		if err != nil {
			t.Fatalf("heuristic for %s: %v", p.to, err)
		}
		_, astarCost, err := search.AStar(c, src, dst, h)
		if err != nil {
			t.Fatalf("AStar %s->%s: %v", p.from, p.to, err)
		}

		if astarCost != dijkstraCost {
			t.Errorf("%s->%s: A* cost %d != Dijkstra cost %d (heuristic not admissible)",
				p.from, p.to, astarCost, dijkstraCost)
		}
	}
}

// TestAStarExpandsNoMore verifies the demonstrated benefit: A* with the
// coordinate heuristic settles no more nodes than Dijkstra (A* with the
// zero heuristic) on the lisbon->berlin query, while reaching the same
// optimal cost.
func TestAStarExpandsNoMore(t *testing.T) {
	c, mapper := buildRoutingCSR(t)
	src, _ := mapper.Lookup("lisbon")
	dst, _ := mapper.Lookup("berlin")

	zeroH := func(graph.NodeID) int64 { return 0 }
	dijkstraExpanded, dijkstraCost, err := expansions(c, src, dst, zeroH)
	if err != nil {
		t.Fatalf("Dijkstra expansions: %v", err)
	}

	h, err := heuristic(mapper, "berlin")
	if err != nil {
		t.Fatalf("heuristic: %v", err)
	}
	astarExpanded, astarCost, err := expansions(c, src, dst, h)
	if err != nil {
		t.Fatalf("A* expansions: %v", err)
	}

	if astarCost != dijkstraCost {
		t.Errorf("A* cost %d != Dijkstra cost %d", astarCost, dijkstraCost)
	}
	if astarExpanded > dijkstraExpanded {
		t.Errorf("A* expanded %d nodes, more than Dijkstra's %d", astarExpanded, dijkstraExpanded)
	}
}
