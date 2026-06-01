package search

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// TestDijkstra_ZeroWeightEdges verifies that Dijkstra handles graphs
// containing zero-weight edges without entering an infinite relaxation
// loop and produces correct shortest distances. Runs each case twice to
// verify determinism.
func TestDijkstra_ZeroWeightEdges(t *testing.T) {
	t.Parallel()

	t.Run("zero-weight-triangle", func(t *testing.T) {
		t.Parallel()
		// 0→1 (w=0), 1→2 (w=0), 0→2 (w=5)
		// Shortest path from 0: dist[1]=0, dist[2]=0 (via 0→1→2).
		a := adjlist.New[int, float64](adjlist.Config{Directed: true})
		for _, e := range []float64Edge{{0, 1, 0}, {1, 2, 0}, {0, 2, 5}} {
			if err := a.AddEdge(e.from, e.to, e.w); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		m := a.Mapper()

		id0, _ := m.Lookup(0)
		id1, _ := m.Lookup(1)
		id2, _ := m.Lookup(2)

		wantDist := map[graph.NodeID]float64{id0: 0, id1: 0, id2: 0}
		checkZeroTriangle := func(d *Distances[float64]) {
			t.Helper()
			for id, want := range wantDist {
				got, ok := d.Distance(id)
				if !ok {
					t.Errorf("node %v: unreachable, want dist=%v", id, want)
					continue
				}
				if got != want {
					t.Errorf("node %v: dist=%v, want %v", id, got, want)
				}
			}
		}

		d1, err := Dijkstra(c, id0)
		if err != nil {
			t.Fatalf("first Dijkstra: %v", err)
		}
		checkZeroTriangle(d1)

		d2, err := Dijkstra(c, id0)
		if err != nil {
			t.Fatalf("second Dijkstra: %v", err)
		}
		checkZeroTriangle(d2)

		// Explicit determinism check.
		for id := range wantDist {
			v1, _ := d1.Distance(id)
			v2, _ := d2.Distance(id)
			if v1 != v2 {
				t.Errorf("node %v: non-deterministic — run1=%v, run2=%v", id, v1, v2)
			}
		}
	})

	t.Run("zero-weight-chain-between-heavy-paths", func(t *testing.T) {
		t.Parallel()
		// 0→1 (w=10), 1→2 (w=0), 2→3 (w=0), 3→4 (w=5)
		// From 0: dist[4] = 10+0+0+5 = 15.
		a := adjlist.New[int, float64](adjlist.Config{Directed: true})
		for _, e := range []float64Edge{{0, 1, 10}, {1, 2, 0}, {2, 3, 0}, {3, 4, 5}} {
			if err := a.AddEdge(e.from, e.to, e.w); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
		}
		c := csr.BuildFromAdjList(a)
		m := a.Mapper()

		id0, _ := m.Lookup(0)
		id4, _ := m.Lookup(4)

		checkDist4 := func(d *Distances[float64]) {
			t.Helper()
			v, ok := d.Distance(id4)
			if !ok {
				t.Error("dist[4]: unreachable, want 15")
				return
			}
			if v != 15 {
				t.Errorf("dist[4]=%v, want 15", v)
			}
		}

		d1, err := Dijkstra(c, id0)
		if err != nil {
			t.Fatalf("first Dijkstra: %v", err)
		}
		checkDist4(d1)

		d2, err := Dijkstra(c, id0)
		if err != nil {
			t.Fatalf("second Dijkstra: %v", err)
		}
		checkDist4(d2)

		// Explicit determinism check.
		v1, _ := d1.Distance(id4)
		v2, _ := d2.Distance(id4)
		if v1 != v2 {
			t.Errorf("dist[4]: non-deterministic — run1=%v, run2=%v", v1, v2)
		}
	})
}
