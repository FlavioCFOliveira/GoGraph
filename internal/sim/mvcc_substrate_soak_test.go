//go:build soak || nightly

package sim

// mvcc_substrate_soak_test.go — the long-running arm of the MVCC-substrate
// telemetry oracle (rmp #2470).
//
// The short layer proves the oracle works and that each clause fires; it cannot
// prove the substrate STAYS bounded, because that is a question about a run long
// enough for a leak to accumulate. This arm churns two orders of magnitude more
// versions through the same objects, across repeated checkpoints, and holds the
// substrate to the same bounds throughout — which is where a vacuum that
// gradually falls behind, or a chain that grows a little on every cycle, becomes
// visible.
//
// Runs under the soak layer only (docs/test-layers.md); the short layer runs the
// smaller configuration in mvcc_substrate_test.go.

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

// TestMVCCSubstrate_LongRunningBounded is the sustained-churn arm: version
// memory must stay under the published ceiling, the watermark must keep
// advancing, the vacuum must keep reclaiming, and retained chain depth must keep
// returning to its bound — for the whole run, not merely at the end.
func TestMVCCSubstrate_LongRunningBounded(t *testing.T) {
	defer goleak.VerifyNone(t)
	res, err := RunMVCCSubstrateChurn(context.Background(), MVCCSubstrateConfig{
		Seed:        0x2470,
		Objects:     16,
		Rounds:      400_000,
		SampleEvery: 500,
		Checkpoints: 8,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Clean() {
		t.Fatalf("violations over the long-running arm:\n%s\nevidence: %s",
			renderViolations(res.Violations), res.Summary)
	}
	if res.CheckpointsRun != 8 {
		t.Fatalf("checkpoints run = %d, want 8", res.CheckpointsRun)
	}
	// The point of the long arm: far more versions were created than the
	// substrate ever held at once, which is reclamation keeping up rather than
	// the workload being small.
	if res.ReclaimedRecords <= res.MaxVersionRecords {
		t.Fatalf("the vacuum released %d records against a high-water mark of %d: over a run this long that"+
			" is retention, not reclamation (%s)", res.ReclaimedRecords, res.MaxVersionRecords, res.Summary)
	}
	t.Logf("%s", res.Summary)
}

// TestMVCCSubstrate_LongRunningAbortStorm sustains the abort-heavy arm, so the
// synchronous withdrawal of aborted versions is exercised across far more
// refusals than the short layer drives. A withdrawal that leaked even a single
// version per abort would accumulate visibly here.
func TestMVCCSubstrate_LongRunningAbortStorm(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	for _, seed := range []uint64{3, 19, 101} {
		res, cont, err := RunMVCCSubstrateAborts(ctx, MVCCContentionConfig{
			Seed: seed, Ticks: 40_000, Sessions: 12, Counters: 2,
		})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if !res.Clean() {
			t.Fatalf("seed %d violations:\n%s\nevidence: %s", seed,
				renderViolations(res.Violations), res.Summary)
		}
		if cont.TxConflicted == 0 {
			t.Fatalf("seed %d produced no conflict, so the storm aborted nothing", seed)
		}
		t.Logf("seed=%d refusals=%d | %s", seed, cont.TxConflicted, res.Summary)
	}
}
