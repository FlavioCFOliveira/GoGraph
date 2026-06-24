package sim

import (
	"context"
	"testing"
)

// TestDiskFull_Scenario_Passes runs the registered disk-full scenario and
// asserts the engine honours its ACID contract under exhaustion: no ACID
// violation is reported, while the run genuinely exercised ENOSPC (some honest
// writes were rejected) and survived crash+recovery cycles. The oracle advances
// only on a committed write, so a clean pass means every ENOSPC commit failed
// atomically and every acknowledged commit survived recovery.
func TestDiskFull_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioDiskFull)
	if !ok {
		t.Fatalf("disk-full scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("disk-full run: %v", err)
	}
	if report != nil {
		t.Fatalf("disk-full reported a violation (ACID breach under ENOSPC):\n%s", report)
	}
}

// runDiskFull is a helper that drives the disk-full configuration directly so a
// test can inspect the simulator's counters (the Scenario.Run path discards
// them). It returns the report (nil == clean) and the simulator for counter
// inspection.
func runDiskFull(t *testing.T, enospcOnSync bool) (*SimReport, *Simulator) {
	t.Helper()
	cfg := Config{
		Seed:     0xD15CF011,
		MaxTicks: 500,
		Workload: WriteHeavyWorkload(NewSeed(0xD15CF011)),
		Disk:     DiskConfig{CapacityBytes: diskFullCapacityBytes, ENOSPCOnSync: enospcOnSync},
		Crash:    CrashConfig{Enabled: true, CrashProb: 1.0 / 80.0, StabilityWindow: 25},
	}
	sm, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	report, err := sm.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report, sm
}

// TestDiskFull_Delayed_NonVacuous asserts the delayed-allocation (Sync-time)
// ENOSPC path is genuinely exercised: writes are rejected, crashes happen, and
// the run is still clean. A vacuous run (zero rejected writes) would mean the
// budget never bit and the test proves nothing.
func TestDiskFull_Delayed_NonVacuous(t *testing.T) {
	report, sm := runDiskFull(t, true)
	if report != nil {
		t.Fatalf("delayed disk-full reported a violation:\n%s", report)
	}
	if sm.RejectedWrites() == 0 {
		t.Fatalf("vacuous run: ENOSPC never fired (0 rejected writes); lower diskFullCapacityBytes")
	}
	if sm.CrashCount() == 0 {
		t.Fatalf("expected crash+recovery cycles to exercise post-ENOSPC recovery")
	}
	t.Logf("delayed disk-full: rejectedWrites=%d crashes=%d replayedOps=%d oracleNodes=%d",
		sm.RejectedWrites(), sm.CrashCount(), sm.ReplayedOps(), sm.Oracle().NodeCount())
}

// TestDiskFull_Eager_NonVacuous asserts the eager (Write-time) ENOSPC path is
// also exercised cleanly. The two allocation models fail the commit at
// different points (Write vs Sync); both must leave engine and oracle in
// lock-step with no acknowledged commit lost.
func TestDiskFull_Eager_NonVacuous(t *testing.T) {
	report, sm := runDiskFull(t, false)
	if report != nil {
		t.Fatalf("eager disk-full reported a violation:\n%s", report)
	}
	if sm.RejectedWrites() == 0 {
		t.Fatalf("vacuous run: eager ENOSPC never fired (0 rejected writes)")
	}
	t.Logf("eager disk-full: rejectedWrites=%d crashes=%d oracleNodes=%d",
		sm.RejectedWrites(), sm.CrashCount(), sm.Oracle().NodeCount())
}

// TestDiskFull_Reproducible asserts the disk-full run is a pure function of the
// seed: two runs of the identical config reach the same verdict and the same
// rejected-write count, proving the ENOSPC budget check perturbs nothing in the
// reproducible stream.
func TestDiskFull_Reproducible(t *testing.T) {
	r1, sm1 := runDiskFull(t, true)
	r2, sm2 := runDiskFull(t, true)
	if (r1 == nil) != (r2 == nil) {
		t.Fatalf("non-reproducible verdict: r1=%v r2=%v", r1, r2)
	}
	if sm1.RejectedWrites() != sm2.RejectedWrites() {
		t.Fatalf("non-reproducible rejected-write count: %d != %d", sm1.RejectedWrites(), sm2.RejectedWrites())
	}
	if sm1.Oracle().NodeCount() != sm2.Oracle().NodeCount() {
		t.Fatalf("non-reproducible oracle node count: %d != %d", sm1.Oracle().NodeCount(), sm2.Oracle().NodeCount())
	}
}
