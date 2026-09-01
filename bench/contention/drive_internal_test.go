package contention

import (
	"fmt"
	"testing"
)

// TestSplitWorkDeliversEveryDeclaredOperation pins the promise the package
// documentation makes: every level performs the SAME total work.
//
// It fails on the old arithmetic. `total := (ops/level) * level` discarded up
// to level-1 operations without saying so — 1024 of cypher-read-scan-large's
// 2000 operations at level 1024, a 48.8% loss — so the scaling column compared
// runs that had not done the same amount of work.
func TestSplitWorkDeliversEveryDeclaredOperation(t *testing.T) {
	for _, w := range All() {
		for _, level := range Levels() {
			t.Run(fmt.Sprintf("%s@%d", w.Name, level), func(t *testing.T) {
				split := splitWork(w.Ops, level)
				total := 0
				for g := range level {
					total += split.count(g)
				}
				if total != w.Ops {
					t.Errorf("level %d runs %d of the %d declared operations (%.1f%% dropped)",
						level, total, w.Ops, 100*float64(w.Ops-total)/float64(w.Ops))
				}
			})
		}
	}
}

// TestSampleBufferIsPreSizedForEveryWorker checks that no worker's latency
// buffer can outgrow the capacity drive reserves for it. A realloc mid-run
// would allocate on the measured path, in the middle of the window.
func TestSampleBufferIsPreSizedForEveryWorker(t *testing.T) {
	for _, w := range All() {
		for _, level := range Levels() {
			split := splitWork(w.Ops, level)
			stride := sampleStride(w.Ops)
			reserved := split.max()/stride + 1
			for g := range level {
				count, phase, n := split.count(g), samplePhase(g, stride), 0
				for i := range count {
					if i%stride == phase {
						n++
					}
				}
				if n > reserved {
					t.Fatalf("%s@%d worker %d takes %d samples, capacity reserved is %d",
						w.Name, level, g, n, reserved)
				}
			}
		}
	}
}

// TestSamplePhaseIsNotSharedByEveryWorker pins the sampling fix.
//
// Every worker used to sample on phase 0, so every worker timed its own FIRST
// operation — the instant right after the release barrier, carrying the
// stampede and whatever the workload initialises lazily. At level 1024 that
// drew 20% of all latency samples from the start-up instant, and the p99, being
// the top 1%, was then drawn entirely from inside it. With a per-worker phase
// the number of workers that sample iteration 0 falls to about level/stride,
// which is iteration 0's honest share of the population.
func TestSamplePhaseIsNotSharedByEveryWorker(t *testing.T) {
	exercised := 0
	for _, w := range All() {
		for _, level := range Levels() {
			stride := sampleStride(w.Ops)
			if stride < 2 || level < 2 {
				// Stride 1 censuses every operation, so iteration 0 is sampled
				// by construction and there is no phase to spread.
				continue
			}
			exercised++
			split := splitWork(w.Ops, level)
			firstOpSamplers := 0
			for g := range level {
				if split.count(g) > 0 && samplePhase(g, stride) == 0 {
					firstOpSamplers++
				}
			}
			if want := level/stride + 1; firstOpSamplers > want {
				t.Errorf("%s@%d: %d of %d workers sample iteration 0 (stride %d), want at most %d",
					w.Name, level, firstOpSamplers, level, stride, want)
			}
		}
	}
	// Non-vacuity: the loop above skips stride-1 configurations, and if it
	// skipped all of them it would have asserted nothing at all.
	if exercised == 0 {
		t.Fatal("no workload/level pair had a stride above 1; the assertion never ran")
	}
}

// TestMixedWriteFractionIsLevelInvariant pins the mix fix.
//
// cypher-mixed-rw used to decide read-versus-write from the worker index alone,
// so at level 1 — one worker, worker 0 — every operation was a write, while
// every other rung ran a quarter writes. The level-1 baseline was therefore a
// different, much slower experiment, and scaling_vs_1 measured the mix changing
// rather than the module scaling.
func TestMixedWriteFractionIsLevelInvariant(t *testing.T) {
	w, ok := ByName("cypher-mixed-rw")
	if !ok {
		t.Fatal("cypher-mixed-rw is not registered")
	}
	const wantFraction = 1.0 / mixedWriteEvery
	const tolerance = 0.01

	for _, level := range Levels() {
		split := splitWork(w.Ops, level)
		writes, total := 0, 0
		for g := range level {
			for i := range split.count(g) {
				if mixedIsWrite(g, i) {
					writes++
				}
				total++
			}
		}
		if total == 0 {
			t.Fatalf("level %d issued no operations", level)
		}
		got := float64(writes) / float64(total)
		t.Logf("level %-4d writes %d/%d = %.4f", level, writes, total, got)
		if diff := got - wantFraction; diff > tolerance || diff < -tolerance {
			t.Errorf("level %d write fraction %.4f, want %.4f +/- %.2f",
				level, got, wantFraction, tolerance)
		}
	}
}
