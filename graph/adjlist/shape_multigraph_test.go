package adjlist_test

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestAdjList_Multigraph_ParallelDigon verifies that ParallelDigon(k)
// produces exactly k parallel directed edges from node 0 to node 1,
// and that Neighbours(0) yields k entries all pointing to node 1.
func TestAdjList_Multigraph_ParallelDigon(t *testing.T) {
	t.Parallel()

	for _, k := range []int{1, 2, 3, 5, 10, 100} {
		k := k
		t.Run("", func(t *testing.T) {
			t.Parallel()
			g, err := shapegen.ParallelDigon(k).Build(adjlist.Config{})
			if err != nil {
				t.Fatalf("ParallelDigon(%d) Build: %v", k, err)
			}
			a := g.AdjList()

			if got := a.Size(); got != uint64(k) {
				t.Errorf("k=%d: Size = %d, want %d", k, got, k)
			}

			count := 0
			for nb := range a.Neighbours(0) {
				count++
				if nb != 1 {
					t.Errorf("k=%d: Neighbours(0) yielded %v, want 1", k, nb)
				}
			}
			if count != k {
				t.Errorf("k=%d: Neighbours(0) count = %d, want %d", k, count, k)
			}
		})
	}
}

// TestAdjList_Multigraph_RemoveOneOfMany verifies that a single
// RemoveEdge call decrements Size by exactly one and removes exactly
// one occurrence from Neighbours, leaving the remaining k-1 entries
// intact.
func TestAdjList_Multigraph_RemoveOneOfMany(t *testing.T) {
	t.Parallel()

	const k = 5
	g, err := shapegen.ParallelDigon(k).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("ParallelDigon(%d) Build: %v", k, err)
	}
	a := g.AdjList()

	a.RemoveEdge(0, 1)

	if got := a.Size(); got != k-1 {
		t.Errorf("after RemoveEdge: Size = %d, want %d", got, k-1)
	}
	if got := degree(a, 0); got != k-1 {
		t.Errorf("after RemoveEdge: Neighbours(0) count = %d, want %d", got, k-1)
	}
}

// TestAdjList_Multigraph_InsertionOrder verifies that Neighbours
// returns weights in insertion order for a multigraph AdjList built
// directly (not via shapegen).
func TestAdjList_Multigraph_InsertionOrder(t *testing.T) {
	t.Parallel()

	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})

	for _, w := range []int64{10, 20, 30} {
		if err := a.AddEdge(0, 1, w); err != nil {
			t.Fatalf("AddEdge(0, 1, %d): %v", w, err)
		}
	}

	var weights []int64
	for _, w := range a.Neighbours(0) {
		weights = append(weights, w)
	}

	want := []int64{10, 20, 30}
	if len(weights) != len(want) {
		t.Fatalf("Neighbours(0) len = %d, want %d", len(weights), len(want))
	}
	for i, w := range want {
		if weights[i] != w {
			t.Errorf("weights[%d] = %d, want %d", i, weights[i], w)
		}
	}
}

// TestAdjList_Multigraph_StarParallel builds a directed multigraph
// star where every leaf is connected to the centre k times, and
// verifies Order, Size, and degree invariants.
func TestAdjList_Multigraph_StarParallel(t *testing.T) {
	t.Parallel()

	const n = 5 // number of leaves
	const k = 3 // parallel edges per leaf

	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})

	for leaf := 1; leaf <= n; leaf++ {
		for i := 0; i < k; i++ {
			if err := a.AddEdge(0, leaf, int64(i)); err != nil {
				t.Fatalf("AddEdge(0, %d, %d): %v", leaf, i, err)
			}
		}
	}

	if got := a.Order(); got != n+1 {
		t.Errorf("Order = %d, want %d", got, n+1)
	}
	if got := a.Size(); got != uint64(n*k) {
		t.Errorf("Size = %d, want %d", got, n*k)
	}
	if got := degree(a, 0); got != n*k {
		t.Errorf("degree(0) = %d, want %d", got, n*k)
	}
	for leaf := 1; leaf <= n; leaf++ {
		if got := degree(a, leaf); got != 0 {
			t.Errorf("degree(%d) = %d, want 0 (directed: leaves have no out-edges)", leaf, got)
		}
	}
}

// TestAdjList_Multigraph_CycleParallel builds a directed multigraph
// cycle C_n where each arc i→(i+1)%n is duplicated p times, and
// verifies Size and per-node out-degree.
func TestAdjList_Multigraph_CycleParallel(t *testing.T) {
	t.Parallel()

	const n = 4 // cycle length
	const p = 3 // parallel copies per arc

	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})

	for i := 0; i < n; i++ {
		for j := 0; j < p; j++ {
			if err := a.AddEdge(i, (i+1)%n, int64(j)); err != nil {
				t.Fatalf("AddEdge(%d, %d, %d): %v", i, (i+1)%n, j, err)
			}
		}
	}

	if got := a.Size(); got != uint64(n*p) {
		t.Errorf("Size = %d, want %d", got, n*p)
	}
	for i := 0; i < n; i++ {
		if got := degree(a, i); got != p {
			t.Errorf("out-degree(%d) = %d, want %d", i, got, p)
		}
	}
}

// TestAdjList_Multigraph_PropertyBased is a rapid property-based test
// that draws random node counts and edge sequences, inserts every edge,
// and verifies that Size equals the total number of AddEdge calls and
// that per-(src,dst) neighbour counts match the insertion frequency.
func TestAdjList_Multigraph_PropertyBased(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 10).Draw(rt, "n")
		m := rapid.IntRange(1, 50).Draw(rt, "m")

		a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})

		// edgeCount[src][dst] tracks how many times AddEdge(src, dst) was called.
		type endpoint struct{ src, dst int }
		edgeCount := make(map[endpoint]int, m)

		for i := 0; i < m; i++ {
			src := rapid.IntRange(0, n).Draw(rt, "src")
			dst := rapid.IntRange(0, n).Draw(rt, "dst")
			if err := a.AddEdge(src, dst, int64(i)); err != nil {
				rt.Fatalf("AddEdge(%d, %d, %d): %v", src, dst, i, err)
			}
			edgeCount[endpoint{src, dst}]++
		}

		// Total size must equal total number of AddEdge calls.
		if got := a.Size(); got != uint64(m) {
			rt.Errorf("Size = %d, want %d", got, m)
		}

		// For each (src, dst) pair, the count of neighbour entries
		// yielded by Neighbours(src) that equal dst must match how
		// many times we called AddEdge(src, dst).
		for ep, want := range edgeCount {
			got := 0
			for nb := range a.Neighbours(ep.src) {
				if nb == ep.dst {
					got++
				}
			}
			if got != want {
				rt.Errorf("Neighbours(%d) count of %d = %d, want %d",
					ep.src, ep.dst, got, want)
			}
		}
	})
}
