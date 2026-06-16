package sim

import (
	"context"
	"testing"
)

// runCrashSim builds and runs a crash-enabled simulation to completion, closing
// the store afterwards, and returns the simulator for assertions. It fails the
// test on any run error or violation report.
func runCrashSim(t *testing.T, cfg Config) *Simulator {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report != nil {
		t.Fatalf("crash simulation reported violations:\n%s", report)
	}
	return s
}

// TestSimulator_CrashRecoveryCycle is the #1542 acceptance test: a crash+recovery
// cycle runs deterministically end-to-end for a fixed seed, the engine reopens
// from the durable SimDisk image via real recovery, and the loop continues to
// completion with no durability violation. A write-heavy mix and a high crash
// rate guarantee multiple cycles over a populated graph.
func TestSimulator_CrashRecoveryCycle(t *testing.T) {
	s := runCrashSim(t, Config{
		Seed: 0xCAFE,
		// Kept modest so the short layer stays within the per-package budget
		// under -race (the full-scan durability check plus WAL replay on each
		// cycle is the cost driver); a dense crash rate guarantees several
		// cycles in this window. The multi-hundred-thousand-tick crash storm
		// lives in the soak layer (see crashstorm_soak_test.go).
		MaxTicks:   1200,
		CheckEvery: 16,
		Workload:   WriteHeavyWorkload(NewSeed(0xCAFE)),
		Crash: CrashConfig{
			Enabled:   true,
			CrashProb: 1.0 / 60.0, // dense enough for several cycles in 1200 ticks.
		},
	})
	if s.CrashCount() == 0 {
		t.Fatal("expected at least one crash+recovery cycle over 4000 ticks")
	}
	if s.Oracle().NodeCount() == 0 {
		t.Fatal("write-heavy crash run created no surviving nodes")
	}
	if s.ReplayedOps() == 0 {
		t.Fatal("recovery replayed zero WAL ops across all cycles (durable bytes were not exercised)")
	}
}

// TestSimulator_CrashRecoveryReproducible verifies the crash+recovery run is a
// pure function of the seed: two runs reach identical surviving state and an
// identical number of crash cycles. This is the determinism guarantee that lets
// a crash-triggered failure be replayed bit-for-bit.
func TestSimulator_CrashRecoveryReproducible(t *testing.T) {
	mk := func() Config {
		return Config{
			Seed:       0xD15EA5E,
			MaxTicks:   1000,
			CheckEvery: 32,
			Workload:   WriteHeavyWorkload(NewSeed(0xD15EA5E)),
			Crash:      CrashConfig{Enabled: true, CrashProb: 1.0 / 60.0},
		}
	}
	a := runCrashSim(t, mk())
	b := runCrashSim(t, mk())

	if a.CrashCount() != b.CrashCount() {
		t.Fatalf("crash count not reproducible: %d vs %d", a.CrashCount(), b.CrashCount())
	}
	if a.Oracle().NodeCount() != b.Oracle().NodeCount() {
		t.Fatalf("surviving node count not reproducible: %d vs %d", a.Oracle().NodeCount(), b.Oracle().NodeCount())
	}
	if a.Oracle().EdgeCount() != b.Oracle().EdgeCount() {
		t.Fatalf("surviving edge count not reproducible: %d vs %d", a.Oracle().EdgeCount(), b.Oracle().EdgeCount())
	}
	if a.ReplayedOps() != b.ReplayedOps() {
		t.Fatalf("replayed WAL ops not reproducible: %d vs %d", a.ReplayedOps(), b.ReplayedOps())
	}
}

// TestSimulator_CrashDisabledNoStore verifies the safe default: with crashes
// disabled the simulator drives a plain in-memory engine, builds no SimStore,
// and Close is a no-op — confirming the no-crash path is unchanged.
func TestSimulator_CrashDisabledNoStore(t *testing.T) {
	s, err := New(Config{Seed: 1, MaxTicks: 10, Workload: DefaultWorkload(NewSeed(1))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.store != nil {
		t.Fatal("disabled-crash simulator must not build a SimStore")
	}
	if s.crash.Enabled() {
		t.Fatal("disabled-crash simulator must have an inert schedule")
	}
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.CrashCount() != 0 {
		t.Fatalf("disabled-crash run performed %d crashes", s.CrashCount())
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close (no store): %v", err)
	}
}
