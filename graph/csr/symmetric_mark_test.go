package csr

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// randomMultigraph builds a random directed multigraph over n keys with
// self-loops, parallel and reciprocal arcs. withHandles routes every arc
// through AddEdgeH so the handle column is populated; weightless puts the
// adjacency in weightless mode.
func randomMultigraph(t *testing.T, r *rand.Rand, n int, withHandles, weightless bool) *adjlist.AdjList[int, int64] {
	t.Helper()
	a := adjlist.New[int, int64](adjlist.Config{Weightless: weightless})
	m := r.IntN(6*n + 1)
	for i := 0; i < m; i++ {
		u := r.IntN(n)
		v := r.IntN(n)
		if r.IntN(10) == 0 {
			v = u
		}
		w := r.Int64N(1000)
		var err error
		if withHandles {
			err = a.AddEdgeH(u, v, w, uint64(i+1))
		} else {
			err = a.AddEdge(u, v, w)
		}
		if err != nil {
			t.Fatalf("add arc %d->%d: %v", u, v, err)
		}
	}
	return a
}

// TestBuildReverse_OfSymmetricIsNotCopied proves BuildReverse of a CSR
// marked symmetric returns the same snapshot with no allocation, and that
// the alias is sound: the full transpose of an unmarked copy equals it in
// every column.
func TestBuildReverse_OfSymmetricIsNotCopied(t *testing.T) {
	// Not parallel: testing.AllocsPerRun refuses to run in a parallel test.
	r := rand.New(rand.NewPCG(41, 43)) //nolint:gosec // deterministic test RNG
	for trial := 0; trial < 200; trial++ {
		a := randomMultigraph(t, r, 1+r.IntN(60), trial%2 == 0, false)
		c := BuildFromAdjList(a).BuildSymmetric()
		if rev := c.BuildReverse(); rev != c {
			t.Fatalf("trial %d: BuildReverse of a symmetric CSR returned a copy", trial)
		}
		if allocs := testing.AllocsPerRun(10, func() { _ = c.BuildReverse() }); allocs != 0 {
			t.Fatalf("trial %d: BuildReverse of a symmetric CSR allocated %.0f objects, want 0", trial, allocs)
		}
		unmarked := *c
		unmarked.symmetric = false
		full := unmarked.BuildReverse()
		if full == &unmarked {
			t.Fatalf("trial %d: BuildReverse of an unmarked CSR returned its receiver", trial)
		}
		if !slices.Equal(full.vertices, c.vertices) || !slices.Equal(full.edges, c.edges) ||
			!slices.Equal(full.weights, c.weights) || !slices.Equal(full.handles, c.handles) ||
			full.order != c.order || full.size != c.size {
			t.Fatalf("trial %d: transpose of the symmetric projection differs from it; the alias would change results", trial)
		}
	}
}

// TestKnownSymmetric_OnlyBySymmetricBuilders pins the mark's scope: only
// BuildSymmetric sets it, a directed build and its transpose do
// not, and IsSymmetric answers a marked CSR without allocating.
func TestKnownSymmetric_OnlyBySymmetricBuilders(t *testing.T) {
	// Not parallel: testing.AllocsPerRun refuses to run in a parallel test.
	a := adjlist.New[int, int64](adjlist.Config{})
	for _, e := range [][2]int{{0, 1}, {1, 2}, {2, 0}, {2, 2}} {
		if err := a.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	dir := BuildFromAdjList(a)
	if dir.KnownSymmetric() || dir.BuildReverse().KnownSymmetric() {
		t.Fatal("a directed build or its transpose is marked symmetric")
	}
	if FromArrays(dir.VerticesSlice(), dir.EdgesSlice(), dir.WeightsSlice(), dir.Order(), dir.Size()).KnownSymmetric() {
		t.Fatal("FromArrays result is marked symmetric")
	}
	sym := dir.BuildSymmetric()
	if !sym.KnownSymmetric() {
		t.Fatal("a symmetric build is not marked symmetric")
	}
	if allocs := testing.AllocsPerRun(10, func() {
		if !sym.IsSymmetric() {
			t.Fatal("IsSymmetric false on a symmetric build")
		}
	}); allocs != 0 {
		t.Fatalf("IsSymmetric on a marked CSR allocated %.0f objects, want 0", allocs)
	}
	empty := (&CSR[int64]{vertices: []uint64{0}}).BuildSymmetric()
	if !empty.KnownSymmetric() {
		t.Fatal("empty symmetric build is not marked symmetric")
	}
}
