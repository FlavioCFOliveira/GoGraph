package search

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// BenchmarkFloydWarshall_V2048 is the headline benchmark for task
// #132. The blocked (i, j) tiling must beat the unblocked baseline
// by >2x at V=2048 — the working set crosses L1 in the unblocked
// variant, becoming DRAM-bandwidth-bound.
func BenchmarkFloydWarshall_V2048(b *testing.B) {
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	const n = 2048
	r := rand.New(rand.NewPCG(73, 79)) //nolint:gosec // deterministic benchmark RNG
	for i := 0; i < 4*n; i++ {
		if err := a.AddEdge(r.IntN(n), r.IntN(n), int64(r.IntN(100)+1)); err != nil {
			b.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FloydWarshall(c)
	}
}

func TestFloydWarshall_HandBuilt(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 10}, {0, 2, 3},
		{1, 3, 1},
		{2, 1, 4}, {2, 3, 8}, {2, 4, 2},
		{3, 4, 7},
	})
	apsp := FloydWarshall(c)
	src, _ := a.Mapper().Lookup(0)
	cases := []struct {
		name     string
		k        int
		expected int64
	}{
		{"to-self", 0, 0},
		{"to-1", 1, 7},
		{"to-2", 2, 3},
		{"to-3", 3, 8},
		{"to-4", 4, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, _ := a.Mapper().Lookup(tc.k)
			v, ok := apsp.At(src, id)
			if !ok || v != tc.expected {
				t.Fatalf("d(0,%d) = (%d, %v), want %d", tc.k, v, ok, tc.expected)
			}
		})
	}
}

func TestFloydWarshall_Unreachable(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{{0, 1, 1}, {2, 3, 1}})
	apsp := FloydWarshall(c)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(3)
	if _, ok := apsp.At(src, dst); ok {
		t.Fatalf("(0,3) should be unreachable")
	}
}

// TestFloydWarshall_Int32WeightsNoOverflow asserts FW returns correct
// distances on an int32-weighted graph. An earlier sentinel wrapped
// on int32 and corrupted unreachable-pair detection.
func TestFloydWarshall_Int32WeightsNoOverflow(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int32](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 5); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(1, 2, 3); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(0, 2, 100); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
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
	if err := a.AddEdge(0, 1, 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(2, 3, 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	apsp := FloydWarshall(c)
	id0, _ := a.Mapper().Lookup(0)
	id3, _ := a.Mapper().Lookup(3)
	if _, ok := apsp.At(id0, id3); ok {
		t.Fatalf("expected unreachable on disjoint components")
	}
}
