//go:build soakfull

package sim

// The two endurance scenarios below drive millions of ticks (2,000,000 and
// 1,000,000) and run for many minutes-to-hours under the race detector — long
// enough to blow the 50m go-test timeout / 90m job budget of the scheduled
// nightly-ci runner. They are therefore gated to the heaviest soak tier,
// `soakfull` (part of the soak family), which the full `make test-nightly`
// target passes but the CI-safe `make test-nightly-ci` subset does not. This
// loses no scenario coverage: the ScenarioLongRunning run-path is exercised on
// every PR by the short-layer TestCatalogue_SmokeSubsetRunsClean and at a
// 2,000-tick budget by the soak-layer TestCatalogue_EachScenarioRunsClean — the
// endurance budget here is a periodic heap/goroutine-stability watch, which the
// project's CLAUDE.md classifies as a periodic reliability exercise, not a CI
// release gate.

import (
	"context"
	"runtime"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestScenario_LongRunningSoak is the soak-layer long-running scenario: it
// drives the bounded-churn steady-state workload for a large number of small
// ops and asserts (a) it upholds every invariant (clean report), (b) it leaks no
// goroutine (goleak in teardown), and (c) the working set stays bounded near
// churnHighWater rather than growing without bound — the heap/goroutine-stability
// watch the scenario exists for.
//
// It is gated to the soak layer because the full op budget is millions of ops
// and so is minutes-long; the short layer exercises the same scenario at a small
// budget (the catalogue table test).
func TestScenario_LongRunningSoak(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)

	sc := longRunningScenario()
	sc.MaxTicks = 2_000_000

	baselineGoroutines := runtime.NumGoroutine()

	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("long-running soak run error (seed %d): %v", sc.DefaultSeed, err)
	}
	if report != nil {
		t.Fatalf("long-running soak reported violations (seed %d):\n%s", sc.DefaultSeed, report)
	}

	// Goroutine stability, as a COARSE cross-check on top of goleak.
	//
	// goleak.VerifyNone above is the instrument that actually certifies "leaks no
	// goroutine": it identifies a leak by its STACK and ignores runtime-owned
	// goroutines. This delta is a second, cruder look at the same property, kept
	// because it catches a leak that has already exited by teardown, which goleak
	// by construction cannot.
	//
	// It carries a named slack rather than demanding exact zero (rmp #2592).
	// runtime.NumGoroutine() counts goroutines the RUNTIME owns as well as the
	// harness's — GC workers, the finalizer, timer and netpoll helpers — so exact
	// zero asserts something this sample cannot see the inputs to. The acceptance
	// criteria for #2592 offered exact zero only "if the sample provably counts
	// only harness-owned goroutines", and it provably does not.
	//
	// EIGHT, and why the value is not copied blindly from the swarm sites. In
	// internal/sim goroutineSlack is a PARAMETER, not a constant, and its callers
	// choose 0 for a single deterministic scenario (metrics_oracle.go:274) and 2,
	// 4 or 8 for a swarm, scaling with worker count: more concurrent workers, more
	// runtime helpers parked. This run is single-goroutine, so concurrency scaling
	// does not apply — but it is the LONGEST run in the family at 2,000,000 ticks
	// and minutes of wall time, which is the other thing that gives the runtime
	// opportunity to park a helper. bolt/server's connection-churn soak, the only
	// other minutes-long site, independently settled on 8 for the same reason.
	// The slack applies to THIS run length; a shorter run should not inherit it.
	const goroutineSlack = 8
	if grown := runtime.NumGoroutine() - baselineGoroutines; grown > goroutineSlack {
		t.Fatalf("goroutine count grew by %d over the soak run, beyond the slack of %d "+
			"(baseline %d). goleak in teardown is the precise instrument; a delta this far "+
			"past the runtime's own bookkeeping is a leak it may also report",
			grown, goroutineSlack, baselineGoroutines)
	}
}

// TestScenario_LongRunningWorkingSetBounded asserts, under the soak layer, that
// the steady-state workload holds the modelled node count near churnHighWater
// across the whole run rather than growing linearly with the op count — the
// property that makes a millions-of-ops run a stability watch rather than an
// O(n^2) blow-up.
func TestScenario_LongRunningWorkingSetBounded(t *testing.T) {
	testlayers.RequireSoak(t)

	const ticks = 1_000_000
	seed := uint64(0x10067)
	sm, err := New(Config{
		Seed:       seed,
		MaxTicks:   ticks,
		CheckEvery: 1000,
		Workload:   SteadyStateWorkload(NewSeed(seed)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = sm.Close() }()

	report, err := sm.Run(context.Background())
	if err != nil {
		t.Fatalf("run error: %v", err)
	}
	if report != nil {
		t.Fatalf("reported violations:\n%s", report)
	}
	// After millions of ops the working set must remain a small multiple of the
	// high-water mark, not proportional to the op count.
	if n := sm.Oracle().NodeCount(); n > churnHighWater*2 {
		t.Fatalf("working set not bounded: %d nodes after %d ticks (high-water %d)", n, ticks, churnHighWater)
	}
	t.Logf("seed %d: %d ticks, final working set = %d nodes (bounded near high-water %d)",
		seed, ticks, sm.Oracle().NodeCount(), churnHighWater)
}
