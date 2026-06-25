package search

// astar_overflow_test.go — regression gate for the 2026-06-25 reliability
// audit finding #1763: the A* priority-queue key f = g + h has no overflow
// guard for an integer Weight. At extreme magnitudes f wraps, which can only
// mis-order exploration — it must NOT corrupt the RETURNED cost, because the
// cost is dist[dst] (the true g-value), tracked separately from the f-score.
// This pins that documented cost-correctness invariant.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

func TestAStar_FScoreOverflow_CostStaysCorrect(t *testing.T) {
	t.Parallel()
	// 0 --7e18--> 1 --1--> 2.  Heuristic h(1)=7e18, so f(1)=g(1)+h(1)=1.4e19
	// overflows int64 (max ≈ 9.22e18) and wraps negative.
	const big = int64(7_000_000_000_000_000_000)
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, big},
		{1, 2, 1},
	})
	src, _ := a.Mapper().Lookup(0)
	mid, _ := a.Mapper().Lookup(1)
	dst, _ := a.Mapper().Lookup(2)

	h := func(n graph.NodeID) int64 {
		if n == mid {
			return big // forces f(mid) = big + big to overflow
		}
		return 0
	}

	path, cost, err := AStar(c, src, dst, h)
	if err != nil {
		t.Fatalf("AStar: %v", err)
	}
	// The true shortest-path cost is big + 1, regardless of the f-score wrap.
	if want := big + 1; cost != want {
		t.Fatalf("cost = %d, want %d (f-score overflow must not corrupt the returned g-value)", cost, want)
	}
	if len(path) != 3 {
		t.Fatalf("path length = %d, want 3 (0->1->2)", len(path))
	}
}
