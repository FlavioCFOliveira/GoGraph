package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestSimulator_CrashStormSoak is the soak-layer crash-storm: a high-tick,
// dense-crash run across several seeds, asserting the durability invariant holds
// across many crash+recovery cycles. It is gated to the soak layer because each
// run replays a large, growing WAL on every crash and so is minutes-long; the
// short layer covers the same code path at a smaller tick budget
// (TestSimulator_CrashRecoveryCycle).
//
// The storm uses a write-heavy mix so the graph stays populated (recovery has
// real committed state to replay) and a crash probability tuned so each run
// goes through tens of crash cycles, every one of which runs the full-scan
// CheckDurability at the boundary. A single dropped or leaked committed op over
// the whole storm fails the run with a reproducible seed.
//
// Tick and crash budgets are deliberately bounded. The SimStore's recovery is
// WAL-only (snapshot/checkpoint truncation on SimDisk was a tracked follow-up
// of #1540), so every crash replays the whole growing WAL from the start: the
// cumulative replay cost grows ~quadratically in ticks and superlinearly in the
// crash count. The chosen budget keeps the storm to tens of cycles per run and
// completes well inside a normal soak window while still exercising the
// durability boundary heavily; pushing ticks or crash density much higher makes
// the run dominated by re-replay rather than new fault coverage.
func TestSimulator_CrashStormSoak(t *testing.T) {
	testlayers.RequireSoak(t)

	const (
		masterSeed = 0xC8A54
		runs       = 3
		ticks      = 40_000
	)
	for i := 0; i < runs; i++ {
		// Derive distinct child seeds from the master via the FNV prime so the
		// runs explore different op + crash streams while staying a pure function
		// of the master seed.
		seed := uint64(masterSeed) ^ uint64(i+1)*0x100000001b3
		t.Run("storm", func(t *testing.T) {
			s, err := New(Config{
				Seed:       seed,
				MaxTicks:   ticks,
				CheckEvery: 256, // online checks stay cheap; the crash boundary uses the full scan.
				Workload:   WriteHeavyWorkload(NewSeed(seed)),
				// ~1/1600 over 40k ticks yields roughly 20-30 crash cycles per run:
				// a genuine storm whose cumulative WAL re-replay stays bounded.
				Crash: CrashConfig{Enabled: true, CrashProb: 1.0 / 1600.0},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer func() { _ = s.Close() }()

			report, err := s.Run(context.Background())
			if err != nil {
				t.Fatalf("crash-storm run error (seed %d): %v", seed, err)
			}
			if report != nil {
				t.Fatalf("crash-storm reported violations (seed %d):\n%s", seed, report)
			}
			if s.CrashCount() < 10 {
				t.Fatalf("crash-storm (seed %d) only saw %d crashes over %d ticks, expected a storm",
					seed, s.CrashCount(), ticks)
			}
			t.Logf("seed %d: %d crash cycles, %d WAL ops replayed, survived nodes=%d edges=%d",
				seed, s.CrashCount(), s.ReplayedOps(), s.Oracle().NodeCount(), s.Oracle().EdgeCount())
		})
	}
}
