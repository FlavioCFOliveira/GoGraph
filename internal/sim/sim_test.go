package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// runToCompletion runs a simulation with the given config and fails the test on
// any error or violation, returning the simulator for state assertions.
func runToCompletion(t *testing.T, cfg Config) *Simulator {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report != nil {
		t.Fatalf("simulation reported violations:\n%s", report)
	}
	return s
}

// TestSimulator_Reproducibility verifies that the same seed produces identical
// oracle state and zero violations across two independent runs — the core
// guarantee of the harness. The check cadence is irrelevant to reproducibility
// (the checker draws from its own derived sub-seed, so it never perturbs the
// workload stream); a modest CheckEvery keeps the run within the short-layer
// budget under -race while still validating invariants throughout.
func TestSimulator_Reproducibility(t *testing.T) {
	mk := func() Config {
		return Config{Seed: 42, MaxTicks: 2500, CheckEvery: 16, Workload: DefaultWorkload(NewSeed(42))}
	}
	a := runToCompletion(t, mk())
	b := runToCompletion(t, mk())

	if a.Oracle().NodeCount() != b.Oracle().NodeCount() {
		t.Fatalf("node count not reproducible: %d vs %d", a.Oracle().NodeCount(), b.Oracle().NodeCount())
	}
	if a.Oracle().EdgeCount() != b.Oracle().EdgeCount() {
		t.Fatalf("edge count not reproducible: %d vs %d", a.Oracle().EdgeCount(), b.Oracle().EdgeCount())
	}
	if a.checker.HasViolations() || b.checker.HasViolations() {
		t.Fatalf("unexpected violations: a=%v b=%v", a.checker.Violations(), b.checker.Violations())
	}
}

// TestSimulator_Workloads runs each workload mix and asserts a clean result; the
// write-heavy and default mixes must also create nodes.
func TestSimulator_Workloads(t *testing.T) {
	// CheckEvery is kept modest (not 1) on the longer runs so the whole short
	// layer stays well within the per-package 60s budget under -race; invariant
	// coverage is unaffected because any divergence persists until the next
	// check.
	tests := []struct {
		name        string
		workload    func(*Seed) *Workload
		ticks       int
		checkEvery  int
		wantNonzero bool
	}{
		{"write-heavy", WriteHeavyWorkload, 1000, 8, true},
		{"read-heavy", ReadHeavyWorkload, 1000, 8, false},
		{"default", DefaultWorkload, 3000, 32, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := runToCompletion(t, Config{
				Seed:       7,
				MaxTicks:   tc.ticks,
				CheckEvery: tc.checkEvery,
				Workload:   tc.workload(NewSeed(7)),
			})
			if tc.wantNonzero && s.Oracle().NodeCount() == 0 {
				t.Fatalf("%s: expected nodes to be created, got 0", tc.name)
			}
		})
	}
}

// TestSimulator_OracleTracksCreates verifies the write-heavy workload populates
// the oracle and stays violation-free under per-tick checking (CheckEvery 1),
// exercising the check-every-operation path.
func TestSimulator_OracleTracksCreates(t *testing.T) {
	s := runToCompletion(t, Config{Seed: 123, MaxTicks: 800, CheckEvery: 1, Workload: WriteHeavyWorkload(NewSeed(123))})
	if s.Oracle().NodeCount() == 0 {
		t.Fatal("write-heavy workload created no nodes")
	}
}

// TestSimulator_ContextCancellation verifies Run honours a cancelled context.
func TestSimulator_ContextCancellation(t *testing.T) {
	s, err := New(Config{Seed: 1, MaxTicks: 1_000_000})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := s.Run(ctx)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if report != nil {
		t.Fatalf("expected nil report on cancellation, got %s", report)
	}
}

// TestSimulator_Soak runs several seeds at high tick counts. It is gated to the
// soak layer because each run is minutes-long.
func TestSimulator_Soak(t *testing.T) {
	testlayers.RequireSoak(t)

	const (
		masterSeed = 0xA11CE
		runs       = 5
		ticks      = 500_000
	)
	// Derive distinct child seeds from the master via the FNV prime, so the five
	// runs explore different op streams while staying a pure function of the
	// master seed.
	for i := 0; i < runs; i++ {
		seed := uint64(masterSeed) ^ uint64(i+1)*0x100000001b3
		t.Run("seed", func(t *testing.T) {
			runToCompletion(t, Config{
				Seed:     seed,
				MaxTicks: ticks,
				// Check less often so the multi-hundred-thousand-tick run stays
				// within the soak budget; correctness still holds because a
				// divergence persists until the next check.
				CheckEvery: 256,
				Workload:   DefaultWorkload(NewSeed(seed)),
			})
		})
	}
}
