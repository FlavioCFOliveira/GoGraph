package expandinto_test

// exponent_test.go — the exponent fitter's own contract (#2152).
//
// Layer: short. The TIMING-based exponent sweeps live in exponent_soak_test.go,
// gated to the soak layer, because `make ci` runs the short layer under `-race`
// and a timing measurement there is not meaningful: race instrumentation inflates
// every operation by roughly an order of magnitude and does not inflate the fixed
// and the degree-dependent costs equally, so a fitted exponent shifts. Measured
// directly — the triangle's exponent read 1.562 without `-race` and 1.332 with it,
// on identical code — and the sweeps also took 163 s under `-race`, well over the
// short layer's per-package budget.

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/expandinto"
)

// TestFitExponent_RejectsBadInput pins the fit's own contract: it must return NaN
// rather than a plausible number when it cannot compute an exponent. A harness that
// silently returns a wrong figure with full confidence is the failure mode this whole
// package's comments warn about.
func TestFitExponent_RejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		degrees []int
		costs   []float64
	}{
		{"length mismatch", []int{1, 2}, []float64{1}},
		{"single point", []int{1}, []float64{1}},
		{"empty", nil, nil},
		{"zero degree", []int{0, 2}, []float64{1, 2}},
		{"zero cost", []int{1, 2}, []float64{0, 2}},
		{"negative cost", []int{1, 2}, []float64{-1, 2}},
		{"repeated degree (no spread)", []int{4, 4}, []float64{1, 2}},
	} {
		if got := expandinto.FitExponent(tc.degrees, tc.costs); !math.IsNaN(got) {
			t.Fatalf("%s: FitExponent = %v, want NaN", tc.name, got)
		}
	}
	// A clean power law must be recovered exactly: cost = d^2.
	exp := expandinto.FitExponent([]int{2, 4, 8, 16}, []float64{4, 16, 64, 256})
	if math.Abs(exp-2) > 1e-9 {
		t.Fatalf("FitExponent on an exact d^2 series = %v, want 2", exp)
	}
}
