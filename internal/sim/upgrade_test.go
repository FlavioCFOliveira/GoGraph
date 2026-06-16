package sim

import (
	"context"
	"testing"
)

// TestRunUpgrade_RoundTripParity is the PRIMARY upgrade deliverable: write a
// deterministic workload through a real WAL-backed store, close, reopen the same
// durable image across the simulated version boundary via real recovery, and
// assert full oracle parity (no loss, no ghost) — over several seeds.
func TestRunUpgrade_RoundTripParity(t *testing.T) {
	seeds := []uint64{0x1, 0x5217E, 0xC0FFEE, 0xDA7A, 0x900D}
	for _, seed := range seeds {
		t.Run("seed", func(t *testing.T) {
			res, err := RunUpgrade(context.Background(), UpgradeConfig{Seed: seed, Ops: 400})
			if err != nil {
				t.Fatalf("RunUpgrade(seed=%d): %v", seed, err)
			}
			if !res.Parity() {
				t.Fatalf("upgrade parity FAILED for seed=%d:\n%s", seed, res.Report.String())
			}
			// The recovered engine must hold exactly what the oracle modelled at
			// close: no data loss, no ghost nodes/edges.
			if res.RecoveredNodes != int64(res.WrittenNodes) {
				t.Errorf("seed=%d node count drift: written=%d recovered=%d", seed, res.WrittenNodes, res.RecoveredNodes)
			}
			if res.RecoveredEdges != int64(res.WrittenEdges) {
				t.Errorf("seed=%d edge count drift: written=%d recovered=%d", seed, res.WrittenEdges, res.RecoveredEdges)
			}
			// The image must carry real structure (the test is meaningless if the
			// workload wrote nothing).
			if res.WrittenNodes == 0 {
				t.Errorf("seed=%d wrote zero nodes; upgrade parity check is vacuous", seed)
			}
		})
	}
}

// TestRunUpgrade_WithIndex extends the round-trip to a user-created index,
// guarding index durability across the boundary (CREATE INDEX must survive the
// reopen and stay consistent with the base data).
func TestRunUpgrade_WithIndex(t *testing.T) {
	res, err := RunUpgrade(context.Background(), UpgradeConfig{
		Seed:       0xABBA,
		Ops:        400,
		IndexSpecs: []IndexSpec{{Label: "Person", Property: "name"}},
	})
	if err != nil {
		t.Fatalf("RunUpgrade with index: %v", err)
	}
	if !res.Parity() {
		t.Fatalf("upgrade-with-index parity FAILED:\n%s", res.Report.String())
	}
}

// TestRunUpgrade_Deterministic asserts the write phase is a pure function of the
// seed: two upgrades with the same seed write the identical durable counts.
func TestRunUpgrade_Deterministic(t *testing.T) {
	a, err := RunUpgrade(context.Background(), UpgradeConfig{Seed: 0x33, Ops: 300})
	if err != nil {
		t.Fatalf("RunUpgrade a: %v", err)
	}
	b, err := RunUpgrade(context.Background(), UpgradeConfig{Seed: 0x33, Ops: 300})
	if err != nil {
		t.Fatalf("RunUpgrade b: %v", err)
	}
	if a.WrittenNodes != b.WrittenNodes || a.WrittenEdges != b.WrittenEdges {
		t.Errorf("non-deterministic write phase: a=(%d,%d) b=(%d,%d)",
			a.WrittenNodes, a.WrittenEdges, b.WrittenNodes, b.WrittenEdges)
	}
}

// TestCheckCorruptImageRejected is the fail-stop deliverable: a durable image
// corrupted inside a committed frame must be rejected on reopen, never silently
// accepted.
func TestCheckCorruptImageRejected(t *testing.T) {
	seeds := []uint64{0x1, 0xBEEF, 0x5EED}
	for _, seed := range seeds {
		t.Run("seed", func(t *testing.T) {
			if err := CheckCorruptImageRejected(context.Background(), seed); err != nil {
				t.Errorf("corrupt-image fail-stop failed for seed=%d: %v", seed, err)
			}
		})
	}
}
