package search

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestDijkstra_DisconnectedForest verifies that Dijkstra correctly marks
// vertices in disconnected components as unreachable while producing
// correct distances for the source component.
//
// Forest layout:
//
//	Component A (src=0): path 0→1 (w=1.0), 1→2 (w=2.0).
//	Component B:         star centre=10, leaves 11, 12 (w=1.0 each).
//	Component C:         triangle 20→21→22→20 (w=1.0 each).
//
// From src=0: A is fully reachable with dist=[0,1,3].
// Nodes in B and C must return (0.0, false) from Distances.Distance.
func TestDijkstra_DisconnectedForest(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, float64](adjlist.Config{Directed: true})
	addE := func(from, to int, w float64) {
		if err := a.AddEdge(from, to, w); err != nil {
			t.Fatalf("AddEdge(%d→%d, %v): %v", from, to, w, err)
		}
	}

	// Component A.
	addE(0, 1, 1.0)
	addE(1, 2, 2.0)

	// Component B.
	addE(10, 11, 1.0)
	addE(10, 12, 1.0)

	// Component C.
	addE(20, 21, 1.0)
	addE(21, 22, 1.0)
	addE(22, 20, 1.0)

	c := csr.BuildFromAdjList(a)
	m := a.Mapper()

	id0, _ := m.Lookup(0)
	id1, _ := m.Lookup(1)
	id2, _ := m.Lookup(2)

	d, err := Dijkstra(c, id0)
	if err != nil {
		t.Fatalf("Dijkstra: %v", err)
	}

	// Component A: correct distances.
	wantA := map[int]float64{0: 0, 1: 1.0, 2: 3.0}
	for key, want := range wantA {
		nodeID, _ := m.Lookup(key)
		got, ok := d.Distance(nodeID)
		if !ok {
			t.Errorf("node %d: unreachable, want dist=%v", key, want)
			continue
		}
		if got != want {
			t.Errorf("node %d: dist=%v, want %v", key, got, want)
		}
	}
	_ = id1
	_ = id2

	// Component B: unreachable.
	for _, key := range []int{10, 11, 12} {
		nodeID, _ := m.Lookup(key)
		v, ok := d.Distance(nodeID)
		if ok {
			t.Errorf("node %d (component B): reported reachable with dist=%v, want unreachable", key, v)
		}
		if v != 0.0 {
			t.Errorf("node %d (component B): zero value for unreachable should be 0.0, got %v", key, v)
		}
	}

	// Component C: unreachable.
	for _, key := range []int{20, 21, 22} {
		nodeID, _ := m.Lookup(key)
		v, ok := d.Distance(nodeID)
		if ok {
			t.Errorf("node %d (component C): reported reachable with dist=%v, want unreachable", key, v)
		}
		if v != 0.0 {
			t.Errorf("node %d (component C): zero value for unreachable should be 0.0, got %v", key, v)
		}
	}

	// Verify that NodeIDs not in the CSR also return (0.0, false).
	ghost := graph.NodeID(999)
	v, ok := d.Distance(ghost)
	if ok {
		t.Errorf("ghost NodeID 999: reported reachable with dist=%v", v)
	}
}
