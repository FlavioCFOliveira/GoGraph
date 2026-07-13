package sim

import "testing"

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
