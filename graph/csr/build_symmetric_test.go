package csr

import (
	"math/rand/v2"
	"slices"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// wantCSR is the exact shape of a CSR, used both as a table input and as the
// expected BuildSymmetric result.
type wantCSR struct {
	vertices []uint64
	edges    []graph.NodeID
	weights  []int64
	handles  []uint64
	size     uint64
}

// TestBuildSymmetric_Table pins the projection rules slot for slot: a non-loop
// arc is mirrored with its weight and handle, a self-loop is kept once,
// parallel arcs and the two arcs of a reciprocal pair stay distinct, and every
// run is ordered by (destination, handle).
func TestBuildSymmetric_Table(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   wantCSR
		want wantCSR
	}{
		{
			name: "empty",
			in:   wantCSR{vertices: []uint64{0}},
			want: wantCSR{vertices: []uint64{0}},
		},
		{
			name: "self-loop kept once",
			in:   wantCSR{vertices: []uint64{0, 1}, edges: []graph.NodeID{0}, weights: []int64{7}, handles: []uint64{1}, size: 1},
			want: wantCSR{
				vertices: []uint64{0, 1},
				edges:    []graph.NodeID{0},
				weights:  []int64{7},
				handles:  []uint64{1},
				size:     1,
			},
		},
		{
			name: "single arc mirrored with weight and handle",
			in:   wantCSR{vertices: []uint64{0, 1, 1}, edges: []graph.NodeID{1}, weights: []int64{5}, handles: []uint64{1}, size: 1},
			want: wantCSR{
				vertices: []uint64{0, 1, 2},
				edges:    []graph.NodeID{1, 0},
				weights:  []int64{5, 5},
				handles:  []uint64{1, 1},
				size:     2,
			},
		},
		{
			name: "parallel arcs stay distinct",
			// Input run deliberately ordered by DESCENDING handle.
			in: wantCSR{vertices: []uint64{0, 2, 2}, edges: []graph.NodeID{1, 1}, weights: []int64{20, 10}, handles: []uint64{2, 1}, size: 2},
			want: wantCSR{
				vertices: []uint64{0, 2, 4},
				edges:    []graph.NodeID{1, 1, 0, 0},
				weights:  []int64{10, 20, 10, 20},
				handles:  []uint64{1, 2, 1, 2},
				size:     4,
			},
		},
		{
			name: "reciprocal pair stays two relationships",
			in:   wantCSR{vertices: []uint64{0, 1, 2}, edges: []graph.NodeID{1, 0}, weights: []int64{10, 20}, handles: []uint64{1, 2}, size: 2},
			want: wantCSR{
				vertices: []uint64{0, 2, 4},
				edges:    []graph.NodeID{1, 1, 0, 0},
				weights:  []int64{10, 20, 10, 20},
				handles:  []uint64{1, 2, 1, 2},
				size:     4,
			},
		},
		{
			name: "self-loop beside a mirrored arc",
			in:   wantCSR{vertices: []uint64{0, 0, 3, 3}, edges: []graph.NodeID{0, 1, 2}, weights: []int64{20, 10, 30}, handles: []uint64{2, 1, 3}, size: 3},
			want: wantCSR{
				vertices: []uint64{0, 1, 4, 5},
				edges:    []graph.NodeID{1, 0, 1, 2, 1},
				weights:  []int64{20, 20, 10, 30, 30},
				handles:  []uint64{2, 2, 1, 3, 3},
				size:     5,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const order = 3 // any value: BuildSymmetric carries Order over unchanged
			in := &CSR[int64]{
				vertices: tc.in.vertices,
				edges:    tc.in.edges,
				weights:  tc.in.weights,
				handles:  tc.in.handles,
				order:    order,
				size:     tc.in.size,
			}
			got := in.BuildSymmetric()
			assertCSR(t, got, &tc.want, order)
			if !got.IsSymmetric() {
				t.Fatal("IsSymmetric() = false on a BuildSymmetric result")
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// TestBuildSymmetric_FromArraysWithoutHandles covers an input with no handle
// column and an UNORDERED run: the result carries no handle column, keeps the
// weights aligned, and orders every run by destination.
func TestBuildSymmetric_FromArraysWithoutHandles(t *testing.T) {
	t.Parallel()
	// 0 -> {2, 1}: run deliberately not ordered.
	in := FromArrays[int64]([]uint64{0, 2, 2, 2}, []graph.NodeID{2, 1}, []int64{20, 10}, 3, 2)
	got := in.BuildSymmetric()
	assertCSR(t, got, &wantCSR{
		vertices: []uint64{0, 2, 3, 4},
		edges:    []graph.NodeID{1, 2, 0, 0},
		weights:  []int64{10, 20, 10, 20},
		size:     4,
	}, 3)
	if !got.IsSymmetric() {
		t.Fatal("IsSymmetric() = false on a BuildSymmetric result")
	}
}

// TestBuildSymmetric_MatchesMirroredAdjList is the equivalence property: for
// a random directed edge list, BuildSymmetric of the directed CSR equals,
// array for array, the CSR of an undirected multigraph built by hand: a
// directed multigraph adjlist fed every edge followed, for a non-loop edge, by
// its mirror carrying the same weight and the same handle. That is the layout
// the storage-level undirected mode had before storage became directed-only.
// Both adjlists mint handles from the same sequence, so the handle columns are
// compared too. It also checks that the input snapshot is not mutated.
func TestBuildSymmetric_MatchesMirroredAdjList(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 12).Draw(rt, "n")
		m := rapid.IntRange(0, 3*n).Draw(rt, "m")
		seed := rapid.Uint64().Draw(rt, "seed")
		rng := rand.New(rand.NewPCG(seed, seed^0x3071)) //nolint:gosec // deterministic test RNG

		dir := adjlist.New[int, int64](adjlist.Config{})
		und := adjlist.New[int, int64](adjlist.Config{})
		for i := range m {
			src, dst, w := rng.IntN(n), rng.IntN(n), int64(i)
			if err := dir.AddEdge(src, dst, w); err != nil {
				rt.Fatalf("directed AddEdge: %v", err)
			}
			h := und.NextHandle()
			if err := und.AddEdgeH(src, dst, w, h); err != nil {
				rt.Fatalf("mirrored AddEdgeH: %v", err)
			}
			if src != dst {
				if err := und.AddEdgeH(dst, src, w, h); err != nil {
					rt.Fatalf("mirror AddEdgeH: %v", err)
				}
			}
		}

		in := BuildFromAdjList(dir)
		inVerts := slices.Clone(in.VerticesSlice())
		inEdges := slices.Clone(in.EdgesSlice())
		inWeights := slices.Clone(in.WeightsSlice())
		inHandles := slices.Clone(in.HandlesSlice())

		got := in.BuildSymmetric()
		want := BuildFromAdjList(und)

		if !got.IsSymmetric() {
			rt.Fatal("IsSymmetric() = false on a BuildSymmetric result")
		}
		if got.Order() != want.Order() || got.Size() != want.Size() {
			rt.Fatalf("order/size = %d/%d, want %d/%d", got.Order(), got.Size(), want.Order(), want.Size())
		}
		if !slices.Equal(got.VerticesSlice(), want.VerticesSlice()) {
			rt.Fatalf("vertices = %v, want %v", got.VerticesSlice(), want.VerticesSlice())
		}
		if !slices.Equal(got.EdgesSlice(), want.EdgesSlice()) {
			rt.Fatalf("edges = %v, want %v", got.EdgesSlice(), want.EdgesSlice())
		}
		if !slices.Equal(got.WeightsSlice(), want.WeightsSlice()) {
			rt.Fatalf("weights = %v, want %v", got.WeightsSlice(), want.WeightsSlice())
		}
		if !slices.Equal(got.HandlesSlice(), want.HandlesSlice()) {
			rt.Fatalf("handles = %v, want %v", got.HandlesSlice(), want.HandlesSlice())
		}

		if !slices.Equal(in.VerticesSlice(), inVerts) || !slices.Equal(in.EdgesSlice(), inEdges) ||
			!slices.Equal(in.WeightsSlice(), inWeights) || !slices.Equal(in.HandlesSlice(), inHandles) {
			rt.Fatal("BuildSymmetric mutated its input snapshot")
		}
	})
}

// assertCSR compares every array of got against want, and Order against order.
func assertCSR(t *testing.T, got *CSR[int64], want *wantCSR, order uint64) {
	t.Helper()
	if !slices.Equal(got.VerticesSlice(), want.vertices) {
		t.Errorf("vertices = %v, want %v", got.VerticesSlice(), want.vertices)
	}
	if !slices.Equal(got.EdgesSlice(), want.edges) {
		t.Errorf("edges = %v, want %v", got.EdgesSlice(), want.edges)
	}
	if !slices.Equal(got.WeightsSlice(), want.weights) {
		t.Errorf("weights = %v, want %v", got.WeightsSlice(), want.weights)
	}
	if !slices.Equal(got.HandlesSlice(), want.handles) {
		t.Errorf("handles = %v, want %v", got.HandlesSlice(), want.handles)
	}
	if got.Size() != want.size || got.Order() != order {
		t.Errorf("size/order = %d/%d, want %d/%d", got.Size(), got.Order(), want.size, order)
	}
}
