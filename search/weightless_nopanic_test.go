package search

// weightless_nopanic_test.go — regression gate for the 2026-06-25 round-3 audit
// finding #1776: the shortest-path relaxer family panicked on a weightless-mode
// CSR (csr.BuildFromAdjList over a Weightless adjlist → nil weights slice) with
// at least one edge. Every public shortest-path entry must treat the absent
// weight column as the zero weight (CLRS §24 unweighted = W≡0), never panic.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestShortestPath_WeightlessCSR_NoPanic(t *testing.T) {
	t.Parallel()
	// Weightless graph 0->1->2 (nil weights column); a live edge-bearing source.
	a := adjlist.New[int, int](adjlist.Config{Directed: true, Weightless: true})
	if err := a.AddEdge(0, 1, 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList[int, int](a)
	if c.WeightsSlice() != nil {
		t.Fatalf("precondition: expected a nil weights slice for a weightless CSR, got %v", c.WeightsSlice())
	}
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(2)
	zeroH := func(graph.NodeID) int { return 0 }

	// Each entry must run without panicking; all-zero weights ⇒ dist(0→2)=0.
	t.Run("Dijkstra", func(t *testing.T) {
		d, err := Dijkstra[int](c, src)
		if err != nil {
			t.Fatalf("Dijkstra: %v", err)
		}
		if w, ok := d.Distance(dst); !ok || w != 0 {
			t.Errorf("Dijkstra dist(0→2) = %v,%v, want 0,true", w, ok)
		}
	})
	t.Run("BellmanFord", func(t *testing.T) {
		d, err := BellmanFord[int](c, src)
		if err != nil {
			t.Fatalf("BellmanFord: %v", err)
		}
		if w, ok := d.Distance(dst); !ok || w != 0 {
			t.Errorf("BellmanFord dist(0→2) = %v,%v, want 0,true", w, ok)
		}
	})
	t.Run("AStar", func(t *testing.T) {
		_, cost, err := AStar[int](c, src, dst, zeroH)
		if err != nil {
			t.Fatalf("AStar: %v", err)
		}
		if cost != 0 {
			t.Errorf("AStar cost(0→2) = %v, want 0", cost)
		}
	})
	t.Run("BidirectionalDijkstra", func(t *testing.T) {
		_, cost, err := BidirectionalDijkstra[int](c, src, dst)
		if err != nil {
			t.Fatalf("BidirectionalDijkstra: %v", err)
		}
		if cost != 0 {
			t.Errorf("BidirectionalDijkstra cost(0→2) = %v, want 0", cost)
		}
	})
	t.Run("DijkstraAPSP", func(t *testing.T) {
		apsp, err := DijkstraAPSP[int](c)
		if err != nil {
			t.Fatalf("DijkstraAPSP: %v", err)
		}
		if w, ok := apsp.At(src, dst); !ok || w != 0 {
			t.Errorf("DijkstraAPSP At(0,2) = %v,%v, want 0,true", w, ok)
		}
	})
	t.Run("JohnsonAPSP", func(t *testing.T) {
		apsp, err := JohnsonAPSP[int](c)
		if err != nil {
			t.Fatalf("JohnsonAPSP: %v", err)
		}
		if w, ok := apsp.At(src, dst); !ok || w != 0 {
			t.Errorf("JohnsonAPSP At(0,2) = %v,%v, want 0,true", w, ok)
		}
	})
}
