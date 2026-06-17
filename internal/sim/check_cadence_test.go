package sim

import (
	"context"
	"testing"
)

// TestSimulator_CheckCadence verifies task #1576: the invariant-check cadence
// (Config.CheckEvery) is honoured exactly, the terminal tick is always checked
// even when the cadence would skip it, and changing the cadence never perturbs
// the deterministic workload (identical oracle state across cadences).
func TestSimulator_CheckCadence(t *testing.T) {
	const (
		seed     = uint64(20260617)
		maxTicks = 300
	)
	run := func(checkEvery int) *Simulator {
		t.Helper()
		s, err := New(Config{
			Seed:       seed,
			MaxTicks:   maxTicks,
			CheckEvery: checkEvery,
			Workload:   DefaultWorkload(NewSeed(seed)),
		})
		if err != nil {
			t.Fatalf("New(checkEvery=%d): %v", checkEvery, err)
		}
		report, err := s.Run(context.Background())
		if err != nil {
			t.Fatalf("Run(checkEvery=%d): %v", checkEvery, err)
		}
		if report != nil {
			t.Fatalf("checkEvery=%d reported violations:\n%s", checkEvery, report)
		}
		return s
	}

	// every == 1: a check on every one of the maxTicks ticks; the terminal tick
	// coincides with an in-loop check, so finalCheck is a no-op (no extra check).
	every1 := run(1)
	if got := every1.checker.ChecksRun(); got != maxTicks {
		t.Fatalf("CheckEvery=1: expected %d checks, got %d", maxTicks, got)
	}

	// every == 7: in-loop checks at ticks 7,14,…,294 (maxTicks/7 = 42), and the
	// terminal tick 300 (300%7 != 0) adds exactly one final check → 43.
	const every = 7
	every7 := run(every)
	wantEvery7 := maxTicks/every + 1
	if got := every7.checker.ChecksRun(); got != wantEvery7 {
		t.Fatalf("CheckEvery=%d: expected %d checks (%d in-loop + 1 terminal), got %d",
			every, wantEvery7, maxTicks/every, got)
	}

	// every > maxTicks: no in-loop check ever fires, so the terminal check is the
	// ONLY check that runs — proving a CheckEvery>1 run can never finish without
	// verifying its terminal state (the #1576 safety guarantee).
	huge := run(maxTicks + 1)
	if got := huge.checker.ChecksRun(); got != 1 {
		t.Fatalf("CheckEvery>maxTicks: expected exactly 1 terminal check, got %d", got)
	}

	// Cadence must not perturb the deterministic workload: every run drives the
	// identical op stream, so the oracle state is identical regardless of when
	// (or how often) the checker observed it.
	n1, e1 := every1.Oracle().NodeCount(), every1.Oracle().EdgeCount()
	for _, s := range []*Simulator{every7, huge} {
		if s.Oracle().NodeCount() != n1 || s.Oracle().EdgeCount() != e1 {
			t.Fatalf("cadence perturbed workload: got (%d nodes, %d edges), want (%d, %d)",
				s.Oracle().NodeCount(), s.Oracle().EdgeCount(), n1, e1)
		}
	}
}
