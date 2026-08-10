// Package main_test — the decision rule shared by every soak instrument that
// fits a regression, and its regression test.
//
// This file carries no build tag on purpose. The rule it encodes was violated
// by the soak layer itself (rmp #2396), so it is pinned by the SHORT layer —
// i.e. by `make ci` — rather than only by the soak layer whose defect it
// describes. A gate's own logic must not be guarded exclusively by the tag that
// failed to exercise it.
package main_test

import "testing"

// minRegressionPoints is the smallest number of samples this project will fit a
// slope to.
//
// The bound is about residual degrees of freedom, not taste. Ordinary
// least-squares through n points spends 2 of them on the fit itself, so n=2
// reproduces both samples exactly and yields a slope carrying no information
// whatever about noise; n=3 leaves a single residual, so one outlier still
// dictates the answer. At n=6 there are 4 residual degrees of freedom, the
// smallest count at which a single anomalous sample cannot determine the fitted
// slope. The full soak variants collect hundreds of samples and are unaffected;
// this floor exists to stop a SHORT window reporting a confident slope drawn
// from two points, which is what the 2026-08-10 certification quoted.
const minRegressionPoints = 6

// slopeGateOutcome is what an instrument must do about its slope check.
type slopeGateOutcome int

const (
	// slopeGateAssert — enough samples: fit the regression and assert on it.
	slopeGateAssert slopeGateOutcome = iota
	// slopeGateSkip — too few samples, and this run was not expected to assert.
	// The test must SKIP, so the absence of an assertion is visible, rather than
	// logging a note and returning success.
	slopeGateSkip
	// slopeGateFail — too few samples in a run that WAS expected to assert. A
	// mandated acceptance criterion must never be reported as met by a run that
	// never evaluated it.
	slopeGateFail
)

func (o slopeGateOutcome) String() string {
	switch o {
	case slopeGateAssert:
		return "assert"
	case slopeGateSkip:
		return "skip"
	case slopeGateFail:
		return "fail"
	default:
		return "unknown"
	}
}

// slopeGateDecide maps a sample count and whether the run was expected to assert
// onto the action the instrument must take.
//
// The whole of rmp #2396 is the third row of this table. Before it, an
// instrument with too few samples logged a line and returned success in EVERY
// configuration, including the full-length variants whose entire purpose is to
// evaluate the criterion — so "soak layer green" was compatible with nothing
// having been checked.
func slopeGateDecide(samples int, mandated bool) slopeGateOutcome {
	if samples >= minRegressionPoints {
		return slopeGateAssert
	}
	if mandated {
		return slopeGateFail
	}
	return slopeGateSkip
}

// TestSlopeGateDecide pins the decision table. It runs in the short layer.
func TestSlopeGateDecide(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		samples  int
		mandated bool
		want     slopeGateOutcome
	}{
		// Enough samples: assert, regardless of whether the run was mandated.
		{"at the floor, not mandated", minRegressionPoints, false, slopeGateAssert},
		{"at the floor, mandated", minRegressionPoints, true, slopeGateAssert},
		{"far above the floor", 329, true, slopeGateAssert},

		// Too few samples in a run that was NOT expected to assert: skip, so the
		// missing assertion is visible in the test output.
		{"short layer, one sample", 1, false, slopeGateSkip},
		{"short layer, two samples (the 2026-08-10 no_growth case)", 2, false, slopeGateSkip},
		{"short layer, zero windows (the 2026-08-10 p99 case)", 0, false, slopeGateSkip},
		{"short layer, one below the floor", minRegressionPoints - 1, false, slopeGateSkip},

		// Too few samples in a run that WAS expected to assert: fail. This is the
		// row whose absence made the soak layer a gate that could not fail.
		{"mandated, one sample", 1, true, slopeGateFail},
		{"mandated, zero samples", 0, true, slopeGateFail},
		{"mandated, one below the floor", minRegressionPoints - 1, true, slopeGateFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := slopeGateDecide(tc.samples, tc.mandated); got != tc.want {
				t.Errorf("slopeGateDecide(samples=%d, mandated=%t) = %s, want %s",
					tc.samples, tc.mandated, got, tc.want)
			}
		})
	}
}

// TestSlopeGateFloorRejectsExactFits guards the reason the floor exists rather
// than its current value: whatever minRegressionPoints is set to, it must never
// admit a sample count that ordinary least-squares fits exactly. Two points
// always fit a line with zero residual, and three leave a single residual, so
// both must be rejected — otherwise the instrument reports a slope that is an
// artefact of the fit rather than a measurement of the workload.
func TestSlopeGateFloorRejectsExactFits(t *testing.T) {
	t.Parallel()
	if minRegressionPoints < 4 {
		t.Fatalf("minRegressionPoints = %d admits a fit with fewer than 2 residual "+
			"degrees of freedom; a slope from that few points is an artefact",
			minRegressionPoints)
	}
	for _, n := range []int{0, 1, 2, 3} {
		if got := slopeGateDecide(n, false); got != slopeGateSkip {
			t.Errorf("slopeGateDecide(%d, false) = %s, want skip: too few points to regress", n, got)
		}
		if got := slopeGateDecide(n, true); got != slopeGateFail {
			t.Errorf("slopeGateDecide(%d, true) = %s, want fail: mandated run must not pass without asserting", n, got)
		}
	}
}
