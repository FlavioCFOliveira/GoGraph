package centrality

import (
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestPageRankReverseStructure_SymmetricAliases proves the pull path reads a
// CSR marked symmetric as its own transpose — no copy — and that this is
// exact: the transpose it skips is bit-identical, and PageRank over the
// marked CSR equals PageRank over an unmarked copy of the same arrays.
func TestPageRankReverseStructure_SymmetricAliases(t *testing.T) {
	g, err := shapegen.BarabasiAlbert(3000, 4, 5).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("BarabasiAlbert.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList()).BuildSymmetric()
	verts, edges := c.VerticesSlice(), c.EdgesSlice()
	n := len(verts) - 1
	rv, re := pageRankReverseStructure(c, verts, edges, n)
	if &rv[0] != &verts[0] || &re[0] != &edges[0] {
		t.Fatal("pageRankReverseStructure copied a CSR marked symmetric")
	}
	fv, fe := pageRankBuildReverseStructure(verts, edges, n)
	if !slices.Equal(fv, verts) || !slices.Equal(fe, edges) {
		t.Fatal("the transpose of the symmetric projection differs from it; aliasing would change ranks")
	}
	unmarked := csr.FromArrays(verts, edges, c.WeightsSlice(), c.Order(), c.Size())
	if unmarked.KnownSymmetric() {
		t.Fatal("FromArrays copy is marked symmetric")
	}
	opts := PageRankOptions{Damping: 0.85, MaxIterations: 40, Tolerance: 1e-9}
	got, gotIt, err := PageRank(c, opts)
	if err != nil {
		t.Fatalf("PageRank(marked): %v", err)
	}
	want, wantIt, err := PageRank(unmarked, opts)
	if err != nil {
		t.Fatalf("PageRank(unmarked): %v", err)
	}
	if gotIt != wantIt || !slices.Equal(got, want) {
		t.Fatalf("ranks differ between the marked and unmarked CSR (iterations %d vs %d)", gotIt, wantIt)
	}
}
