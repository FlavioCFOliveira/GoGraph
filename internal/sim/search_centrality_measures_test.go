package sim

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestCentralityMeasures_CleanOnFixtures asserts the closeness/harmonic/
// eigenvector/Katz/PPR battery finds no divergence on its deterministic
// fixtures across a spread of ticks: every measure agrees with its independent
// reference. A failure is either a real bug in search/centrality or in a
// reference here, surfaced as a SEARCH_DIVERGENCE.
func TestCentralityMeasures_CleanOnFixtures(t *testing.T) {
	t.Parallel()
	ticks := []int64{0, 1, 2, 3, 7, 11, 42, 99, 1000, 123456, 7654321}
	for _, tick := range ticks {
		if vs := centralityMeasureViolations(tick); len(vs) != 0 {
			t.Errorf("centralityMeasureViolations(%d) = %d violation(s), want 0:", tick, len(vs))
			for _, v := range vs {
				t.Errorf("  %s", v)
			}
		}
	}
}

// TestCentralityMeasures_Deterministic asserts the checker is a pure function of
// the tick.
func TestCentralityMeasures_Deterministic(t *testing.T) {
	t.Parallel()
	for _, tick := range []int64{0, 5, 50, 500, 5000} {
		a := centralityMeasureViolations(tick)
		b := centralityMeasureViolations(tick)
		if len(a) != len(b) {
			t.Fatalf("centralityMeasureViolations(%d) not deterministic: run1=%d run2=%d", tick, len(a), len(b))
		}
	}
}

// TestClosenessReference_Path anchors the closeness reference against a
// hand-computed value on the undirected path 0-1-2-3-4. For the centre vertex 2
// the distances to the four others are {2,1,1,2} summing to 6, reaching r=4 of
// n-1=4 others, so C(2) = (4/4)*(4/6) = 2/3.
func TestClosenessReference_Path(t *testing.T) {
	t.Parallel()
	f := centralityPath("path5", 5)
	ref := closenessReference(f)
	if got, want := ref[2], 2.0/3.0; !centralityApproxEqualEps(got, want, 1e-12, 1e-12) {
		t.Fatalf("closeness reference C(2) = %.17g, want %.17g", got, want)
	}
	// Endpoints reach {1,2,3,4} at distances {1,2,3,4} summing to 10: C(0) =
	// (4/4)*(4/10) = 0.4.
	if got, want := ref[0], 0.4; !centralityApproxEqualEps(got, want, 1e-12, 1e-12) {
		t.Fatalf("closeness reference C(0) = %.17g, want %.17g", got, want)
	}
}

// TestHarmonicReference_Star anchors the harmonic reference on the undirected
// star with hub 0 and 5 leaves. The hub reaches every leaf at distance 1, so
// H(0) = (5 * 1) / (n-1) = 5/5 = 1. A leaf reaches the hub at distance 1 and the
// four other leaves at distance 2: H(leaf) = (1 + 4*0.5)/5 = 3/5.
func TestHarmonicReference_Star(t *testing.T) {
	t.Parallel()
	f := centralityStar("star6", 6)
	ref := harmonicReference(f)
	if got, want := ref[0], 1.0; !centralityApproxEqualEps(got, want, 1e-12, 1e-12) {
		t.Fatalf("harmonic reference H(hub) = %.17g, want %.17g", got, want)
	}
	if got, want := ref[1], 3.0/5.0; !centralityApproxEqualEps(got, want, 1e-12, 1e-12) {
		t.Fatalf("harmonic reference H(leaf) = %.17g, want %.17g", got, want)
	}
}

// TestMeasureCompare_DetectsDivergence proves the comparison predicate flags a
// real disagreement rather than vacuously passing, and stays silent on equal
// vectors within tolerance.
func TestMeasureCompare_DetectsDivergence(t *testing.T) {
	t.Parallel()
	f := centralityPath("path5", 5)
	want := []float64{0, 1, 2, 3, 4}

	// A perturbation an order of magnitude above the tolerance must be flagged.
	bad := []float64{0, 1, 2, 3, 4 + 1e-3}
	if vs := measureCompare(0, "search:Closeness", f, want, bad, closenessHarmonicEps, closenessHarmonicEps); len(vs) == 0 {
		t.Fatal("measureCompare failed to flag a 1e-3 divergence")
	}
	// An identical vector must produce nothing.
	same := []float64{0, 1, 2, 3, 4}
	if vs := measureCompare(0, "search:Closeness", f, want, same, closenessHarmonicEps, closenessHarmonicEps); len(vs) != 0 {
		t.Fatalf("measureCompare flagged identical vectors: %v", vs)
	}
	// A length mismatch must be flagged.
	if vs := measureCompare(0, "search:Closeness", f, want, want[:4], closenessHarmonicEps, closenessHarmonicEps); len(vs) == 0 {
		t.Fatal("measureCompare failed to flag a length mismatch")
	}
}

// TestPPRReference_SumsToOne proves the PPR power-iteration reference produces a
// proper distribution (sums to 1 within tolerance): the teleport plus dangling
// handling conserves mass, which the local-push vector it is compared against
// only approaches from below.
func TestPPRReference_SumsToOne(t *testing.T) {
	t.Parallel()
	seed := NewSeed(0x1234 ^ pprMeasureSalt)
	n, edges := pagerankGenGraph(seed)
	pi := pprReference(n, edges, 0.85, 0)
	var sum float64
	for _, v := range pi {
		sum += v
	}
	if sum < 1-1e-9 || sum > 1+1e-9 {
		t.Fatalf("PPR reference mass = %.12f, want 1.0", sum)
	}
}

// --- Shipped default regimes ---------------------------------------------------

// testCycleFixture builds the undirected n-cycle 0-1-...-(n-1)-0 with unit
// weights. It exists so a Katz injection can hand the engine a graph with the
// SAME maximum in-degree as the reference's graph (2, for both a cycle and a
// path) but a different fixed point — isolating the correctness comparison from
// the auto-alpha contract pin, which would otherwise fire first and mask it.
func testCycleFixture(name string, n int) centralityFixture {
	arcs := make([]centralityArc, 0, 2*n)
	for i := 0; i < n; i++ {
		j := (i + 1) % n
		arcs = append(arcs,
			centralityArc{Src: graph.NodeID(i), Dst: graph.NodeID(j), Weight: 1},
			centralityArc{Src: graph.NodeID(j), Dst: graph.NodeID(i), Weight: 1},
		)
	}
	return centralityFixture{name: name, order: n, arcs: arcs}
}

// assertOneDivergence asserts exactly one violation was produced, tagged
// SEARCH_DIVERGENCE with the expected Op, and returns its message.
func assertOneDivergence(t *testing.T, what, wantOp string, vs []Violation) string {
	t.Helper()
	if len(vs) != 1 {
		t.Fatalf("%s must flag exactly one divergence, got %d (%v)", what, len(vs), vs)
	}
	if vs[0].Kind != ViolationSearchDivergence {
		t.Fatalf("%s: kind = %q, want %q", what, vs[0].Kind, ViolationSearchDivergence)
	}
	if vs[0].Op != wantOp {
		t.Fatalf("%s: Op = %q, want %q", what, vs[0].Op, wantOp)
	}
	return vs[0].Message
}

// TestEigenvectorDefaultRegime_DetectsInjectedDivergence proves the
// default-regime eigenvector check can fail. It cannot perturb
// search/centrality itself, so it perturbs the INPUT: the engine is handed the
// CSR of one graph while the reference is derived from another of the same
// order, and the checker must speak up. Handed matching inputs it must stay
// silent — otherwise the "clean on fixtures" result would prove nothing.
func TestEigenvectorDefaultRegime_DetectsInjectedDivergence(t *testing.T) {
	t.Parallel()
	path := centralityPath("path5", 5)
	star := centralityStar("star5", 5)

	// Agreement: the real fixture judged against its own CSR.
	for _, f := range []centralityFixture{path, star, centralityCompleteUndirected("complete-k4", 4)} {
		if vs := eigenvectorDefaultViolations(0, f, centralityBuildCSR(f)); len(vs) != 0 {
			t.Fatalf("%s under DefaultEigenvectorOptions diverged from its own reference: %v", f.name, vs)
		}
	}

	// Injection: engine sees the star, reference describes the path. Same order,
	// so this is a genuine value divergence and not a length mismatch.
	assertOneDivergence(t, "path reference vs star CSR", opEigenvectorDefaults,
		eigenvectorDefaultViolations(0, path, centralityBuildCSR(star)))
}

// TestKatzDefaultRegime_DetectsInjectedDivergence proves BOTH assertions the
// default-regime Katz check makes can fail, each on its own, and that neither
// fires when the inputs agree.
//
// The two injections are deliberately different shapes. Swapping in a graph with
// the SAME maximum in-degree keeps the auto-alpha contract satisfied and leaves
// only the fixed point wrong, so the correctness comparison is what speaks.
// Swapping in a graph with a DIFFERENT maximum in-degree makes the re-derived
// alpha disagree with the one the library selects, so the contract pin speaks
// first. Asserting on which message appears is what keeps the two apart: a
// single injection could otherwise "prove" an assertion that never actually ran.
func TestKatzDefaultRegime_DetectsInjectedDivergence(t *testing.T) {
	t.Parallel()
	path := centralityPath("path5", 5)     // max in-degree 2 -> alpha 0.85/3
	cycle := testCycleFixture("cycle5", 5) // max in-degree 2 -> alpha 0.85/3
	star := centralityStar("star5", 5)     // max in-degree 4 -> alpha 0.85/5

	// Agreement: every real fixture judged against its own CSR.
	for _, f := range centralityFixtures(NewSeed(7)) {
		if vs := katzDefaultViolations(0, f, centralityBuildCSR(f)); len(vs) != 0 {
			t.Fatalf("%s under DefaultKatzOptions diverged from its own reference: %v", f.name, vs)
		}
	}

	// Injection 1 — correctness arm. Same alpha on both sides, different fixed
	// point, so the contract pin passes and the value comparison must fire.
	msg := assertOneDivergence(t, "path reference vs cycle CSR", opKatzDefaults,
		katzDefaultViolations(0, path, centralityBuildCSR(cycle)))
	if strings.Contains(msg, "auto-alpha contract broken") {
		t.Fatalf("expected the correctness arm to fire, but the contract pin did: %s", msg)
	}

	// Injection 2 — auto-alpha contract arm. Different maximum in-degree, so the
	// re-derived alpha is not the one the library picks.
	msg = assertOneDivergence(t, "path reference vs star CSR", opKatzDefaults,
		katzDefaultViolations(0, path, centralityBuildCSR(star)))
	if !strings.Contains(msg, "auto-alpha contract broken") {
		t.Fatalf("expected the auto-alpha contract pin to fire, got: %s", msg)
	}
}

// TestKatzAutoAlphaReference_MatchesDocumentedFormula anchors the independent
// re-derivation of the auto-selected attenuation factor against hand-computed
// values, so the re-derivation is pinned by arithmetic done here and not merely
// by agreeing with the library it is meant to judge.
//
// [centrality.KatzOptions] documents alpha = 0.85 / (1 + maxInDegree) for the
// Alpha <= 0 sentinel, where the in-degree counts arcs ARRIVING at a node.
func TestKatzAutoAlphaReference_MatchesDocumentedFormula(t *testing.T) {
	t.Parallel()
	const num = katzAutoAlphaNumerator
	cases := []struct {
		f     centralityFixture
		maxIn int
		want  float64
	}{
		// Undirected path: the three interior vertices each receive 2 arcs.
		{centralityPath("path5", 5), 2, num / 3},
		// Undirected star with 5 leaves: the hub receives 5 arcs.
		{centralityStar("star6", 6), 5, num / 6},
		// Directed chain: NOT symmetrised, so every vertex receives exactly 1.
		{centralityDirectedChain("directed-chain5", 5), 1, num / 2},
		// K4 undirected: every vertex receives 3.
		{centralityCompleteUndirected("complete-k4", 4), 3, num / 4},
		// Directed diamond 0->1, 0->2, 1->3, 2->3: the sink receives 2.
		{centralityDirectedDiamond("directed-diamond"), 2, num / 3},
		// No arcs at all: no degree bound, so the documented fallback is 0.85.
		{centralityIsolated("isolated-no-edges", 4), 0, num},
	}
	for _, tc := range cases {
		if got := katzAutoAlphaReference(tc.f); got != tc.want {
			t.Errorf("katzAutoAlphaReference(%s) = %.17g, want %.17g (maxInDegree %d)",
				tc.f.name, got, tc.want, tc.maxIn)
		}
	}
}

// TestPPRDefaultRegime_DetectsInjectedDivergence proves the default-regime
// personalised-PageRank check can fail, using the same input-perturbation shape
// as the other two arms: the engine is handed one graph and the reference
// another of the same order.
func TestPPRDefaultRegime_DetectsInjectedDivergence(t *testing.T) {
	t.Parallel()
	// A: spine 0->1->2->3->4 with a back edge 2->0; 4 is a dangling sink.
	a := [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {2, 0}}
	// B: a hub fan-out from the seed; 3 and 4 are dangling.
	b := [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 0}, {2, 0}}

	// Agreement: matching engine and reference inputs must flag nothing, both on
	// the hand-built graph and on the graphs the tick generator produces.
	if vs := pprDefaultRegimeViolations(0, pagerankBuildCSR(5, a), 5, a); len(vs) != 0 {
		t.Fatalf("matching inputs flagged a divergence: %v", vs)
	}
	for _, tick := range []int64{0, 1, 42, 7654321} {
		n, edges := pagerankGenGraph(NewSeed(uint64(tick) ^ pprMeasureSalt))
		if vs := pprDefaultRegimeViolations(tick, pagerankBuildCSR(n, edges), n, edges); len(vs) != 0 {
			t.Fatalf("tick %d: default PPR regime diverged on its own fixture: %v", tick, vs)
		}
	}

	// Injection: engine walks B, reference describes A.
	assertOneDivergence(t, "graph A reference vs graph B CSR", opPPRDefaults,
		pprDefaultRegimeViolations(0, pagerankBuildCSR(5, b), 5, a))

	// And the same injection through the grouping the battery actually calls,
	// proving the default regime is REACHED from it. [pprViolations] rebuilds
	// its own (correct) fixture from the tick, so it stays silent and the only
	// Op that can appear is the default one. Drop the default arm from
	// pprMeasureRegimeViolations and this assertion fails.
	assertOneDivergence(t, "default regime reached from pprMeasureRegimeViolations", opPPRDefaults,
		pprMeasureRegimeViolations(0, pagerankBuildCSR(5, b), 5, a))

	// Control for the grouping: matching inputs, both regimes silent.
	n0, e0 := pagerankGenGraph(NewSeed(uint64(0) ^ pprMeasureSalt))
	if vs := pprMeasureRegimeViolations(0, pagerankBuildCSR(n0, e0), n0, e0); len(vs) != 0 {
		t.Fatalf("both PPR regimes on their own fixture flagged %d violation(s): %v", len(vs), vs)
	}
}

// TestDefaultRegimeTolerances_Discriminate pins the size of defect each measured
// tolerance still catches. A perturbation just above the constant must be
// flagged and one comfortably below it must not — the property that makes the
// constant a tripwire rather than either a source of flakes or a rubber stamp.
//
// The vectors are L2-normalised centrality scores, so a component of ~0.45 is
// representative; at that scale the combined predicate's relative arm
// (eps * 0.45) is smaller than its absolute arm, so the absolute epsilon is what
// binds and these are the numbers that matter.
func TestDefaultRegimeTolerances_Discriminate(t *testing.T) {
	t.Parallel()
	f := centralityPath("path5", 5)
	base := []float64{0.3717, 0.6015, 0.6533, 0.6015, 0.3717}

	perturb := func(delta float64) []float64 {
		out := make([]float64, len(base))
		copy(out, base)
		out[2] += delta
		return out
	}

	for _, tc := range []struct {
		name string
		eps  float64
		op   string
	}{
		{"eigenvector", eigenvectorDefaultEps, opEigenvectorDefaults},
		{"katz", katzDefaultEps, opKatzDefaults},
	} {
		if vs := measureCompare(0, tc.op, f, base, perturb(2*tc.eps), tc.eps, tc.eps); len(vs) != 1 {
			t.Errorf("%s: a %g perturbation (2x the tolerance) must be flagged, got %d violation(s)", tc.name, 2*tc.eps, len(vs))
		}
		if vs := measureCompare(0, tc.op, f, base, perturb(tc.eps/2), tc.eps, tc.eps); len(vs) != 0 {
			t.Errorf("%s: a %g perturbation (half the tolerance) must NOT be flagged, got %v", tc.name, tc.eps/2, vs)
		}
	}
}

// TestCentralityFixtureShapes_MatchToleranceEvidence turns the evidence recorded
// on [eigenvectorDefaultEps] and [katzDefaultEps] into an assertion.
//
// Those two constants are documented as EXHAUSTIVE rather than sampled, on the
// grounds that both measures read only the adjacency and that
// [centralityFixtures] can produce exactly 23 distinct shapes: 7 fixed plus the
// 16 (a, b) size combinations of [centralityRandomBridged]. That claim silently
// stops being true the moment a fixture is added or a size range widened, and a
// stale tolerance is exactly the kind of defect a green suite hides. This test
// fails in that case, which is the signal to re-measure.
func TestCentralityFixtureShapes_MatchToleranceEvidence(t *testing.T) {
	t.Parallel()
	const ticks = 3000
	seen := map[string]bool{}
	for tick := int64(0); tick < ticks; tick++ {
		for _, f := range centralityFixtures(NewSeed(uint64(tick) ^ centralityMeasureSalt)) {
			seen[f.name] = true
		}
	}
	const wantShapes = 23
	if len(seen) != wantShapes {
		names := make([]string, 0, len(seen))
		for k := range seen {
			names = append(names, k)
		}
		t.Fatalf("centralityFixtures yields %d distinct shapes over %d ticks, want %d — the measured "+
			"default-regime tolerances were established over exactly %d shapes and must be re-measured: %v",
			len(seen), ticks, wantShapes, wantShapes, names)
	}
}

// TestCentralityFixtureViolations_DrivesEveryRegime proves the shipped-default
// checks are actually REACHED from the per-fixture battery, not merely present
// in the file.
//
// A green battery cannot distinguish "the default-regime check ran and agreed"
// from "the default-regime check was dropped from the list". So the engine is
// handed the star's CSR while the references describe the path — every check in
// the list then has something to disagree about — and the set of Ops that come
// back must contain all six, the three default-regime labels included. Delete
// any one call from [centralityFixtureViolations] and this test fails.
func TestCentralityFixtureViolations_DrivesEveryRegime(t *testing.T) {
	t.Parallel()
	path := centralityPath("path5", 5)
	star := centralityStar("star5", 5)

	// Control: matching engine and reference inputs, every real fixture, silent.
	for _, f := range centralityFixtures(NewSeed(11)) {
		if vs := centralityFixtureViolations(0, f, centralityBuildCSR(f)); len(vs) != 0 {
			t.Fatalf("%s judged against its own CSR flagged %d violation(s): %v", f.name, len(vs), vs)
		}
	}

	got := map[string]bool{}
	for _, v := range centralityFixtureViolations(0, path, centralityBuildCSR(star)) {
		if v.Kind != ViolationSearchDivergence {
			t.Errorf("violation kind = %q, want %q (%s)", v.Kind, ViolationSearchDivergence, v.Op)
		}
		got[v.Op] = true
	}
	for _, op := range []string{
		"search:Closeness",
		"search:Harmonic",
		"search:Katz",
		opKatzDefaults,
		"search:Eigenvector",
		opEigenvectorDefaults,
	} {
		if !got[op] {
			t.Errorf("check %q did not fire on a mismatched CSR — it is not wired into centralityFixtureViolations (fired: %v)", op, got)
		}
	}
}
