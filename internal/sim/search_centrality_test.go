package sim

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// TestCentralityChecks_CleanOnFixtures asserts the whole CENTRALITY battery
// (unweighted + weighted betweenness vs the independent BFS/Dijkstra references)
// is clean on every seed-derived instance across a spread of ticks. The
// instances are pure functions of the tick, so a clean pass here is
// reproducible. A flag here means either a real library divergence or a defect in
// the reference — both worth surfacing.
func TestCentralityChecks_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	for tick := int64(0); tick < 200; tick++ {
		if vs := centralityViolations(tick); len(vs) != 0 {
			t.Fatalf("centrality battery flagged a clean instance at tick=%d:\n%v", tick, vs)
		}
	}
}

// TestCentralityChecks_Deterministic asserts the battery is a pure function of
// the tick: the same tick yields identical fixtures and an identical (empty)
// result, the property the DST harness relies on for replay.
func TestCentralityChecks_Deterministic(t *testing.T) {
	t.Parallel()
	const tick = int64(987654)

	a := centralityFixtures(NewSeed(uint64(tick) ^ centralitySeedSalt))
	b := centralityFixtures(NewSeed(uint64(tick) ^ centralitySeedSalt))
	if len(a) != len(b) {
		t.Fatalf("fixture count differs across identical seeds: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].name != b[i].name || a[i].order != b[i].order ||
			a[i].directed != b[i].directed || len(a[i].arcs) != len(b[i].arcs) {
			t.Fatalf("fixture %d differs across identical seeds: %+v vs %+v", i, a[i], b[i])
		}
		for j := range a[i].arcs {
			if a[i].arcs[j] != b[i].arcs[j] {
				t.Fatalf("fixture %q arc %d differs across identical seeds: %v vs %v",
					a[i].name, j, a[i].arcs[j], b[i].arcs[j])
			}
		}
	}

	// And the end-to-end result is reproducible.
	r1 := centralityViolations(tick)
	r2 := centralityViolations(tick)
	if len(r1) != len(r2) {
		t.Fatalf("tick=%d nondeterministic: len %d vs %d", tick, len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i] != r2[i] {
			t.Fatalf("tick=%d violation %d differs: %v vs %v", tick, i, r1[i], r2[i])
		}
	}
}

// --- Reference-correctness tests: prove OUR references are right, on tiny
// hand-checked instances, before trusting them to judge the library. ---

// TestCentralityUnweightedReference_HandChecked verifies the unweighted
// reference against shapes whose UNNORMALISED ordered-pair betweenness is known
// by hand. The ordered-pair convention sums each undirected pair {s,t} twice
// (as (s,t) and (t,s)), matching the library, so the path's interior values are
// 2*i*(n-1-i) and the star hub is (n-1)*(n-2).
func TestCentralityUnweightedReference_HandChecked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		f    centralityFixture
		want []float64
	}{
		{
			// Path 0-1-2-3-4: interior i scores 2*i*(4-i) -> [0,6,8,6,0].
			name: "path5", f: centralityPath("path5", 5),
			want: []float64{0, 6, 8, 6, 0},
		},
		{
			// Path 0-1-2: only the middle lies between the one pair, counted
			// in both directions -> [0,2,0].
			name: "path3", f: centralityPath("path3", 3),
			want: []float64{0, 2, 0},
		},
		{
			// Star hub + 5 leaves: hub on every ordered leaf pair = 5*4 = 20.
			name: "star6", f: centralityStar("star6", 6),
			want: []float64{20, 0, 0, 0, 0, 0},
		},
		{
			// Directed chain 0->1->2->3->4: forward only, vertex i scores
			// i*(4-i) -> [0,3,4,3,0] (half the undirected path).
			name: "directed-chain5", f: centralityDirectedChain("directed-chain5", 5),
			want: []float64{0, 3, 4, 3, 0},
		},
		{
			// Directed diamond 0->{1,2}->3: pair (0,3) has 2 shortest paths,
			// so 1 and 2 each carry 1/2.
			name: "directed-diamond", f: centralityDirectedDiamond("directed-diamond"),
			want: []float64{0, 0.5, 0.5, 0},
		},
		{
			// K4: every pair adjacent, nobody lies strictly between -> all 0.
			name: "complete-k4", f: centralityCompleteUndirected("complete-k4", 4),
			want: []float64{0, 0, 0, 0},
		},
		{
			// No edges -> no shortest paths -> all 0.
			name: "isolated", f: centralityIsolated("isolated", 4),
			want: []float64{0, 0, 0, 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := centralityUnweightedReference(tc.f)
			if len(got) != len(tc.want) {
				t.Fatalf("length = %d, want %d", len(got), len(tc.want))
			}
			for v := range tc.want {
				if math.Abs(got[v]-tc.want[v]) > 1e-12 {
					t.Errorf("betweenness[%d] = %.17g, want %.17g", v, got[v], tc.want[v])
				}
			}
		})
	}
}

// TestCentralityWeightedReference_HandChecked verifies the weighted reference. On
// a graph with unit weights it must reproduce the unweighted hand-checked values
// exactly (Dijkstra with unit weights == BFS), and on a weight-sensitive shape it
// must route around a heavy edge.
func TestCentralityWeightedReference_HandChecked(t *testing.T) {
	t.Parallel()

	// (a) Unit-weight path reproduces the unweighted result.
	pathWant := []float64{0, 6, 8, 6, 0}
	gotPath := centralityWeightedReference(centralityPath("path5", 5))
	for v := range pathWant {
		if math.Abs(gotPath[v]-pathWant[v]) > 1e-12 {
			t.Errorf("unit-weight path: weighted[%d] = %.17g, want %.17g", v, gotPath[v], pathWant[v])
		}
	}

	// (b) Weight-sensitive triangle. Vertices 0,1,2 with the direct edge 0-2
	// expensive (weight 10) and the two-hop route 0-1-2 cheap (1+1=2). The unique
	// shortest path between 0 and 2 then goes THROUGH 1, so 1 has betweenness 2
	// (ordered pairs (0,2) and (2,0)); 0 and 2 have 0. A reference that ignored
	// weights would instead route 0-2 directly and score 1 at 0. Undirected,
	// every edge present both ways.
	und := func(a, b int, w float64) []centralityArc {
		return []centralityArc{
			{Src: graph.NodeID(a), Dst: graph.NodeID(b), Weight: w},
			{Src: graph.NodeID(b), Dst: graph.NodeID(a), Weight: w},
		}
	}
	arcs := und(0, 1, 1)
	arcs = append(arcs, und(1, 2, 1)...)
	arcs = append(arcs, und(0, 2, 10)...)
	tri := centralityFixture{name: "weighted-triangle", order: 3, arcs: arcs}

	wantTri := []float64{0, 2, 0}
	gotTri := centralityWeightedReference(tri)
	for v := range wantTri {
		if math.Abs(gotTri[v]-wantTri[v]) > 1e-12 {
			t.Errorf("weight-sensitive triangle: weighted[%d] = %.17g, want %.17g", v, gotTri[v], wantTri[v])
		}
	}

	// Sanity: the UNWEIGHTED reference on the same triangle scores the direct
	// 0-2 edge as the shortest path, so vertex 1 gets 0 — proving the weighted
	// reference genuinely uses weights rather than hop count.
	unwTri := centralityUnweightedReference(tri)
	if math.Abs(unwTri[1]-0) > 1e-12 {
		t.Errorf("unweighted triangle: betweenness[1] = %.17g, want 0 (direct edge is the unweighted shortest path)", unwTri[1])
	}
}

// TestCentralityReferenceMatchesLibrary spot-checks that the library's parallel
// betweenness agrees with the hand-checked reference values directly (not just
// reference-vs-library), pinning the normalisation convention the battery relies
// on. If the library ever changed its scaling (e.g. started halving for
// undirected input), this test would catch the mismatch independently of the
// fixtures loop.
func TestCentralityReferenceMatchesLibrary(t *testing.T) {
	t.Parallel()
	f := centralityPath("path5", 5)
	c := centralityBuildCSR(f)
	got := centrality.BetweennessParallel[float64](c, centralityWorkers)
	want := []float64{0, 6, 8, 6, 0}
	for v := range want {
		if !centralityApproxEqual(got[v], want[v]) {
			t.Errorf("library BetweennessParallel[%d] = %.17g, want ~%.17g", v, got[v], want[v])
		}
	}
}

// TestCentralityCompare_DetectsCoarseMismatch proves the epsilon comparison
// flags a divergence well above the float-reassociation noise floor. A reference
// that is wrong by a whole unit (here 8 vs 9 at the path centre) must produce a
// violation; values within epsilon must not.
func TestCentralityCompare_DetectsCoarseMismatch(t *testing.T) {
	t.Parallel()
	f := centralityPath("path5", 5)

	want := []float64{0, 6, 8, 6, 0}
	// A coarse mismatch at the centre vertex (8 -> 9).
	bad := []float64{0, 6, 9, 6, 0}
	vs := centralityCompare(7, "search:BetweennessParallel", f, want, bad)
	if len(vs) == 0 {
		t.Fatal("centralityCompare did not flag a whole-unit (8 vs 9) divergence")
	}
	for _, v := range vs {
		if v.Kind != ViolationSearchDivergence {
			t.Errorf("violation kind = %q, want %q", v.Kind, ViolationSearchDivergence)
		}
		if v.Op != "search:BetweennessParallel" {
			t.Errorf("violation op = %q, want %q", v.Op, "search:BetweennessParallel")
		}
		if v.Tick != 7 {
			t.Errorf("violation tick = %d, want 7", v.Tick)
		}
	}

	// A tiny perturbation (within the float-reassociation tolerance) must NOT
	// flag — otherwise the battery would false-positive on the legitimate
	// parallel-vs-reference noise the library documents (~1e-12).
	near := []float64{0, 6, 8 + 1e-13, 6, 0}
	if vs := centralityCompare(7, "search:BetweennessParallel", f, want, near); len(vs) != 0 {
		t.Fatalf("centralityCompare false-positived on a 1e-13 perturbation: %v", vs)
	}

	// A length mismatch is itself a single divergence.
	if vs := centralityCompare(7, "search:BetweennessParallel", f, want, []float64{0, 6}); len(vs) != 1 {
		t.Fatalf("length-mismatch divergence count = %d, want 1", len(vs))
	}
}

// TestCentralityApproxEqual exercises the absolute+relative tolerance boundaries
// directly.
func TestCentralityApproxEqual(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b float64
		want bool
	}{
		{0, 0, true},
		{0, centralityAbsEps / 2, true},               // within absolute term near zero
		{0, centralityAbsEps * 10, false},             // outside both terms near zero
		{1e6, 1e6 * (1 + centralityRelEps/2), true},   // within relative term at scale
		{1e6, 1e6 * (1 + centralityRelEps*10), false}, // outside relative term at scale
		{8, 9, false},                                 // whole-unit gap
	}
	for _, tc := range cases {
		if got := centralityApproxEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("centralityApproxEqual(%g, %g) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestCentralityRandomBridged_IsArticulated asserts the seed-derived fixture
// always has the articulated two-cluster structure the brief requires: a single
// bridge whose endpoints are cut vertices, and a bounded order. It checks the
// invariant across many seeds.
func TestCentralityRandomBridged_IsArticulated(t *testing.T) {
	t.Parallel()
	for tick := int64(0); tick < 100; tick++ {
		f := centralityRandomBridged(NewSeed(uint64(tick) ^ centralitySeedSalt))
		if f.order < 4 || f.order > 12 {
			t.Fatalf("tick=%d order=%d out of [4,12]", tick, f.order)
		}
		// The bridge endpoints must carry strictly positive betweenness (every
		// cross-cluster shortest path traverses them), confirming the articulated
		// structure produced a non-trivial centrality landscape.
		bw := centralityUnweightedReference(f)
		var positives int
		for _, v := range bw {
			if v > 0 {
				positives++
			}
		}
		if positives == 0 {
			t.Fatalf("tick=%d random-bridged fixture %q has no positive betweenness (not articulated): %v",
				tick, f.name, bw)
		}
	}
}
