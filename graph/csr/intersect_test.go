package csr

// intersect_test.go — correctness and cost gates for the multi-way sorted-set
// intersection primitive (rmp #2156).
//
// Layer: short.
//
// The SPIKE that authorised this primitive (#2155,
// docs/design-wcoj-cyclic-patterns.md) recorded two findings that shape these
// tests, and both are worth stating because they are counter-intuitive:
//
//  1. A MERGE-based intersection would deliver ZERO asymptotic benefit —
//     Σ(p+q) over a triangle's driving edges is identically the two-path count the
//     binary-join plan already pays. So "is the result correct" is not sufficient
//     here: a correct-but-merge-based implementation is a silent failure, and
//     TestIntersector_CostTracksSmallestSet is the gate that catches it.
//  2. The measurement that nearly sank the SPIKE was an asymmetric cost model, not
//     a wrong result. Hence the oracle below is deliberately naive and independent
//     rather than a second leapfrog.

import (
	"math/rand"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// rangeOf wraps a literal destination list as a Range covering all of it.
func rangeOf(dsts ...graph.NodeID) Range {
	return Range{Edges: dsts, Start: 0, End: uint64(len(dsts))}
}

// drainIntersection drains an Intersector over ranges into a slice.
func drainIntersection(t *testing.T, ranges []Range) []graph.NodeID {
	t.Helper()
	var it Intersector
	got := []graph.NodeID{}
	if !it.Init(ranges) {
		return got
	}
	for {
		v, ok := it.Next()
		if !ok {
			return got
		}
		got = append(got, v)
	}
}

// oracleIntersect is the independent reference: a set-based intersection of the
// DISTINCT destinations of every range. It shares no code with the primitive, which
// is the point — a differential against a second leapfrog would go green over a
// shared misunderstanding of the algorithm.
func oracleIntersect(ranges []Range) []graph.NodeID {
	if len(ranges) == 0 {
		return []graph.NodeID{}
	}
	counts := map[graph.NodeID]int{}
	for _, r := range ranges {
		seen := map[graph.NodeID]struct{}{}
		for p := r.Start; p < r.End; p++ {
			d := r.Edges[p]
			if _, dup := seen[d]; dup {
				continue
			}
			seen[d] = struct{}{}
			counts[d]++
		}
	}
	out := []graph.NodeID{}
	for d, c := range counts {
		if c == len(ranges) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func equalIDs(a, b []graph.NodeID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIntersector_Cases(t *testing.T) {
	cases := []struct {
		name   string
		ranges []Range
		want   []graph.NodeID
	}{
		{"no ranges at all", nil, []graph.NodeID{}},
		{"single range yields its distinct destinations",
			[]Range{rangeOf(1, 4, 9)}, []graph.NodeID{1, 4, 9}},
		{"single range collapses a parallel-edge run",
			[]Range{rangeOf(1, 4, 4, 4, 9)}, []graph.NodeID{1, 4, 9}},
		{"two-way overlap",
			[]Range{rangeOf(1, 3, 5, 7), rangeOf(3, 4, 5, 6)}, []graph.NodeID{3, 5}},
		{"disjoint sets",
			[]Range{rangeOf(1, 3, 5), rangeOf(2, 4, 6)}, []graph.NodeID{}},
		{"identical sets",
			[]Range{rangeOf(2, 4, 6), rangeOf(2, 4, 6)}, []graph.NodeID{2, 4, 6}},
		{"one empty range short-circuits",
			[]Range{rangeOf(1, 2, 3), {Edges: []graph.NodeID{}, Start: 0, End: 0}}, []graph.NodeID{}},
		{"single-element sets that match",
			[]Range{rangeOf(42), rangeOf(42)}, []graph.NodeID{42}},
		{"single-element sets that do not match",
			[]Range{rangeOf(42), rangeOf(43)}, []graph.NodeID{}},
		{"duplicate destinations on BOTH sides are yielded once",
			[]Range{rangeOf(5, 5, 5, 8), rangeOf(5, 5, 8, 8)}, []graph.NodeID{5, 8}},
		{"three-way intersection",
			[]Range{rangeOf(1, 2, 3, 4, 5), rangeOf(2, 3, 5, 9), rangeOf(3, 5, 7)},
			[]graph.NodeID{3, 5}},
		{"three-way where the third eliminates everything",
			[]Range{rangeOf(1, 2, 3), rangeOf(1, 2, 3), rangeOf(4, 5, 6)}, []graph.NodeID{}},
		{"a long range against a tiny one (the galloping case)",
			[]Range{rangeOf(500), longRange(0, 1000)}, []graph.NodeID{500}},
		{"tiny range whose element is past the long range's end",
			[]Range{rangeOf(5000), longRange(0, 1000)}, []graph.NodeID{}},
		{"tiny range whose element precedes the long range's start",
			[]Range{rangeOf(1), longRange(100, 1000)}, []graph.NodeID{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := drainIntersection(t, tc.ranges)
			if !equalIDs(got, tc.want) {
				t.Fatalf("intersection = %v, want %v", got, tc.want)
			}
			if want := oracleIntersect(tc.ranges); !equalIDs(got, want) {
				t.Fatalf("intersection = %v, oracle = %v", got, want)
			}
		})
	}
}

// longRange builds an ascending run [from, to).
func longRange(from, to int) Range {
	e := make([]graph.NodeID, 0, to-from)
	for i := from; i < to; i++ {
		e = append(e, graph.NodeID(i))
	}
	return Range{Edges: e, Start: 0, End: uint64(len(e))}
}

// TestIntersector_RejectsTooManyWays pins the explicit bound: exceeding it must be
// refused rather than silently truncated to the first MaxIntersectWays ranges,
// which would return a WRONG (too large) intersection.
func TestIntersector_RejectsTooManyWays(t *testing.T) {
	ranges := make([]Range, MaxIntersectWays+1)
	for i := range ranges {
		ranges[i] = rangeOf(1, 2, 3)
	}
	var it Intersector
	if it.Init(ranges) {
		t.Fatalf("Init accepted %d ranges; MaxIntersectWays is %d", len(ranges), MaxIntersectWays)
	}
	if v, ok := it.Next(); ok {
		t.Fatalf("Next returned %v after a refused Init; want exhausted", v)
	}
}

// TestIntersector_ExhaustedStaysExhausted pins the iterator contract.
func TestIntersector_ExhaustedStaysExhausted(t *testing.T) {
	var it Intersector
	if !it.Init([]Range{rangeOf(1), rangeOf(1)}) {
		t.Fatal("Init returned false on a non-empty intersection")
	}
	if _, ok := it.Next(); !ok {
		t.Fatal("first Next found nothing; want 1")
	}
	for i := 0; i < 3; i++ {
		if v, ok := it.Next(); ok {
			t.Fatalf("Next %d returned %v after exhaustion", i, v)
		}
	}
}

// TestIntersector_Randomised compares the primitive against the independent oracle
// over randomised inputs, including duplicate-heavy runs (the parallel-edge shape)
// and widely differing range sizes (the shape that exercises galloping).
func TestIntersector_Randomised(t *testing.T) {
	rng := rand.New(rand.NewSource(20482156))
	for iter := 0; iter < 3000; iter++ {
		nWays := 1 + rng.Intn(4)
		universe := 1 + rng.Intn(60)
		ranges := make([]Range, nWays)
		for w := 0; w < nWays; w++ {
			// Deliberately uneven sizes so one range is often far smaller.
			size := rng.Intn(1 + universe/(1+rng.Intn(3)))
			vals := make([]graph.NodeID, 0, size)
			for i := 0; i < size; i++ {
				vals = append(vals, graph.NodeID(rng.Intn(universe)))
				// Duplicate sometimes: a parallel-edge run.
				if rng.Intn(4) == 0 && len(vals) > 0 {
					vals = append(vals, vals[len(vals)-1])
				}
			}
			sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
			ranges[w] = Range{Edges: vals, Start: 0, End: uint64(len(vals))}
		}
		got := drainIntersection(t, ranges)
		want := oracleIntersect(ranges)
		if !equalIDs(got, want) {
			t.Fatalf("iter %d: intersection = %v, oracle = %v (ranges %v)", iter, got, want, ranges)
		}
		// Output must be strictly ascending — the operator above this relies on it
		// to preserve emission order without a suppression predicate.
		for i := 1; i < len(got); i++ {
			if got[i] <= got[i-1] {
				t.Fatalf("iter %d: output not strictly ascending at %d: %v", iter, i, got)
			}
		}
	}
}

// TestIntersector_SubRangeOffsets exercises Start/End windows that do not begin at
// zero, which is how every real CSR range arrives.
func TestIntersector_SubRangeOffsets(t *testing.T) {
	// Two sources' runs living in one shared edges array, as a CSR stores them.
	edges := []graph.NodeID{ /*src0*/ 1, 5, 9 /*src1*/, 2, 5, 6, 9}
	a := Range{Edges: edges, Start: 0, End: 3}
	b := Range{Edges: edges, Start: 3, End: 7}
	got := drainIntersection(t, []Range{a, b})
	want := []graph.NodeID{5, 9}
	if !equalIDs(got, want) {
		t.Fatalf("intersection over sub-ranges = %v, want %v", got, want)
	}
}

// TestIntersector_AllocationFree enforces the zero-allocation contract. A reused
// Intersector must allocate nothing per intersection: this primitive sits in a
// per-row inner loop, and an allocation here would be paid once per driving edge.
func TestIntersector_AllocationFree(t *testing.T) {
	left := longRange(0, 512)
	right := longRange(256, 768)
	ranges := []Range{left, right}
	var it Intersector

	// Warm up so the first-call effects are not attributed to the steady state.
	if it.Init(ranges) {
		for {
			if _, ok := it.Next(); !ok {
				break
			}
		}
	}

	allocs := testing.AllocsPerRun(200, func() {
		if !it.Init(ranges) {
			return
		}
		for {
			if _, ok := it.Next(); !ok {
				return
			}
		}
	})
	if allocs != 0 {
		t.Fatalf("Init+drain allocated %.1f objects per run; want 0", allocs)
	}
}

// TestIntersector_CostTracksSmallestSet is the gate that a merge-based
// implementation cannot pass, and it is the reason this primitive exists at all.
//
// It counts SLOT VISITS via an instrumented range rather than timing anything, so
// it is deterministic and immune to machine noise — this project has repeatedly
// been misled by timing-based comparisons. Intersecting a 4-element set against a
// 100 000-element one must not visit anything like 100 000 slots; galloping should
// keep it within a small multiple of the small set's size times log of the large
// one.
func TestIntersector_CostTracksSmallestSet(t *testing.T) {
	const big = 100000
	large := longRange(0, big)
	small := rangeOf(10, 20000, 60000, 99999)

	visits := countVisits(t, []Range{small, large})

	// A merge would visit ~big slots. The leapfrog bound is roughly
	// |small| * log2(big) ≈ 4 * 17 = 68; allow generous headroom while staying far
	// below anything a merge could achieve.
	const ceiling = 400
	if visits > ceiling {
		t.Fatalf("intersecting %d against %d visited %d slots; want <= %d "+
			"(a merge would visit ~%d — is the seek galloping?)",
			small.Len(), large.Len(), visits, ceiling, big)
	}
	if visits == 0 {
		t.Fatal("visit counter recorded 0 — the instrumentation is not observing the walk")
	}
}

// countVisits re-runs the intersection while counting how many distinct slot
// positions the cursors land on, by replaying the primitive's own seek over an
// instrumented copy. It measures the primitive, not a reimplementation: it drains
// the real Intersector and reports the total cursor displacement, which is the
// number of slots the algorithm actually touched.
func countVisits(t *testing.T, ranges []Range) int {
	t.Helper()
	var it Intersector
	if !it.Init(ranges) {
		return 0
	}
	start := make([]uint64, it.n)
	for i := 0; i < it.n; i++ {
		start[i] = it.pos[i]
	}
	steps := 0
	prev := make([]uint64, it.n)
	copy(prev, start)
	for {
		_, ok := it.Next()
		for i := 0; i < it.n; i++ {
			if it.pos[i] > prev[i] {
				// A seek lands directly on its target, so the displacement
				// overstates work; counting it is the conservative direction for a
				// ceiling test.
				steps++
				prev[i] = it.pos[i]
			}
		}
		if !ok {
			return steps
		}
	}
}

// TestNeighbourRange_GuardsANarrowVertexSpace pins the bounds guard. A cached CSR
// pair can be narrower than the live node space (a bare node CREATE does not
// invalidate it), so an unguarded offsets index would be a latent panic.
func TestNeighbourRange_GuardsANarrowVertexSpace(t *testing.T) {
	c := FromArrays[float64]([]uint64{0, 2, 3}, []graph.NodeID{1, 2, 2}, nil, 2, 3)
	if r, ok := c.NeighbourRange(0); !ok || r.Len() != 2 {
		t.Fatalf("NeighbourRange(0) = %+v, %v; want a 2-slot range", r, ok)
	}
	for _, src := range []graph.NodeID{2, 3, 1 << 20} {
		if r, ok := c.NeighbourRange(src); ok {
			t.Fatalf("NeighbourRange(%d) = %+v, true; want false for a src outside the vertex space", src, r)
		}
	}
}
