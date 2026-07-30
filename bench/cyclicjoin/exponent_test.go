package cyclicjoin_test

// exponent_test.go — fits the scaling exponent of both arms from a measured series
// (rmp #2159).
//
// Layer: short.
//
// The task this belongs to asked for exponents fitted from a sweep, with the
// disabled series expected to approach 2.0 and the enabled series 1.5. SPIKE #2155
// REFUTED that framing before any of this was built, and the refutation is what this
// test exists to keep on the record rather than quietly drop:
//
//   - `Θ(m²)` versus `Θ(m^1.5)` compares two WORST-CASE bounds, not two plans on a
//     graph. On a given graph the work terms are `Σ_v d_in(v)·d_out(v)` for the
//     binary-join plan and `Σ_(a,b) min(d_out(b), d_in(a))` for the intersection —
//     and those are EXACTLY EQUAL on any regular graph.
//   - So on a uniform-degree fixture the exponents of the two arms should be CLOSE
//     TO EACH OTHER, and the win must come from the constant: fewer materialised
//     intermediate rows and a sequential merge in place of a per-candidate probe.
//
// The exponent is fitted in EDGE COUNT m, and the regime is stated with it, because
// the same quantity is quadratic when degree grows at fixed n and linear when n
// grows at fixed degree. An exponent quoted without its regime is meaningless — that
// was one of the SPIKE's three premise refutations.
//
// This is a test rather than a benchmark so it runs in the normal gate and cannot
// silently rot, and it asserts only what the measurement robustly supports: both
// exponents positive and finite, and the fused arm's not worse than the other's.

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bench/cyclicjoin"
	"github.com/FlavioCFOliveira/GoGraph/bench/expandinto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// timeQuery returns the median wall time of runs executions of q, after a warm-up.
// A median rather than a mean, so one scheduler hiccup cannot move the fit.
func timeQuery(t *testing.T, eng *cypher.Engine, q string, runs int) float64 {
	t.Helper()
	samples := make([]float64, 0, runs)
	for i := 0; i < runs; i++ {
		start := time.Now()
		res, err := eng.Run(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		for res.Next() { //nolint:revive // full drain
		}
		if err := res.Err(); err != nil {
			t.Fatalf("Err: %v", err)
		}
		_ = res.Close()
		samples = append(samples, float64(time.Since(start)))
	}
	// Insertion sort: runs is tiny.
	for i := 1; i < len(samples); i++ {
		for j := i; j > 0 && samples[j] < samples[j-1]; j-- {
			samples[j], samples[j-1] = samples[j-1], samples[j]
		}
	}
	return samples[len(samples)/2]
}

// TestCyclicJoin_FittedExponents fits both arms over a degree sweep at fixed node
// count — the DENSE regime, in which m = n·(d+1) grows with degree and the
// binary-join plan's two-path term is Θ(m²/n).
func TestCyclicJoin_FittedExponents(t *testing.T) {
	const nodes = 2000
	degrees := []int{4, 8, 16, 32}

	// Pre-sized: the upper bound is exactly len(degrees), per the project's
	// pre-size-every-slice mandate.
	ms := make([]int, 0, len(degrees))
	twoCosts := make([]float64, 0, len(degrees))
	fusedCosts := make([]float64, 0, len(degrees))
	for _, d := range degrees {
		g, err := cyclicjoin.SeedUniform(nodes, d)
		if err != nil {
			t.Fatalf("SeedUniform(%d, %d): %v", nodes, d, err)
		}
		m := nodes * (d + 1) // each node contributes d forward edges plus one back-edge
		ms = append(ms, m)

		for _, fused := range []bool{false, true} {
			eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{EnableCyclicIntersect: fused})
			// Warm the CSR pair cache so the first timed run is not a build.
			_ = timeQuery(t, eng, cyclicjoin.TriangleQuery, 1)
			cost := timeQuery(t, eng, cyclicjoin.TriangleQuery, 5)
			if fused {
				fusedCosts = append(fusedCosts, cost)
			} else {
				twoCosts = append(twoCosts, cost)
			}
		}
	}

	twoExp := expandinto.FitExponent(ms, twoCosts)
	fusedExp := expandinto.FitExponent(ms, fusedCosts)
	t.Logf("regime: DENSE (fixed n=%d, degree %v, so m grows with degree)", nodes, degrees)
	t.Logf("m series: %v", ms)
	t.Logf("two-Expand costs (ns): %.0f", twoCosts)
	t.Logf("fused costs (ns):      %.0f", fusedCosts)
	t.Logf("FITTED EXPONENT IN m — two-Expand %.3f | fused %.3f", twoExp, fusedExp)

	if math.IsNaN(twoExp) || math.IsNaN(fusedExp) {
		t.Fatalf("a fit returned NaN (two=%v fused=%v); the series is degenerate", twoExp, fusedExp)
	}
	if twoExp <= 0 || fusedExp <= 0 {
		t.Fatalf("a fitted exponent is non-positive (two=%.3f fused=%.3f), which cannot describe "+
			"a cost that grows with input size", twoExp, fusedExp)
	}
	// The load-bearing assertion, and deliberately the only inequality asserted: the
	// fused arm must not scale WORSE. A tight band around a specific value would be
	// asserting machine timing, which this project has repeatedly been misled by.
	if fusedExp > twoExp+0.35 {
		t.Fatalf("fused exponent %.3f is materially worse than two-Expand %.3f — the fusion "+
			"should never scale worse, since its work term is bounded by the plan it replaces "+
			"(Sum min <= half Sum d^2 pointwise)", fusedExp, twoExp)
	}
	// Every point must be a win, which is a stronger and more robust claim than the
	// exponent and is what the default-enable recommendation rests on.
	for i := range ms {
		if fusedCosts[i] >= twoCosts[i] {
			t.Fatalf("at m=%d the fused arm (%.0f ns) is not faster than two-Expand (%.0f ns)",
				ms[i], fusedCosts[i], twoCosts[i])
		}
	}
}
