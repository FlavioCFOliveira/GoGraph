package csr

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestCSR_HandlesSlice_AlignsWithEdges verifies that HandlesSlice() is the
// same length as EdgesSlice() and aligns slot-for-slot: the handle at
// position pos is the handle of the edge at edges[pos].
func TestCSR_HandlesSlice_AlignsWithEdges(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	// a→b (handle 1), a→b parallel (handle 2), a→c (handle 3).
	if err := a.AddEdgeH("a", "b", 0, 1); err != nil {
		t.Fatalf("AddEdgeH a→b#1: %v", err)
	}
	if err := a.AddEdgeH("a", "b", 0, 2); err != nil {
		t.Fatalf("AddEdgeH a→b#2: %v", err)
	}
	if err := a.AddEdgeH("a", "c", 0, 3); err != nil {
		t.Fatalf("AddEdgeH a→c: %v", err)
	}

	c := BuildFromAdjList(a)
	edges := c.EdgesSlice()
	handles := c.HandlesSlice()
	if handles == nil {
		t.Fatal("HandlesSlice() = nil for a graph built with AddEdgeH")
	}
	if len(handles) != len(edges) {
		t.Fatalf("len(handles)=%d, len(edges)=%d; want equal", len(handles), len(edges))
	}

	// Resolve a's adjacency range and assert handle/neighbour alignment.
	aID, _ := a.Mapper().Lookup("a")
	bID, _ := a.Mapper().Lookup("b")
	cID, _ := a.Mapper().Lookup("c")
	verts := c.VerticesSlice()
	start, end := verts[uint64(aID)], verts[uint64(aID)+1]
	if end-start != 3 {
		t.Fatalf("a out-degree = %d, want 3", end-start)
	}
	// Collect (neighbour → handle) pairs in slot order.
	seen := map[uint64]uint64{} // handle → neighbour-id
	for pos := start; pos < end; pos++ {
		seen[handles[pos]] = uint64(edges[pos])
	}
	if seen[1] != uint64(bID) || seen[2] != uint64(bID) {
		t.Fatalf("handles 1,2 should map to b (%d); got %d, %d", bID, seen[1], seen[2])
	}
	if seen[3] != uint64(cID) {
		t.Fatalf("handle 3 should map to c (%d); got %d", cID, seen[3])
	}
}

// TestCSR_HandlesSlice_NilForSimpleGraph verifies the lazy/opt-in column:
// a plain graph that never used AddEdgeH carries no handle column.
func TestCSR_HandlesSlice_NilForSimpleGraph(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge("b", "c", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := BuildFromAdjList(a)
	if h := c.HandlesSlice(); h != nil {
		t.Fatalf("HandlesSlice() = %v for plain AddEdge graph; want nil (lazy column)", h)
	}
}

// TestCSR_BuildReverse_CarriesHandles verifies that the reverse CSR carries
// the handle of each edge to its reversed slot, so a logical edge keeps one
// identity across both adjacency directions.
func TestCSR_BuildReverse_CarriesHandles(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
	if err := a.AddEdgeH("a", "b", 0, 42); err != nil {
		t.Fatalf("AddEdgeH a→b: %v", err)
	}
	if err := a.AddEdgeH("c", "b", 0, 43); err != nil {
		t.Fatalf("AddEdgeH c→b: %v", err)
	}

	fwd := BuildFromAdjList(a)
	rev := fwd.BuildReverse()
	revH := rev.HandlesSlice()
	if revH == nil {
		t.Fatal("reverse HandlesSlice() = nil; want carried handles")
	}
	revEdges := rev.EdgesSlice()
	revVerts := rev.VerticesSlice()
	if len(revH) != len(revEdges) {
		t.Fatalf("reverse len(handles)=%d, len(edges)=%d", len(revH), len(revEdges))
	}

	// b's reverse adjacency holds the two in-edges (from a, handle 42; from
	// c, handle 43). Verify each reversed slot carries the forward handle.
	aID, _ := a.Mapper().Lookup("a")
	bID, _ := a.Mapper().Lookup("b")
	cID, _ := a.Mapper().Lookup("c")
	start, end := revVerts[uint64(bID)], revVerts[uint64(bID)+1]
	got := map[uint64]uint64{} // source-id → handle
	for pos := start; pos < end; pos++ {
		got[uint64(revEdges[pos])] = revH[pos]
	}
	if got[uint64(aID)] != 42 {
		t.Fatalf("reverse edge b←a handle = %d, want 42", got[uint64(aID)])
	}
	if got[uint64(cID)] != 43 {
		t.Fatalf("reverse edge b←c handle = %d, want 43", got[uint64(cID)])
	}
}

// TestCSR_BuildReverse_NilHandlesForSimpleGraph verifies BuildReverse does
// not synthesise a handle column when the forward CSR has none.
func TestCSR_BuildReverse_NilHandlesForSimpleGraph(t *testing.T) {
	t.Parallel()
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	if err := a.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	rev := BuildFromAdjList(a).BuildReverse()
	if h := rev.HandlesSlice(); h != nil {
		t.Fatalf("reverse HandlesSlice() = %v for simple graph; want nil", h)
	}
}

// benchBuildAdj builds a directed graph of the given size; when withHandles
// is true every edge is stamped via AddEdgeH so BuildFromAdjList allocates
// and copies the handle column (the multigraph read-path cost).
func benchBuildAdj(b *testing.B, withHandles bool) *adjlist.AdjList[uint32, struct{}] {
	b.Helper()
	a := adjlist.New[uint32, struct{}](adjlist.Config{Directed: true, Multigraph: true})
	const universe = 1 << 16
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic benchmark RNG
	const fill = 200_000
	for i := 0; i < fill; i++ {
		src, dst := uint32(r.IntN(universe)), uint32(r.IntN(universe))
		var err error
		if withHandles {
			err = a.AddEdgeH(src, dst, struct{}{}, uint64(i+1))
		} else {
			err = a.AddEdge(src, dst, struct{}{})
		}
		if err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	return a
}

// BenchmarkCSR_Build_NoHandles measures BuildFromAdjList on a graph with no
// handle column (the unaffected simple-graph hot path).
func BenchmarkCSR_Build_NoHandles(b *testing.B) {
	a := benchBuildAdj(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = BuildFromAdjList(a)
	}
}

// BenchmarkCSR_Build_WithHandles measures BuildFromAdjList on a graph that
// carries the lazy handle column (multigraph read path).
func BenchmarkCSR_Build_WithHandles(b *testing.B) {
	a := benchBuildAdj(b, true)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_ = BuildFromAdjList(a)
	}
}
