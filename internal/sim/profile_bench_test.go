package sim

import (
	"context"
	"testing"
)

// BenchmarkSimulatorRun drives the deterministic safety loop end-to-end so that
// a CPU/heap profile over a fixed wall-clock window (e.g. -benchtime=180s)
// captures where the DST harness — and, through it, the real cypher.Engine —
// spends its time. Each iteration runs a fresh Simulator over a fixed tick
// budget with the default workload; the seed is varied per iteration so the
// profile is not dominated by one degenerate draw sequence.
//
// This is a profiling harness, not a regression gate: it asserts only that the
// run does not surface a violation, so a real bug still fails the bench.
func BenchmarkSimulatorRun(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		sm, err := New(Config{
			Seed:     uint64(i) + 1,
			MaxTicks: 20000,
			Workload: DefaultWorkload(NewSeed(uint64(i) + 1)),
		})
		if err != nil {
			b.Fatalf("New: %v", err)
		}
		report, err := sm.Run(ctx)
		if err != nil {
			b.Fatalf("Run: %v", err)
		}
		if report != nil {
			b.Fatalf("violation: %s", report.String())
		}
		if err := sm.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}
}
