package search

import (
	"math/rand/v2"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestYen_KShortest(t *testing.T) {
	t.Parallel()
	// Two-path fixture: 0->1->3 (cost 4), 0->2->3 (cost 3).
	c, a := buildWeightedCSR([]weightedEdge{
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

func TestYen_NoPath(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 1}, {2, 3, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	if got := YenKShortest(c, src, dst, 3); len(got) != 0 {
		t.Fatalf("expected no paths, got %d", len(got))
	}
}

func TestYen_KZero(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 1}})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(1)
	if got := YenKShortest(c, src, dst, 0); got != nil {
		t.Fatalf("k=0 must return nil")
	}
}

// TestYenKShortest_Int32WeightsNoOverflow asserts Yen produces
// correct shortest paths when the weight type is a 32-bit integer.
// The v1.0.0 in-band Inf sentinel built by repeated doubling wrapped
// to 0 on int32 and silently corrupted unreachable distances.
func TestYenKShortest_Int32WeightsNoOverflow(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int32](adjlist.Config{Directed: true})
	a.AddEdge(0, 1, 3)
	a.AddEdge(1, 2, 4)
	a.AddEdge(0, 2, 10)
	a.AddEdge(2, 3, 1)
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
	a.AddEdge(0, 1, 1)
	a.AddEdge(2, 3, 1) // disjoint component
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
		a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(50)+1))
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
