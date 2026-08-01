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
//
// WHY THE PER-POINT WIN IS ASSERTED IN ALLOCATIONS AND NOT IN WALL CLOCK (rmp #2268).
// This test used to require `fusedCosts[i] < twoCosts[i]` at every point. That is a
// strict inequality between two wall-clock medians, and `make ci` runs the whole
// repository in parallel at the coverage step, so the two arms do not see the same
// machine. It duly passed in `test-short` and FAILED at `cover-gate` within one
// invocation — 40.31 ms against 39.13 ms, a 3 % miss — turning the local gate red
// with no defect present. A gate that cannot distinguish a regression from a
// co-tenant is not evidence.
//
// The claim the fusion actually makes is structural, not temporal: it does not
// materialise the intermediate two-path rows that the binary-join plan builds and
// then discards. That is measurable as ALLOCATION VOLUME, which is a property of the
// executed plan and is invariant under machine load. Measured on this fixture, the
// arms differ by 2.7× at degree 4 and 27× at degree 32, with a run-to-run spread
// below 0.01 % — three orders of magnitude more headroom than the 3 % that made the
// wall-clock form flaky. So allocations carry the per-point claim, and wall clock is
// kept only as a coarse guard against a real time regression, with a tolerance far
// outside scheduler noise.

import (
	"context"
	"math"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bench/cyclicjoin"
	"github.com/FlavioCFOliveira/GoGraph/bench/expandinto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// timeTolerance is how much slower the fused arm may be at any single point before
// the wall-clock guard fires. It is deliberately loose: the load-bearing per-point
// claim is the allocation one, and this exists only to catch a time regression so
// large that no amount of co-tenancy could explain it.
const timeTolerance = 1.50

// minAllocWin is the factor by which the fused arm must allocate less at every
// point. The measured margin is 2.7×–27×, so this leaves ample room while still
// failing decisively if the fusion stops eliminating intermediate rows.
const minAllocWin = 1.5

// mallocsFor returns the number of heap objects allocated by one drained execution
// of q. Unlike wall clock this is a property of the plan that ran, so a busy machine
// cannot change it.
func mallocsFor(t *testing.T, eng *cypher.Engine, q string) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
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
	runtime.ReadMemStats(&after)
	return after.Mallocs - before.Mallocs
}

// engageCounter counts fused-cyclic-expand engagements, so a recogniser that
// silently declines cannot pass this test by making both arms run the same plan.
type engageCounter struct{ n atomic.Uint64 }

func (p *engageCounter) IncCounter(name string, delta uint64) {
	if name == exec.MetricExpandIntersectEngaged {
		p.n.Add(delta)
	}
}
func (p *engageCounter) ObserveLatency(string, time.Duration) {}

func (p *engageCounter) SetGauge(string, float64) {}

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
	twoAllocs := make([]uint64, 0, len(degrees))
	fusedAllocs := make([]uint64, 0, len(degrees))
	var twoEngaged, fusedEngaged uint64
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

			// Allocations and engagement are measured on a separate, unmetered run so
			// the metrics backend and the MemStats stop-the-world never perturb the
			// timing series above.
			probe := &engageCounter{}
			cmetrics.SetBackend(probe)
			allocs := mallocsFor(t, eng, cyclicjoin.TriangleQuery)
			cmetrics.SetBackend(nil)

			if fused {
				fusedCosts = append(fusedCosts, cost)
				fusedAllocs = append(fusedAllocs, allocs)
				fusedEngaged += probe.n.Load()
			} else {
				twoCosts = append(twoCosts, cost)
				twoAllocs = append(twoAllocs, allocs)
				twoEngaged += probe.n.Load()
			}
		}
	}

	twoExp := expandinto.FitExponent(ms, twoCosts)
	fusedExp := expandinto.FitExponent(ms, fusedCosts)
	t.Logf("regime: DENSE (fixed n=%d, degree %v, so m grows with degree)", nodes, degrees)
	t.Logf("m series: %v", ms)
	t.Logf("two-Expand costs (ns): %.0f", twoCosts)
	t.Logf("fused costs (ns):      %.0f", fusedCosts)
	t.Logf("two-Expand mallocs:     %v", twoAllocs)
	t.Logf("fused mallocs:          %v", fusedAllocs)
	t.Logf("FITTED EXPONENT IN m — two-Expand %.3f | fused %.3f", twoExp, fusedExp)

	// Engagement, first: everything below compares two plans, and that comparison is
	// vacuous if the recogniser declined and both arms ran the same one.
	if fusedEngaged == 0 {
		t.Fatalf("the fused arm never engaged (%s == 0), so both arms ran the same plan and "+
			"every comparison below is vacuous", exec.MetricExpandIntersectEngaged)
	}
	if twoEngaged != 0 {
		t.Fatalf("the two-Expand arm engaged the fused operator %d times; the control is "+
			"contaminated and the comparison means nothing", twoEngaged)
	}

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
	// exponent and is what the default-enable recommendation rests on. The win is
	// asserted in ALLOCATIONS rather than in wall clock — see the header for why.
	for i := range ms {
		if fusedAllocs[i] == 0 || twoAllocs[i] == 0 {
			t.Fatalf("at m=%d an arm allocated nothing (two=%d fused=%d); the measurement is "+
				"degenerate, not a win", ms[i], twoAllocs[i], fusedAllocs[i])
		}
		ratio := float64(twoAllocs[i]) / float64(fusedAllocs[i])
		if ratio < minAllocWin {
			t.Fatalf("at m=%d the fused arm allocated %d objects against two-Expand's %d "+
				"(%.2f×, below the %.2f× floor) — the fusion has stopped eliminating the "+
				"intermediate two-path rows, which is the whole reason it exists",
				ms[i], fusedAllocs[i], twoAllocs[i], ratio, minAllocWin)
		}
	}
	// A coarse wall-clock guard. This is NOT the per-point win claim; it exists only
	// so that a fusion which became dramatically slower while still allocating less
	// cannot pass. The tolerance is far outside the scheduler noise that made the
	// strict form flaky.
	for i := range ms {
		if fusedCosts[i] > twoCosts[i]*timeTolerance {
			t.Fatalf("at m=%d the fused arm took %.0f ns against two-Expand's %.0f ns "+
				"(%.2f×, over the %.2f× tolerance) — too large to attribute to machine load",
				ms[i], fusedCosts[i], twoCosts[i], fusedCosts[i]/twoCosts[i], timeTolerance)
		}
	}
}
