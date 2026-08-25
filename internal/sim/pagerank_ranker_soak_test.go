//go:build soak

package sim

// pagerank_ranker_soak_test.go — the soak-layer arms of the stateful-PageRanker
// scenario (rmp #2495).
//
// Four claims live here rather than in the short layer, each for its own reason:
//
//   - The SEED SWEEP. Every non-vacuity gate is a claim about what a run reaches,
//     and a claim verified on one seed is a claim about one seed. The fixture's
//     size, its extra arcs and four of its six dampings are drawn, so which
//     windows converge, how many iterations each takes, and whether the derived
//     partition collapses a boundary are all seed-dependent.
//   - The DETERMINISM SWEEP, separate from the sweep above because the two claims
//     are different: that one asserts the invariants hold, this one asserts the
//     harness is a pure function of the seed. A digest can be stable for one seed
//     and not for the draw sequence another seed takes.
//   - The WIDE CLAMP SWEEP. The short layer visits three worker counts (1, 4, 8).
//     The parallel path's partition — and therefore the order in which its
//     per-worker partial L1 deltas are reduced — is a function of the worker
//     count, and the file header records a MEASURED difference in the last bits of
//     that reduction across counts. This arm is the widest search for a seed and a
//     worker count where that difference finally straddles the convergence
//     threshold and changes the answer, which would refute a godoc claim rather
//     than this clause.
//   - The LONG SEQUENCE. The short plan reuses one PageRanker six times. This arm
//     reuses one 30 times, alternating regimes throughout, because "the cached
//     state never drifts" is a claim about a sequence and six is a short one.
//
// None of these tests calls t.Parallel(), for the reason the short-layer file
// gives: the scenario clamps the process-global GOMAXPROCS.

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/search/centrality"
)

// The soak budgets. Each run is a few milliseconds of arithmetic, so the sweeps
// are sized by how much seed space they cover rather than by wall clock.
const (
	pageRankerSoakSeeds     = 32
	pageRankerSoakLongRuns  = 30
	pageRankerSoakWideSeeds = 8
)

// pageRankerSoakWideClamps is the wide clamp cycle: every worker count from 1 to
// 8, so eight different partitions of the same fixture are driven through one
// reused PageRanker.
var pageRankerSoakWideClamps = []int{1, 2, 3, 4, 5, 6, 7, 8}

// pageRankerSoakWideCrossWorkers widens the cross-regime sweep to every worker
// count from 2 to 12, including counts above the reference host's core count, so
// the partition is exercised beyond the shapes a well-provisioned host produces.
var pageRankerSoakWideCrossWorkers = []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

// pageRankerSoakPlan builds a plan of n windows that alternates regimes on the
// supplied clamp cycle, gives every window its own damping (so the aliasing pin
// is armed at every transition), starts SERIAL (so the lazy transpose is built
// mid-sequence), caps exactly one window's iterations (so both power-iteration
// exits are reached) and ENDS on a converging window (which
// `gate:reference-converged` requires).
func pageRankerSoakPlan(n int, clamps []int) []prWindow {
	plan := make([]prWindow, 0, n)
	for i := 0; i < n; i++ {
		clamp := 1
		if i > 0 {
			clamp = clamps[(i-1)%len(clamps)]
		}
		opts := centrality.PageRankOptions{
			// A damping unique to the window, spread across the band.
			Damping:       pageRankerDampingLow + (pageRankerDampingHigh-pageRankerDampingLow)*float64(i%17)/17.0,
			MaxIterations: pageRankerMaxIters,
			Tolerance:     pageRankerTolerance,
		}
		// Exactly one capped window, and never the last.
		if i == n/2 {
			opts.MaxIterations = pageRankerCapIters
			opts.Tolerance = pageRankerCapTol
		}
		if i == n-1 {
			opts.Damping = pageRankerDampingLow
		}
		plan = append(plan, prWindow{Clamp: clamp, Opts: opts})
	}
	return plan
}

// TestPageRankRanker_SoakSeedSweep drives the scenario across many derived seeds
// and requires every run to be clean AND non-vacuous.
//
// A violation here names the seed, and because the scenario is deterministic the
// seed replays the failure exactly: `go run ./cmd/sim -scenario=pagerank-ranker
// <seed>`.
func TestPageRankRanker_SoakSeedSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < pageRankerSoakSeeds; i++ {
		seed := pageRankRankerDefaultSeed ^ (uint64(i+1) * 0x9E37_79B9_7F4A_7C15)
		ev, report, err := RunPageRankRanker(context.Background(), DefaultPageRankRankerConfig(seed))
		if err != nil {
			t.Fatalf("seed %#x: run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %#x reported a violation:\n%s", seed, report)
		}
		// The counters whose preconditions depend on a draw, asserted per seed. A
		// regression in the fixture generator or the plan shows up here rather than
		// only on whichever seed happens to expose it.
		if ev.SerialWindows == 0 || ev.ParallelWindows == 0 || ev.FirstParallel <= 0 {
			t.Fatalf("seed %#x: the interleaving degenerated: serial=%d parallel=%d first-parallel=%d",
				seed, ev.SerialWindows, ev.ParallelWindows, ev.FirstParallel)
		}
		// At most two backing arrays, with at least one repeat. Not exactly two:
		// which buffer a Run returns is the parity of its iteration count, and seed
		// 0x8d10afeecdf8dcf gave all six windows an even count and therefore one
		// array six times — which is why this is the shape asserted rather than an
		// equality.
		if ev.AliasArmed == 0 || ev.DistinctBuffers < 1 || ev.DistinctBuffers > 2 || ev.BufferRepeats == 0 {
			t.Fatalf("seed %#x: the aliasing arm degenerated: armed=%d buffers=%d repeats=%d",
				seed, ev.AliasArmed, ev.DistinctBuffers, ev.BufferRepeats)
		}
		if ev.MaxEmptyRanges == 0 {
			t.Fatalf("seed %#x: the derived partition never collapsed a boundary (hub holds %d of %d "+
				"in-edges)", seed, ev.HubInDeg, ev.Edges)
		}
		if ev.FirstParallelAlloc < ev.TransposeFloor {
			t.Fatalf("seed %#x: the first parallel window allocated %dB, below the %dB transpose floor",
				seed, ev.FirstParallelAlloc, ev.TransposeFloor)
		}
	}
}

// TestPageRankRanker_SoakDeterminismSweep re-runs each seed and requires the
// reproducible evidence to match.
func TestPageRankRanker_SoakDeterminismSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < pageRankerSoakSeeds; i++ {
		seed := pageRankRankerDefaultSeed ^ (uint64(i+1) * 0x51ED_2701_C0FF_EE01)
		cfg := DefaultPageRankRankerConfig(seed)
		first, report, err := RunPageRankRanker(context.Background(), cfg)
		if err != nil || report != nil {
			t.Fatalf("seed %#x: first run err=%v report=%v", seed, err, report)
		}
		second, report, err := RunPageRankRanker(context.Background(), cfg)
		if err != nil || report != nil {
			t.Fatalf("seed %#x: second run err=%v report=%v", seed, err, report)
		}
		if first.ReproducibleSummary() != second.ReproducibleSummary() {
			t.Fatalf("seed %#x is NOT reproducible:\nfirst:  %s\nsecond: %s",
				seed, first.ReproducibleSummary(), second.ReproducibleSummary())
		}
	}
}

// TestPageRankRanker_SoakWideClampSweep drives one reused PageRanker across every
// worker count from 1 to 8 and widens the cross-regime arm to 2..12, on several
// seeds.
//
// It is the widest search this task builds for the one thing that could refute
// the parallelism godoc: a partition whose L1-delta reduction differs enough in
// its last bits to change the iteration count and therefore the answer. Every
// combination MEASURED so far agrees bit for bit, and this arm exists so a
// counterexample is found by the harness rather than by a user.
func TestPageRankRanker_SoakWideClampSweep(t *testing.T) {
	defer goleak.VerifyNone(t)

	for i := 0; i < pageRankerSoakWideSeeds; i++ {
		seed := pageRankRankerDefaultSeed ^ (uint64(i+1) * 0xD1B5_4A32_D192_ED03)
		cfg := DefaultPageRankRankerConfig(seed)
		cfg.Plan = pageRankerSoakPlan(len(pageRankerSoakWideClamps)+2, pageRankerSoakWideClamps)
		cfg.CrossRegimeWorkers = pageRankerSoakWideCrossWorkers

		ev, report, err := RunPageRankRanker(context.Background(), cfg)
		if err != nil {
			t.Fatalf("seed %#x: run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %#x reported a violation:\n%s", seed, report)
		}
		if len(ev.CrossRegime) != len(pageRankerSoakWideCrossWorkers) {
			t.Fatalf("seed %#x: cross-regime arm compared %d worker count(s), want %d",
				seed, len(ev.CrossRegime), len(pageRankerSoakWideCrossWorkers))
		}
		// Every worker count must have produced a real worker pool, or the sweep is
		// wider only on paper.
		for w := range ev.Windows {
			win := &ev.Windows[w]
			if win.ExpectParallel && win.LabelLookups < int64(win.ExpectWorkers) {
				t.Fatalf("seed %#x window %d (clamp %d): observed %d worker-spawn lookup(s), want at "+
					"least %d", seed, w, win.Clamp, win.LabelLookups, win.ExpectWorkers)
			}
		}
		t.Logf("seed %#x: %s", seed, ev.ReproducibleSummary())
	}
}

// TestPageRankRanker_SoakLongSequence reuses ONE PageRanker for thirty Runs,
// alternating regimes throughout.
//
// The short plan proves the cached state survives five transitions. This proves it
// survives thirty, which is the claim "reusing cached state changes only the
// allocation profile, never the output" at a length where a slow drift — a stale
// buffer read once every few runs, a transpose that ages — would show.
func TestPageRankRanker_SoakLongSequence(t *testing.T) {
	defer goleak.VerifyNone(t)

	cfg := DefaultPageRankRankerConfig(pageRankRankerDefaultSeed)
	cfg.Plan = pageRankerSoakPlan(pageRankerSoakLongRuns, pageRankerSoakWideClamps)

	ev, report, err := RunPageRankRanker(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("reported a violation:\n%s", report)
	}
	if len(ev.Windows) != pageRankerSoakLongRuns {
		t.Fatalf("drove %d window(s), want %d", len(ev.Windows), pageRankerSoakLongRuns)
	}
	if ev.DistinctBuffers < 1 || ev.DistinctBuffers > 2 {
		t.Fatalf("%d Run(s) returned %d distinct backing arrays; a PageRanker owns exactly two and "+
			"Run's result must alias one of them", len(ev.Windows), ev.DistinctBuffers)
	}
	if ev.AliasArmed != len(ev.Windows)-1 {
		t.Fatalf("the aliasing pin was armed at %d of %d transitions; every window has its own "+
			"damping, so every transition should differ", ev.AliasArmed, len(ev.Windows)-1)
	}
	t.Logf("long sequence: %s", ev.String())
}
