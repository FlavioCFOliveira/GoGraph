package search

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

func TestYen_KShortest(t *testing.T) {
	t.Parallel()
	// Two-path fixture: 0->1->3 (cost 4), 0->2->3 (cost 3).
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 2}, {1, 3, 2},
		{0, 2, 1}, {2, 3, 2},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	got := YenKShortest(c, src, dst, 2)
	if len(got) != 2 {
		t.Fatalf("got %d paths, want 2", len(got))
	}
	if got[0].Cost > got[1].Cost {
		t.Fatalf("paths not sorted by cost")
	}
	if got[0].Cost != 3 {
		t.Fatalf("first path cost = %d, want 3", got[0].Cost)
	}
}

// TestYenKShortest_WeightedMultigraph_MinParallelEdge is the regression
// gate for #1884. On a weighted multigraph, buildEdgeIndex must record the
// MINIMUM-weight parallel edge per (from,to) pair — the one both Yen
// Dijkstra passes actually traverse — not the first CSR occurrence. With
// the pre-fix first-occurrence indexing the 0->1 hop's root cost was
// charged at 100 (the expensive parallel edge) instead of the traversed 1,
// inflating [0 1 2 3] to cost 102 and mis-ranking the k-set as
// [0 1 3]=2, [0 3]=50, [0 1 2 3]=102. The correct k-set is
// [0 1 3]=2, [0 1 2 3]=3, [0 3]=50.
func TestYenKShortest_WeightedMultigraph_MinParallelEdge(t *testing.T) {
	t.Parallel()
	// The expensive 0->1 edge (100) is inserted first so it lands first in
	// CSR order; the cheap parallel 0->1 edge (1) is what a shortest-path
	// search uses. Requires Multigraph:true to retain both parallel edges.
	c, a := buildWeightedCSRCfg(t, []weightedEdge{
		{0, 1, 100}, // first CSR occurrence of the (0,1) pair
		{0, 1, 1},   // minimum-weight parallel edge
		{1, 2, 1},
		{2, 3, 1},
		{1, 3, 1},
		{0, 3, 50},
	}, adjlist.Config{Directed: true, Multigraph: true})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)

	got := YenKShortest(c, src, dst, 3)
	if len(got) != 3 {
		t.Fatalf("got %d paths, want 3 (paths: %v)", len(got), got)
	}
	// Correct minimum-weight realizations, sorted ascending by cost.
	wantCosts := []int64{2, 3, 50}
	for i, w := range wantCosts {
		if got[i].Cost != w {
			t.Fatalf("got[%d].Cost = %d, want %d (paths: %v)", i, got[i].Cost, w, got)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Cost > got[i].Cost {
			t.Fatalf("paths not sorted by cost: %v", got)
		}
	}
}

func TestYen_NoPath(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{{0, 1, 1}, {2, 3, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	if got := YenKShortest(c, src, dst, 3); len(got) != 0 {
		t.Fatalf("expected no paths, got %d", len(got))
	}
}

func TestYen_KZero(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{{0, 1, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(1)
	if got := YenKShortest(c, src, dst, 0); got != nil {
		t.Fatalf("k=0 must return nil")
	}
}

// TestYenKShortest_Int32WeightsNoOverflow asserts Yen produces
// correct shortest paths when the weight type is a 32-bit integer.
// An earlier in-band Inf sentinel built by repeated doubling wrapped
// to 0 on int32 and silently corrupted unreachable distances.
func TestYenKShortest_Int32WeightsNoOverflow(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int32](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 3); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, 4); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(0, 2, 10); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 3, 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	id0, _ := a.Mapper().Lookup(0)
	id3, _ := a.Mapper().Lookup(3)
	paths := YenKShortest(c, id0, id3, 3)
	if len(paths) == 0 {
		t.Fatal("Yen returned no paths on int32-weighted graph")
	}
	// Shortest path is 0->1->2->3 with cost 3+4+1 = 8.
	if paths[0].Cost != 8 {
		t.Fatalf("paths[0].Cost = %d, want 8", paths[0].Cost)
	}
}

// TestYenKShortest_UnreachableReturnsNil asserts Yen returns nil
// when the source cannot reach the destination, without relying on
// any sentinel comparison.
func TestYenKShortest_UnreachableReturnsNil(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 3, 1); err != nil { // disjoint component
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	id0, _ := a.Mapper().Lookup(0)
	id3, _ := a.Mapper().Lookup(3)
	paths := YenKShortest(c, id0, id3, 3)
	if paths != nil {
		t.Fatalf("expected nil for unreachable target, got %v", paths)
	}
}

// BenchmarkYen_K100 measures the steady-state allocation profile of
// 100-shortest-paths on a moderate random graph. Task #124 sets a
// regression budget of <10% of v1.0 allocations; we report
// ReportAllocs() so the regression is visible in benchstat output.
func BenchmarkYen_K100(b *testing.B) {
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	const n = 256
	r := rand.New(rand.NewPCG(7, 13)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < 4*n; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(50)+1)); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	srcID, _ := a.Mapper().Lookup(0)
	dstID, _ := a.Mapper().Lookup(n - 1)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = YenKShortest(c, srcID, dstID, 100)
	}
}
