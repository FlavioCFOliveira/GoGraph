package search

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

func TestFloydWarshall_HandBuilt(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	})
	apsp := FloydWarshall(c)
	src, _ := a.Mapper().Lookup(0)
	cases := map[int]int64{0: 0, 1: 7, 2: 3, 3: 8, 4: 5}
	for k, expected := range cases {
		id, _ := a.Mapper().Lookup(k)
		v, ok := apsp.At(src, id)
		if !ok || v != expected {
			t.Fatalf("d(0,%d) = (%d, %v), want %d", k, v, ok, expected)
		}
	}
}

func TestFloydWarshall_Unreachable(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR([]weightedEdge{{0, 1, 1}, {2, 3, 1}})
	apsp := FloydWarshall(c)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	if _, ok := apsp.At(src, dst); ok {
		t.Fatalf("(0,3) should be unreachable")
	}
}

// TestFloydWarshall_Int32WeightsNoOverflow asserts FW returns correct
// distances on an int32-weighted graph. The v1.0.0 sentinel wrapped
// on int32 and corrupted unreachable-pair detection.
func TestFloydWarshall_Int32WeightsNoOverflow(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int32](adjlist.Config{Directed: true})
	a.AddEdge(0, 1, 5)
	a.AddEdge(1, 2, 3)
	a.AddEdge(0, 2, 100)
	c := csr.BuildFromAdjList(a)
	apsp := FloydWarshall(c)
	id0, _ := a.Mapper().Lookup(0)
	id1, _ := a.Mapper().Lookup(1)
	id2, _ := a.Mapper().Lookup(2)
	d02, ok := apsp.At(id0, id2)
	if !ok || d02 != 8 {
		t.Fatalf("d(0,2) = %d ok=%v, want 8 ok=true", d02, ok)
	}
	if _, ok := apsp.At(id1, id0); ok {
		t.Fatalf("d(1,0) ok=true, want unreachable")
	}
}

// TestFloydWarshall_UnreachableReportedExplicitly covers the
// found[] bitmap path: disconnected components must produce
// (zero, false) not (sentinel, true).
func TestFloydWarshall_UnreachableReportedExplicitly(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	a.AddEdge(0, 1, 1)
	a.AddEdge(2, 3, 1)
	c := csr.BuildFromAdjList(a)
	apsp := FloydWarshall(c)
	id0, _ := a.Mapper().Lookup(0)
	id3, _ := a.Mapper().Lookup(3)
	if _, ok := apsp.At(id0, id3); ok {
		t.Fatalf("expected unreachable on disjoint components")
	}
}
