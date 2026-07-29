//go:build soak || nightly

package expandinto_test

// exponent_soak_test.go — the asymptotic assertions (#2152).
//
// Layer: soak. See exponent_test.go for why these are not short-layer: `make ci`
// runs the short layer under `-race`, where a fitted exponent is not a meaningful
// measurement.
//
//	go test -run Exponent -v -tags=soak ./bench/expandinto/
//
// The sprint's claim about the closing hop is that its growth in out-degree changes
// KIND: Θ(d) work per input row becomes O(log d + r). A benchmark at one degree cannot
// show that, and a ratio between two degrees is at the mercy of whichever endpoint
// carries the most fixed cost. So the exponent is FITTED over a sweep and asserted.
//
// Both assertions here are RELATIVE — one arm against the other, or one shape against
// another. An absolute threshold on a timing-derived quantity is a flake generator: the
// first version of the triangle case asserted a floor of 1.4 and failed under `-race`
// at 1.332 on correct code.

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bench/expandinto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// bestOf runs q reps times after a warm-up and returns the FASTEST wall time.
//
// The minimum, not the mean: a benchmark's noise is one-sided — scheduling, GC and
// thermal effects only ever ADD time — so the minimum is the least contaminated
// estimator of the work itself, which is what an exponent must be fitted from.
func bestOf(tb testing.TB, eng *cypher.Engine, q string, reps int) time.Duration {
	tb.Helper()
	run := func() {
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			tb.Fatalf("Run(%q): %v", q, err)
		}
		for res.Next() { //nolint:revive // intentional full drain
		}
		if err := res.Err(); err != nil {
			tb.Fatalf("Err(%q): %v", q, err)
		}
		_ = res.Close()
	}
	run() // warm the CSR pair cache so its build is not timed
	best := time.Duration(math.MaxInt64)
	for i := 0; i < reps; i++ {
		start := time.Now()
		run()
		if el := time.Since(start); el < best {
			best = el
		}
	}
	return best
}

// exponentNodes is smaller than the benchmarks' 20 000 so a whole sweep, including the
// disabled arm at the top degree, stays affordable. The exponent is scale-free in the
// node count: n multiplies every point by the same factor, and a least-squares slope in
// log space is unchanged by that.
const exponentNodes = 3000

// sweepExponent fits the growth exponent of q over degrees, with the seek on or off.
func sweepExponent(t *testing.T, q string, degrees []int, seek bool) (float64, []float64) {
	t.Helper()
	costs := make([]float64, 0, len(degrees))
	for _, d := range degrees {
		g, err := expandinto.SeedRing(exponentNodes, d, true)
		if err != nil {
			t.Fatalf("SeedRing(%d, %d): %v", exponentNodes, d, err)
		}
		eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableExpandIntoSeek: !seek})
		costs = append(costs, float64(bestOf(t, eng, q, 3).Nanoseconds()))
	}
	return expandinto.FitExponent(degrees, costs), costs
}

// TestClosingHopExponentFalls asserts the seek moves the 2-cycle shape's growth
// exponent down by a wide margin.
//
// The margin is deliberately generous (0.25) rather than tight against the measured
// figures. A tight bound on a timing-derived quantity is a flake generator on a shared
// machine, and the property worth gating is that the exponent moved KIND, not that it
// landed on a particular decimal. The recorded figures at HEAD, for reference, are
// 1.72 disabled and 1.09 enabled over degrees 8→64 at 20 000 nodes.
func TestClosingHopExponentFalls(t *testing.T) {
	degrees := []int{8, 16, 32, 64}

	offExp, offCosts := sweepExponent(t, expandinto.ClosingQuery, degrees, false)
	onExp, onCosts := sweepExponent(t, expandinto.ClosingQuery, degrees, true)

	t.Logf("closing hop, n=%d, degrees=%v", exponentNodes, degrees)
	for i, d := range degrees {
		t.Logf("  degree %-4d filter=%-12s seek=%-12s speedup=%.2fx",
			d, time.Duration(offCosts[i]), time.Duration(onCosts[i]), offCosts[i]/onCosts[i])
	}
	t.Logf("  fitted exponent: filter=%.3f seek=%.3f", offExp, onExp)

	if math.IsNaN(offExp) || math.IsNaN(onExp) {
		t.Fatalf("exponent fit produced NaN: filter=%v seek=%v", offExp, onExp)
	}
	const margin = 0.25
	if onExp > offExp-margin {
		t.Fatalf("the bound-destination seek no longer changes the growth exponent: "+
			"filter=%.3f seek=%.3f (need seek <= filter-%.2f). Either the seek stopped "+
			"engaging on this shape, or the closing hop is enumerating again",
			offExp, onExp, margin)
	}
	// The seek must also be faster at the TOP of the sweep, where its advantage is
	// largest. A fitted exponent alone could in principle fall while every point got
	// slower.
	top := len(degrees) - 1
	if onCosts[top] >= offCosts[top] {
		t.Fatalf("at degree %d the seek (%s) is not faster than the filter (%s)",
			degrees[top], time.Duration(onCosts[top]), time.Duration(offCosts[top]))
	}
}

// TestTriangleExponentStaysAboveTheClosingHop pins the LIMIT of the change, so a
// future reader does not expect a win the plan shape cannot deliver.
//
// A triangle's middle hop is OPEN, so it materialises Θ(n·d²) intermediate rows however
// the closing hop executes. The seek removes the closing hop's Θ(d) factor, but the
// intermediate result is inherent to the pattern and Neo4j pays it too. So the
// triangle's exponent must stay materially ABOVE the 2-cycle's, which is what stops the
// sprint's headline being over-claimed across shapes.
//
// The comparison is RELATIVE — triangle against 2-cycle, both with the seek enabled —
// deliberately. An absolute floor was tried first and failed under `-race` at 1.332
// against a threshold of 1.4, on correct code, because race instrumentation does not
// inflate fixed and degree-dependent costs equally. A relative bound is invariant to
// that, since both shapes are measured in the same regime.
func TestTriangleExponentStaysAboveTheClosingHop(t *testing.T) {
	degrees := []int{4, 8, 16}
	triExp, triCosts := sweepExponent(t, expandinto.TriangleQuery, degrees, true)
	cycExp, _ := sweepExponent(t, expandinto.ClosingQuery, degrees, true)

	t.Logf("n=%d, degrees=%v: triangle exponent=%.3f, 2-cycle exponent=%.3f",
		exponentNodes, degrees, triExp, cycExp)
	for i, d := range degrees {
		t.Logf("  triangle degree %-4d seek=%s", d, time.Duration(triCosts[i]))
	}
	if math.IsNaN(triExp) || math.IsNaN(cycExp) {
		t.Fatalf("exponent fit produced NaN: triangle=%v cycle=%v", triExp, cycExp)
	}
	const gap = 0.3
	if triExp < cycExp+gap {
		t.Fatalf("the triangle's growth exponent (%.3f) is no longer materially above the "+
			"2-cycle's (%.3f, need +%.2f). Either the seek has started removing the open "+
			"middle hop's Theta(n*d^2) intermediate result — which would be a surprising WIN "+
			"that contradicts the recorded analysis and must be re-derived before it is "+
			"quoted — or this harness has stopped measuring what it claims",
			triExp, cycExp, gap)
	}
}
