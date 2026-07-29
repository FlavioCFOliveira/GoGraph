package csrorder

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// csrorder_test.go — the guards that keep this benchmark package honest.
//
// A benchmark harness is code, and a wrong harness reports a wrong number with
// full confidence. Sprint 313 has two recorded instances of exactly that: the
// audit figure this sprint was opened on was physically unattainable, and the
// #2139 calibration's own first harness flattered binary search by 12x because a
// 64-key rotating stream stayed cache-resident. Both were caught by checking the
// harness against something independent, so every load-bearing assumption this
// package makes is asserted here rather than trusted.
//
// These tests are short-layer: the fixtures they touch are the small ones, and
// the expensive §2.4 reproduction is gated to the soak layer in
// distribution_soak_test.go.

// TestUnorderedArrays_MatchesOrderedAfterOrdering pins [UnorderedArrays] to
// csr.BuildFromAdjList.
//
// UnorderedArrays reproduces passes 1-2 of the real build and stops before pass
// 3, which makes it a COPY of production layout logic — and a copy that drifts
// would silently make the "unordered build" arm measure a different graph from
// the ordered arm, inverting the reported cost delta. Ordering its output must
// therefore reproduce the real build exactly, array for array.
func TestUnorderedArrays_MatchesOrderedAfterOrdering(t *testing.T) {
	t.Parallel()

	// Degree 64 is above order.go's insertion-sort cutoff of 32, so this exercises
	// the merge path and its scratch buffers rather than only the insertion sort.
	g, err := BuildHubGraph(64)
	if err != nil {
		t.Fatalf("BuildHubGraph: %v", err)
	}
	adj := g.AdjList()

	want := csr.BuildFromAdjList(adj)
	vertices, edges, weights, handles := UnorderedArrays(adj)
	csr.OrderRuns(vertices, edges, weights, handles)

	if got, exp := len(vertices), len(want.VerticesSlice()); got != exp {
		t.Fatalf("vertices length: got %d, want %d", got, exp)
	}
	for i, v := range want.VerticesSlice() {
		if vertices[i] != v {
			t.Fatalf("vertices[%d]: got %d, want %d", i, vertices[i], v)
		}
	}
	if got, exp := len(edges), len(want.EdgesSlice()); got != exp {
		t.Fatalf("edges length: got %d, want %d", got, exp)
	}
	for i, e := range want.EdgesSlice() {
		if edges[i] != e {
			t.Fatalf("edges[%d]: got %d, want %d", i, edges[i], e)
		}
	}
	if got, exp := len(weights), len(want.WeightsSlice()); got != exp {
		t.Fatalf("weights length: got %d, want %d", got, exp)
	}
	for i, w := range want.WeightsSlice() {
		if weights[i] != w {
			t.Fatalf("weights[%d]: got %v, want %v", i, weights[i], w)
		}
	}
	if got, exp := len(handles), len(want.HandlesSlice()); got != exp {
		t.Fatalf("handles length: got %d, want %d", got, exp)
	}
}

// TestUnorderedArrays_AreActuallyUnordered guards the other direction: if the
// fixture happened to insert destinations in ascending order, the "unordered" arm
// would already be ordered, the ordering pass would take its cheap
// already-ordered path, and BenchmarkOrderRuns would report a cost the real build
// never pays.
//
// This is the assertion that makes [destStride] load-bearing rather than
// decorative.
func TestUnorderedArrays_AreActuallyUnordered(t *testing.T) {
	t.Parallel()

	g, err := BuildHubGraph(64)
	if err != nil {
		t.Fatalf("BuildHubGraph: %v", err)
	}
	vertices, edges, _, _ := UnorderedArrays(g.AdjList())

	var disordered int
	for i := 0; i+1 < len(vertices); i++ {
		run := edges[vertices[i]:vertices[i+1]]
		for k := 0; k+1 < len(run); k++ {
			if run[k] > run[k+1] {
				disordered++
				break
			}
		}
	}
	if disordered == 0 {
		t.Fatal("every run arrived already ascending: the fixture's destination " +
			"scatter is broken, so the ordering benchmarks would measure the " +
			"already-ordered fast path instead of a real sort")
	}
}

// TestOrderedCSR_RunsOrdered confirms the shipped invariant holds on this
// package's own fixtures, so the probe benchmarks are measuring a binary search
// over an array that is genuinely sorted.
func TestOrderedCSR_RunsOrdered(t *testing.T) {
	t.Parallel()

	g, err := BuildHubGraph(32)
	if err != nil {
		t.Fatalf("BuildHubGraph: %v", err)
	}
	if c := OrderedCSR(g); !c.RunsOrdered() {
		t.Fatal("OrderedCSR produced unordered runs")
	}
}

// TestSearchFirstDst_MatchesScan is the differential that licenses
// [SearchFirstDst] to stand in for [ScanFirstDst] in the probe sweep.
//
// Both arms must return the SAME slot, not merely agree on presence: a forward
// position is an edge identity, and the pre-#2142 scans returned the FIRST
// matching slot, which is the property several callers depend on for parallel
// edges. The arena is built with parallel edges (repeated destinations) precisely
// to exercise that, since a run of distinct destinations could not distinguish
// "first match" from "any match".
func TestSearchFirstDst_MatchesScan(t *testing.T) {
	t.Parallel()

	// A small ordered run with deliberate multiplicity: destination 4 appears
	// three times and 8 twice, so "first match" is observable.
	run := []graph.NodeID{2, 4, 4, 4, 6, 8, 8, 10}
	for key := uint64(0); key <= 12; key++ {
		wantPos, wantOK := ScanFirstDst(run, 0, uint64(len(run)), key)
		gotPos, gotOK := SearchFirstDst(run, 0, uint64(len(run)), key)
		if gotOK != wantOK || gotPos != wantPos {
			t.Fatalf("key %d: binary got (%d,%t), scan got (%d,%t)",
				key, gotPos, gotOK, wantPos, wantOK)
		}
	}

	// Sub-ranges, so the start offset is exercised rather than always 0 — every
	// real probe runs on a slice of one shared edges array.
	for start := uint64(0); start <= uint64(len(run)); start++ {
		for end := start; end <= uint64(len(run)); end++ {
			for key := uint64(0); key <= 12; key++ {
				wantPos, wantOK := ScanFirstDst(run, start, end, key)
				gotPos, gotOK := SearchFirstDst(run, start, end, key)
				if gotOK != wantOK || gotPos != wantPos {
					t.Fatalf("range [%d,%d) key %d: binary got (%d,%t), scan got (%d,%t)",
						start, end, key, gotPos, gotOK, wantPos, wantOK)
				}
			}
		}
	}
}

// TestBuildHubGraph_DegreeIsExact confirms the swept parameter is the degree the
// fixture actually has. The whole sweep is meaningless if the fixture's degree
// drifts from the label, and a silent drift is easy: a destination collision
// inside one source would be deduplicated by a non-multigraph adjacency and
// shorten the run.
func TestBuildHubGraph_DegreeIsExact(t *testing.T) {
	t.Parallel()

	for _, d := range []int{8, 16, 64, 512} {
		g, err := BuildHubGraph(d)
		if err != nil {
			t.Fatalf("BuildHubGraph(%d): %v", d, err)
		}
		p := ProfileDegrees(g.AdjList(), probeThreshold)
		if p.MaxDegree != d {
			t.Errorf("degree %d: max out-degree is %d, want exactly %d", d, p.MaxDegree, d)
		}
		if p.P50 != d {
			t.Errorf("degree %d: median out-degree is %d, want exactly %d (every hub "+
				"must carry the full degree, or the sweep's x-axis is wrong)", d, p.P50, d)
		}
		if want := uint64(HubFixtureArcs); p.Arcs != want {
			t.Errorf("degree %d: arc count is %d, want %d — the sweep holds arcs "+
				"constant so probe COUNT is fixed and only per-probe cost varies",
				d, p.Arcs, want)
		}
	}
}

// TestProfileDegrees_FractionsAreConsistent checks the three fractions against a
// hand-computed case, because they are the numbers every benchmark result is read
// against and a wrong CostFrac would silently misattribute the win.
//
// The fixture is a controlled hub graph at degree 32, where the answer is known
// by construction: every source has degree 32, so above a threshold of 16 all
// three fractions must be exactly 1, and above a threshold of 32 all three must
// be exactly 0 (the comparison is strict).
func TestProfileDegrees_FractionsAreConsistent(t *testing.T) {
	t.Parallel()

	g, err := BuildHubGraph(32)
	if err != nil {
		t.Fatalf("BuildHubGraph: %v", err)
	}
	adj := g.AdjList()

	above := ProfileDegrees(adj, 16)
	for name, got := range map[string]float64{
		"vertexFrac": above.VertexFrac,
		"edgeFrac":   above.EdgeFrac,
		"costFrac":   above.CostFrac,
	} {
		if got != 1 {
			t.Errorf("T=16 %s: got %v, want 1 (every source has degree 32)", name, got)
		}
	}

	at := ProfileDegrees(adj, 32)
	for name, got := range map[string]float64{
		"vertexFrac": at.VertexFrac,
		"edgeFrac":   at.EdgeFrac,
		"costFrac":   at.CostFrac,
	} {
		if got != 0 {
			t.Errorf("T=32 %s: got %v, want 0 (the threshold comparison is strict)", name, got)
		}
	}

	// Σd² must equal sources × 32², which pins the cost model itself rather than
	// only the ratio derived from it.
	if want := above.Sources * 32 * 32; above.SumSqDegree != want {
		t.Errorf("SumSqDegree: got %d, want %d", above.SumSqDegree, want)
	}
}

// TestProbeArena_MissKeysAreInRange guards the probe sweep's miss case.
//
// An out-of-range miss key is the classic way to accidentally measure nothing: a
// binary search rejects it after one comparison and a linear scan over an ordered
// run could bail early, so both arms would report a cost unrelated to the degree.
// The arena's contract is that an odd key is absent but lies strictly inside the
// run's [min, max] span; this asserts it.
func TestProbeArena_MissKeysAreInRange(t *testing.T) {
	t.Parallel()

	const degree = 64
	arena := buildProbeArena(degree, true)
	run := arena.edges[:degree]
	lo, hi := uint64(run[0]), uint64(run[degree-1])

	for k := uint64(0); k < degree; k++ {
		miss := arena.base(0) + 2*k + 1
		if _, ok := ScanFirstDst(arena.edges, 0, degree, miss); ok {
			t.Fatalf("miss key %d is present in the run", miss)
		}
		// The last odd key sits one past the final even destination, so the
		// in-range requirement is stated against the span it must not escape.
		if miss < lo || miss > hi+1 {
			t.Fatalf("miss key %d is outside the run span [%d,%d]", miss, lo, hi)
		}
	}
}

// TestProbeArena_UnorderedHoldsSameMultiset confirms the two arena layouts differ
// only in order, so BenchmarkProbe_Linear_Hit_Unordered compares layouts rather
// than contents.
func TestProbeArena_UnorderedHoldsSameMultiset(t *testing.T) {
	t.Parallel()

	const degree = 64
	ordered := buildProbeArena(degree, true)
	scattered := buildProbeArena(degree, false)

	if ordered.runs != scattered.runs {
		t.Fatalf("run counts differ: %d vs %d", ordered.runs, scattered.runs)
	}

	seen := make(map[graph.NodeID]int, degree)
	for _, e := range ordered.edges[:degree] {
		seen[e]++
	}
	for _, e := range scattered.edges[:degree] {
		seen[e]--
	}
	for dst, n := range seen {
		if n != 0 {
			t.Fatalf("destination %d has multiplicity delta %d between layouts", dst, n)
		}
	}

	var differs bool
	for k := 0; k < degree; k++ {
		if ordered.edges[k] != scattered.edges[k] {
			differs = true
			break
		}
	}
	if !differs {
		t.Fatal("the scattered layout is identical to the ordered one, so the " +
			"layout comparison measures nothing")
	}
}
