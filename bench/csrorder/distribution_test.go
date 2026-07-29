package csrorder

import (
	"testing"
)

// distribution_test.go — the permanent degree-distribution report.
//
// docs/design-degree-adaptive-adjacency.md §12 records that the degree
// distribution behind §2.4 was measured with "a temporary test ... removed after
// use", and §2.4 itself is refuted partly BECAUSE its harness was retained
// outside the repository and so cannot be re-run. rmp #2145 requires that
// measurement be made permanent instead. This file is that permanence.
//
// The short-layer test reports the benchmark fixtures' own distributions. The
// soak-layer test in distribution_soak_test.go reproduces §2.4's published rows at
// their original parameters and asserts the costFrac figures, which is what turns
// §2.4's surviving table into something a future reader can verify rather than
// inherit on trust.

// reportThresholds are the two thresholds every distribution is reported at: the
// CALIBRATED crossover of 16 from §2.2, and the 64 the refuted §2.4 assumed. Both
// are shown so the cost of having believed 64 stays legible — at 64 roughly a
// third of the available scan cost sits in the band it recommended keeping linear.
var reportThresholds = []int{probeThreshold, auditThreshold}

// TestFixtureDegreeDistribution reports the out-degree distribution of every
// fixture this package benchmarks, at both thresholds.
//
// It is a reporting test, so it asserts only the invariants that would indicate a
// broken fixture; the numbers themselves are output for a human to read alongside
// a benchmark run. Run it with -v to see the table:
//
//	go test -run TestFixtureDegreeDistribution -v ./bench/csrorder/...
func TestFixtureDegreeDistribution(t *testing.T) {
	t.Parallel()

	type entry struct {
		name  string
		build func(threshold int) (*Fixture, error)
	}
	entries := []entry{
		{"barabasi-albert (PRIMARY)", PowerLawFixture},
		{"hub uniform d=8", func(th int) (*Fixture, error) { return HubFixture(8, th) }},
		{"hub uniform d=16", func(th int) (*Fixture, error) { return HubFixture(16, th) }},
		{"hub uniform d=64", func(th int) (*Fixture, error) { return HubFixture(64, th) }},
	}

	for _, e := range entries {
		for _, th := range reportThresholds {
			f, err := e.build(th)
			if err != nil {
				t.Fatalf("%s: build: %v", e.name, err)
			}
			t.Logf("%-26s %s", e.name, f.Profile)

			if f.Profile.Sources == 0 {
				t.Errorf("%s: fixture has no arc-bearing sources", e.name)
			}
			if f.Profile.Arcs == 0 {
				t.Errorf("%s: fixture has no arcs", e.name)
			}
			// CostFrac weights by d², so it can never be below EdgeFrac, which
			// weights by d, which can never be below VertexFrac. If this ordering
			// ever inverts, the cost model is computed wrongly and every benchmark
			// result read against it is misattributed.
			if f.Profile.CostFrac < f.Profile.EdgeFrac {
				t.Errorf("%s T=%d: costFrac %.4f < edgeFrac %.4f — the d² weighting is wrong",
					e.name, th, f.Profile.CostFrac, f.Profile.EdgeFrac)
			}
			if f.Profile.EdgeFrac < f.Profile.VertexFrac {
				t.Errorf("%s T=%d: edgeFrac %.4f < vertexFrac %.4f — the d weighting is wrong",
					e.name, th, f.Profile.EdgeFrac, f.Profile.VertexFrac)
			}
		}
	}
}

// TestPowerLawFixtureIsSkewed asserts the PRIMARY fixture is genuinely a power law
// and not accidentally uniform.
//
// This is the guard against the failure mode #2147 documents for example 26: a
// fixture that generates a UNIFORM out-degree cannot demonstrate a hub win, and if
// this fixture silently became uniform then BenchmarkTraversalPowerLaw would be
// measuring the same thing as BenchmarkTraversalReverse while being quoted as the
// realistic case.
//
// The thresholds are deliberately loose — this asserts the SHAPE, not a pinned
// value, because the generator's exact tail depends on its seed and on n.
func TestPowerLawFixtureIsSkewed(t *testing.T) {
	t.Parallel()

	f, err := PowerLawFixture(probeThreshold)
	if err != nil {
		t.Fatalf("PowerLawFixture: %v", err)
	}
	p := f.Profile

	// A hub tail must exist: the maximum degree must be far above the median.
	if p.MaxDegree < 8*p.P50 {
		t.Errorf("max degree %d is only %.1fx the median %d — the fixture is not "+
			"meaningfully skewed and cannot stand in for a real property graph",
			p.MaxDegree, float64(p.MaxDegree)/float64(p.P50), p.P50)
	}
	// A power law concentrates scan cost far above the vertex share. On §2.4's
	// n=100k m=8 row this gap is 89.21% cost against 23.49% of vertices.
	if p.CostFrac <= 2*p.VertexFrac {
		t.Errorf("costFrac %.4f is not concentrated relative to vertexFrac %.4f — "+
			"the fixture's degree distribution has lost its tail", p.CostFrac, p.VertexFrac)
	}
	t.Logf("power-law fixture: %s", p)
}

// TestRMATOverstatesTheWin reproduces the specific trap §2.4 recorded and §8
// forbids benchmarking into: RMAT concentrates far more scan cost above the
// threshold than a power law does, so a change measured only on RMAT looks better
// than it will ever be on a real graph.
//
// It asserts the DIRECTION of the gap rather than §2.4's exact percentages, which
// the soak-layer test pins at the original parameters. Direction is the part that
// must never silently flip: if RMAT ever stopped overstating, the RMAT fixture
// would no longer be serving as a contrast and the reason it exists would be gone.
func TestRMATOverstatesTheWin(t *testing.T) {
	t.Parallel()

	powerLaw, err := PowerLawFixture(auditThreshold)
	if err != nil {
		t.Fatalf("PowerLawFixture: %v", err)
	}
	rmat, err := RMATFixture(auditThreshold)
	if err != nil {
		t.Fatalf("RMATFixture: %v", err)
	}

	t.Logf("T=%d costFrac — power-law %.2f%%  rmat %.2f%%",
		auditThreshold, 100*powerLaw.Profile.CostFrac, 100*rmat.Profile.CostFrac)

	if rmat.Profile.CostFrac <= powerLaw.Profile.CostFrac {
		t.Errorf("RMAT costFrac %.4f is not above the power law's %.4f at T=%d — "+
			"the RMAT fixture exists only as the contrast that makes the "+
			"RMAT-only trap visible, and it is no longer serving that purpose",
			rmat.Profile.CostFrac, powerLaw.Profile.CostFrac, auditThreshold)
	}
}
