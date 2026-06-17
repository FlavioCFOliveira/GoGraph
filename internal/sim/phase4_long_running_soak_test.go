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

	// Goroutine stability: the deterministic engine-API loop spawns none, so the
	// count must not have grown.
	if grown := runtime.NumGoroutine() - baselineGoroutines; grown > 0 {
		t.Fatalf("goroutine count grew by %d over the soak run (baseline %d)", grown, baselineGoroutines)
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
