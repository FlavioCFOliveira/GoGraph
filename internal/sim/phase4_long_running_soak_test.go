//go:build soak

package sim

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
