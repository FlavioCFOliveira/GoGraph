package sim

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestSwarm_Smoke runs a small bounded swarm over a fast deterministic scenario
// and asserts the aggregate is well-formed: every run accounted for, passes +
// failures == runs, and the worker cap respected.
func TestSwarm_Smoke(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}

	const runs = 24
	sw, err := NewSwarm(reg, SwarmConfig{
		MasterSeed: 0x5EED,
		Scenario:   ScenarioReadHeavy,
		Workers:    4,
		Runs:       runs,
	})
	if err != nil {
		t.Fatalf("NewSwarm: %v", err)
	}

	res, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Swarm.Run: %v", err)
	}
	if res.Runs != runs {
		t.Errorf("Runs = %d, want %d", res.Runs, runs)
	}
	if res.Passes+res.FailureCount() != res.Runs {
		t.Errorf("passes(%d)+failures(%d) != runs(%d)", res.Passes, res.FailureCount(), res.Runs)
	}
	if res.Workers != 4 {
		t.Errorf("Workers = %d, want 4", res.Workers)
	}
	// The read-heavy scenario is correct, so a clean swarm has zero failures.
	if res.FailureCount() != 0 {
		t.Errorf("unexpected failures in a correct scenario:\n%s", res.Summary())
	}
}

// TestSwarm_Reproducible asserts the derived seed schedule is a pure function of
// the master seed: two swarms with the same config execute the identical set of
// (index, seed) pairs regardless of worker scheduling.
func TestSwarm_Reproducible(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}

	collect := func(workers int) map[int]uint64 {
		seen := make(map[int]uint64)
		sw, err := NewSwarm(reg, SwarmConfig{
			MasterSeed: 0xABCDEF,
			Scenario:   ScenarioReadHeavy,
			Workers:    workers,
			Runs:       16,
			Observe:    func(r SwarmRun) { seen[r.Index] = r.Seed },
		})
		if err != nil {
			t.Fatalf("NewSwarm: %v", err)
		}
		if _, err := sw.Run(context.Background()); err != nil {
			t.Fatalf("Swarm.Run: %v", err)
		}
		return seen
	}

	a := collect(1)
	b := collect(8)
	if len(a) != 16 {
		t.Fatalf("run a recorded %d runs, want 16", len(a))
	}
	for idx, seedA := range a {
		if seedB, ok := b[idx]; !ok || seedB != seedA {
			t.Errorf("run %d: seed differs across worker counts: a=%d b=%d (ok=%v)", idx, seedA, seedB, ok)
		}
	}
	// Distinct indices must yield distinct seeds (no trivial collisions).
	inv := make(map[uint64]int, len(a))
	for idx, s := range a {
		if prev, dup := inv[s]; dup {
			t.Errorf("seed collision: runs %d and %d both derived seed %d", prev, idx, s)
		}
		inv[s] = idx
	}
}

// TestSwarm_DurationBudget runs a duration-only swarm under a virtual clock and
// asserts it terminates cleanly (the budget bounds scheduling; in-flight runs
// finish). It proves a swarm need not carry a run-count budget.
func TestSwarm_DurationBudget(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sw, err := NewSwarm(reg, SwarmConfig{
		MasterSeed: 1,
		Scenario:   ScenarioReadHeavy,
		Workers:    2,
		Duration:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSwarm: %v", err)
	}
	res, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Swarm.Run: %v", err)
	}
	if res.Runs == 0 {
		t.Errorf("duration-bounded swarm executed 0 runs")
	}
	if res.Passes+res.FailureCount() != res.Runs {
		t.Errorf("accounting mismatch: passes=%d failures=%d runs=%d", res.Passes, res.FailureCount(), res.Runs)
	}
}

// TestSwarm_ConfigValidation covers the constructor's fast-fail guards.
func TestSwarm_ConfigValidation(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	tests := []struct {
		name    string
		reg     *Registry
		cfg     SwarmConfig
		wantErr bool
	}{
		{"nil registry", nil, SwarmConfig{Scenario: ScenarioReadHeavy, Runs: 1}, true},
		{"no budget", reg, SwarmConfig{Scenario: ScenarioReadHeavy}, true},
		{"unknown scenario", reg, SwarmConfig{Scenario: "no-such", Runs: 1}, true},
		{"ok runs", reg, SwarmConfig{Scenario: ScenarioReadHeavy, Runs: 1}, false},
		{"ok duration", reg, SwarmConfig{Scenario: ScenarioReadHeavy, Duration: time.Millisecond}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSwarm(tt.reg, tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewSwarm err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
