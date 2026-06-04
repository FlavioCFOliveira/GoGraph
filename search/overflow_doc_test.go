package search

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// int32PathCSR builds a directed path 0->1->...->n-1 with the given
// per-hop int32 weight and returns the CSR plus the NodeIDs of the
// endpoints in path order, so a test can assert distance monotonicity
// along the chain.
func int32PathCSR(t *testing.T, n int, perHop int32) (*csr.CSR[int32], []graph.NodeID) {
	t.Helper()
	a := adjlist.New[int, int32](adjlist.Config{Directed: true})
	for i := 0; i < n-1; i++ {
		if err := a.AddEdge(i, i+1, perHop); err != nil {
			t.Fatalf("AddEdge(%d->%d): %v", i, i+1, err)
		}
	}
	c := csr.BuildFromAdjList(a)
	ids := make([]graph.NodeID, n)
	for i := 0; i < n; i++ {
		id, ok := a.Mapper().Lookup(i)
		if !ok {
			t.Fatalf("Lookup(%d) failed", i)
		}
		ids[i] = id
	}
	return c, ids
}

// TestShortestPath_Int32InRangeMonotone documents the integer-W
// precondition from the satisfied side: when the cumulative weight of
// the path stays within int32, the reported distance is strictly
// monotone increasing along the chain — i.e. there is no silent
// wraparound. This is the in-range control half of the #1323 acceptance
// criterion; the overflow-assertion half lives in
// overflow_assert_debug_test.go (built with -tags gograph_debug).
//
// 100 hops of 1,000,000 sum to 100,000,000, comfortably inside
// int32's 2,147,483,647 ceiling.
func TestShortestPath_Int32InRangeMonotone(t *testing.T) {
	t.Parallel()
	const (
		n      = 101
		perHop = int32(1_000_000)
	)
	c, ids := int32PathCSR(t, n, perHop)

	check := func(t *testing.T, dist func(graph.NodeID) (int32, bool)) {
		prev := int32(-1)
		for i := 0; i < n; i++ {
			d, ok := dist(ids[i])
			if !ok {
				t.Fatalf("node %d reported unreachable", i)
			}
			if d <= prev {
				t.Fatalf("distance not monotone at hop %d: prev=%d, got=%d (wraparound?)", i, prev, d)
			}
			if want := int32(i) * perHop; d != want {
				t.Fatalf("distance at hop %d = %d, want %d", i, d, want)
			}
			prev = d
		}
	}

	t.Run("dijkstra", func(t *testing.T) {
		t.Parallel()
		d, err := Dijkstra(c, ids[0])
		if err != nil {
			t.Fatalf("Dijkstra: %v", err)
		}
		check(t, d.Distance)
	})

	t.Run("bellman-ford", func(t *testing.T) {
		t.Parallel()
		d, err := BellmanFord(c, ids[0])
		if err != nil {
			t.Fatalf("BellmanFord: %v", err)
		}
		check(t, d.Distance)
	})
}
