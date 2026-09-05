package csr

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// order_test.go — within-source ordering invariant (rmp #2141).
//
// The load-bearing property is not merely "runs are sorted": it is that the
// permutation is applied IDENTICALLY to every parallel column, and that slots
// the total key cannot separate (handle 0, which is what a MERGE-created slot
// carries) keep their relative input order. cypher/api.go's buildEdgeTypeFilter
// resolves a parallel edge's relationship TYPE from that ordinal, so an unstable
// reorder silently mis-types edges rather than failing loudly.

// checkRunsOrdered asserts every source's run is non-decreasing in (dst, handle).
func checkRunsOrdered(t *testing.T, verts []uint64, edges []graph.NodeID, handles []uint64) {
	t.Helper()
	for i := 0; i+1 < len(verts); i++ {
		lo, hi := verts[i], verts[i+1]
		for k := lo + 1; k < hi; k++ {
			if edges[k] < edges[k-1] {
				t.Fatalf("source %d: destination out of order at %d: %d < %d", i, k, edges[k], edges[k-1])
			}
			if handles != nil && edges[k] == edges[k-1] && handles[k] < handles[k-1] {
				t.Fatalf("source %d: handle out of order within destination run at %d: %d < %d",
					i, k, handles[k], handles[k-1])
			}
		}
	}
}

func TestOrderRuns_SortsByDestinationAndHandle(t *testing.T) {
	t.Parallel()
	// Two sources. Source 0 has parallel edges to destination 5 with distinct
	// handles supplied OUT of handle order, so the tiebreaker must reorder them.
	verts := []uint64{0, 5, 8}
	edges := []graph.NodeID{9, 5, 2, 5, 7 /* source 1: */, 4, 1, 4}
	weights := []int64{90, 51, 20, 52, 70, 40, 10, 41}
	handles := []uint64{900, 511, 200, 502, 700, 400, 100, 401}

	OrderRuns(verts, edges, weights, handles)

	wantEdges := []graph.NodeID{2, 5, 5, 7, 9, 1, 4, 4}
	wantHandles := []uint64{200, 502, 511, 700, 900, 100, 400, 401}
	wantWeights := []int64{20, 52, 51, 70, 90, 10, 40, 41}
	if !slices.Equal(edges, wantEdges) {
		t.Errorf("edges = %v, want %v", edges, wantEdges)
	}
	if !slices.Equal(handles, wantHandles) {
		t.Errorf("handles = %v, want %v", handles, wantHandles)
	}
	// The weight column proves the permutation was applied to the parallel
	// columns and not recomputed: weight 52 must travel with handle 502.
	if !slices.Equal(weights, wantWeights) {
		t.Errorf("weights = %v, want %v", weights, wantWeights)
	}
	checkRunsOrdered(t, verts, edges, handles)
}

// TestOrderRuns_Handle0ResidualIsStable pins the invariant the total key cannot
// express. Slots carrying handle 0 share the key (dst, 0), so only the sort's
// stability keeps them in input order — and that order is what
// buildEdgeTypeFilter's ordinal path reads to resolve per-instance types.
func TestOrderRuns_Handle0ResidualIsStable(t *testing.T) {
	t.Parallel()
	for _, n := range []int{4, runOrderInsertionCutoff, runOrderInsertionCutoff + 1, 300} {
		// Every slot targets the SAME destination with handle 0, so the sort has
		// no discriminating information at all; the weight column carries the
		// original ordinal and must come out unpermuted.
		verts := []uint64{0, uint64(n)}
		edges := make([]graph.NodeID, n)
		weights := make([]int64, n)
		handles := make([]uint64, n)
		for i := range edges {
			edges[i] = 7
			weights[i] = int64(i)
			handles[i] = 0
		}
		OrderRuns(verts, edges, weights, handles)
		for i := range weights {
			if weights[i] != int64(i) {
				t.Fatalf("n=%d: handle-0 residual reordered at slot %d: got ordinal %d, want %d",
					n, i, weights[i], i)
			}
		}
	}
}

// TestOrderRuns_MixedHandleAndHandle0 covers a destination run holding BOTH
// handle-bearing slots and handle-0 slots: the zeros sort first (0 is the least
// handle) and must keep their mutual input order.
func TestOrderRuns_MixedHandleAndHandle0(t *testing.T) {
	t.Parallel()
	verts := []uint64{0, 5}
	edges := []graph.NodeID{7, 7, 7, 7, 7}
	weights := []int64{0, 1, 2, 3, 4} // original ordinal
	handles := []uint64{50, 0, 20, 0, 0}

	OrderRuns(verts, edges, weights, handles)

	// Expected: the three handle-0 slots first, in input order (ordinals 1,3,4),
	// then handle 20 (ordinal 2), then handle 50 (ordinal 0).
	wantOrdinals := []int64{1, 3, 4, 2, 0}
	if !slices.Equal(weights, wantOrdinals) {
		t.Errorf("ordinals = %v, want %v", weights, wantOrdinals)
	}
	checkRunsOrdered(t, verts, edges, handles)
}

// TestOrderRuns_PreservesMultisetAndPairing is the property test: for every
// source, ordering must preserve the multiset of destinations AND the pairing of
// each slot's (destination, handle, weight), permuting all columns as one.
func TestOrderRuns_PreservesMultisetAndPairing(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(11, 13)) //nolint:gosec // G404: math/rand/v2 PCG seeded from a fixed constant — this test asserts a reproducible sequence, which a CSPRNG would destroy.
	for trial := 0; trial < 300; trial++ {
		nSrc := 1 + r.IntN(6)
		verts := make([]uint64, nSrc+1)
		var total uint64
		degrees := make([]int, nSrc)
		for i := range degrees {
			// Span the insertion-sort cutoff in both directions.
			degrees[i] = r.IntN(80)
			verts[i] = total
			total += uint64(degrees[i])
		}
		verts[nSrc] = total

		edges := make([]graph.NodeID, total)
		weights := make([]int64, total)
		handles := make([]uint64, total)
		useHandles := trial%3 != 0 // exercise the nil-handle column too
		for i := range edges {
			// A small destination space forces parallel edges.
			edges[i] = graph.NodeID(r.IntN(8))
			weights[i] = int64(i) // slot identity
			if useHandles {
				// Handle 0 appears deliberately, so residual stability is exercised.
				handles[i] = uint64(r.IntN(4))
			}
		}
		type slot struct {
			d graph.NodeID
			h uint64
			w int64
		}
		before := make(map[int][]slot, nSrc)
		for i := 0; i < nSrc; i++ {
			for k := verts[i]; k < verts[i+1]; k++ {
				before[i] = append(before[i], slot{edges[k], handles[k], weights[k]})
			}
		}

		var hcol []uint64
		if useHandles {
			hcol = handles
		}
		OrderRuns(verts, edges, weights, hcol)
		checkRunsOrdered(t, verts, edges, hcol)

		for i := 0; i < nSrc; i++ {
			var after []slot
			for k := verts[i]; k < verts[i+1]; k++ {
				after = append(after, slot{edges[k], handles[k], weights[k]})
			}
			if len(after) != len(before[i]) {
				t.Fatalf("trial %d source %d: slot count changed %d -> %d",
					trial, i, len(before[i]), len(after))
			}
			// Same multiset of whole slots: a mis-permuted column would pair a
			// destination with another slot's handle or weight and fail here.
			cmp := func(a, b slot) int {
				if a.w != b.w {
					return int(a.w - b.w)
				}
				return 0
			}
			gotSorted := slices.Clone(after)
			wantSorted := slices.Clone(before[i])
			slices.SortFunc(gotSorted, cmp)
			slices.SortFunc(wantSorted, cmp)
			if !slices.Equal(gotSorted, wantSorted) {
				t.Fatalf("trial %d source %d: slot set changed\n got %v\nwant %v",
					trial, i, gotSorted, wantSorted)
			}
		}
	}
}

// TestOrderRuns_AlreadyOrderedIsFree guards the fast path: an already-ordered
// run must be detected and left untouched.
func TestOrderRuns_AlreadyOrderedIsFree(t *testing.T) {
	t.Parallel()
	verts := []uint64{0, 100}
	edges := make([]graph.NodeID, 100)
	for i := range edges {
		edges[i] = graph.NodeID(i * 2)
	}
	want := slices.Clone(edges)
	OrderRuns[struct{}](verts, edges, nil, nil)
	if !slices.Equal(edges, want) {
		t.Errorf("already-ordered run was modified")
	}
}

// TestOrderRuns_NoAllocationBelowCutoff pins the allocation budget that keeps
// the CSR build's cost profile intact for a typical property graph, whose
// average out-degree is 4-16: no scratch buffer is allocated at all when every
// run fits the insertion-sort path.
func TestOrderRuns_NoAllocationBelowCutoff(t *testing.T) {
	// No t.Parallel: testing.AllocsPerRun panics in a parallel test.
	const nSrc = 200
	verts := make([]uint64, nSrc+1)
	var total uint64
	for i := 0; i < nSrc; i++ {
		verts[i] = total
		total += runOrderInsertionCutoff
	}
	verts[nSrc] = total
	edges := make([]graph.NodeID, total)
	handles := make([]uint64, total)
	for i := range edges {
		edges[i] = graph.NodeID(total - uint64(i)) // worst case: fully reversed
		handles[i] = uint64(i)
	}

	allocs := testing.AllocsPerRun(5, func() {
		// Re-disorder in place (slices.Reverse allocates nothing) so every run
		// measures the real sort rather than the already-ordered fast path.
		slices.Reverse(edges)
		slices.Reverse(handles)
		OrderRuns[struct{}](verts, edges, nil, handles)
	})
	if allocs != 0 {
		t.Errorf("OrderRuns allocated %.1f times for runs at the cutoff; want 0", allocs)
	}
}

// TestBuildFromAdjList_RunsOrdered is the end-to-end assertion that the build
// paths carry the invariant, including the parallel-edge case, and that
// RunsOrdered agrees.
func TestBuildFromAdjList_RunsOrdered(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	// Insert each source's neighbours in DESCENDING destination order so the
	// build must actually reorder, and add parallel edges with distinct handles.
	for src := 1; src <= 40; src++ {
		for dst := 60; dst >= 41; dst-- {
			if err := a.AddEdgeH(src, dst, int64(dst), uint64(src*1000+dst)); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.AddEdgeH(src, 50, 999, uint64(src*1000+500)); err != nil {
			t.Fatal(err)
		}
	}
	c := BuildFromAdjList(a)
	if !c.RunsOrdered() {
		t.Fatal("BuildFromAdjList produced unordered runs")
	}
	checkRunsOrdered(t, c.VerticesSlice(), c.EdgesSlice(), c.HandlesSlice())

	// The live build path must agree with the raw one; a nil filter is the
	// documented identity, so the arrays must match element-for-element.
	cl := BuildFromAdjListLive(a, nil)
	if !slices.Equal(c.EdgesSlice(), cl.EdgesSlice()) {
		t.Error("raw and live builds disagree on edge order")
	}
	if !slices.Equal(c.HandlesSlice(), cl.HandlesSlice()) {
		t.Error("raw and live builds disagree on handle order")
	}
}

// TestBuildReverse_InheritsOrdering proves the claim the FromArrays contract
// makes about the reverse CSR, which is DERIVED rather than enforced and so
// deserves a test.
//
// BuildReverse scatters while walking sources ascending, so each reverse bucket
// receives its sources in ascending order; and for a given source it copies that
// source's forward slots in forward order, which is now (destination, handle)
// ordered. Restricted to one destination bucket that means ascending handle. So
// the reverse CSR satisfies the (source, handle) invariant as a CONSEQUENCE of
// the forward CSR satisfying it — which is also why order.go must not re-order
// the reverse side: doing so would break buildRevToFwd's ordinal pairing.
func TestBuildReverse_InheritsOrdering(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
	// Parallel edges inserted with DESCENDING handles, so the forward ordering
	// has to move them and the reverse must reflect the moved order.
	for src := 1; src <= 12; src++ {
		for dst := 20; dst >= 15; dst-- {
			for _, h := range []uint64{900, 300, 600} {
				if err := a.AddEdgeH(src, dst, int64(dst), h+uint64(src)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	fwd := BuildFromAdjList(a)
	if !fwd.RunsOrdered() {
		t.Fatal("forward CSR unordered")
	}
	rev := fwd.BuildReverse()
	if !rev.RunsOrdered() {
		t.Error("reverse CSR does not inherit the (source, handle) ordering; the " +
			"FromArrays contract claims it does")
	}
	checkRunsOrdered(t, rev.VerticesSlice(), rev.EdgesSlice(), rev.HandlesSlice())
}

// TestRunsOrdered_DetectsUnordered proves RunsOrdered is a real check rather
// than a constant: a FromArrays snapshot, which deliberately does not order,
// must report false.
func TestRunsOrdered_DetectsUnordered(t *testing.T) {
	t.Parallel()
	c := FromArrays[struct{}]([]uint64{0, 3}, []graph.NodeID{9, 1, 5}, nil, 3, 3)
	if c.RunsOrdered() {
		t.Error("RunsOrdered reported true for an unordered FromArrays snapshot")
	}
	// And ordering those same arrays makes it true, which is the documented
	// remedy in the FromArrays contract.
	verts := []uint64{0, 3}
	edges := []graph.NodeID{9, 1, 5}
	OrderRuns[struct{}](verts, edges, nil, nil)
	if !FromArrays[struct{}](verts, edges, nil, 3, 3).RunsOrdered() {
		t.Error("RunsOrdered reported false after OrderRuns")
	}
}
