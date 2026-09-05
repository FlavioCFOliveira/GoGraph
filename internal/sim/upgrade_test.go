package sim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
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
			err := CheckCorruptImageRejected(context.Background(), seed)
			if errors.Is(err, ErrCorruptImageCheckAborted) {
				t.Fatalf("the check aborted instead of reaching a verdict for seed=%d: %v", seed, err)
			}
			if err != nil {
				t.Errorf("corrupt-image fail-stop failed for seed=%d: %v", seed, err)
			}
		})
	}
}

// TestCorruptImage_CancelledContextIsNotAVerdict pins the distinction the check
// previously could not express: a cancelled context means the check DID NOT
// FINISH, and must be reported as such rather than folded into the durability
// verdict.
//
// The requirement is one-directional and deliberately so — the check must NOT
// pass. A cancelled run that returned nil would be the vacuous pass this task
// exists to remove; a cancelled run that returned a bare error would be read by
// the caller as a durability defect that did not happen.
func TestCorruptImage_CancelledContextIsNotAVerdict(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := CheckCorruptImageRejected(ctx, 0x1)
	if err == nil {
		t.Fatalf("a cancelled run PASSED the fail-stop check: the oracle reported a durability guarantee it never observed")
	}
	if !errors.Is(err, ErrCorruptImageCheckAborted) {
		t.Errorf("a cancelled run is not reported as aborted, so a caller cannot tell it from a durability failure: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the abort does not carry the cancellation cause: %v", err)
	}
	t.Logf("cancelled run reported as: %v", err)
}

// TestCorruptImage_RejectsANonCorruptionReopenFailure is the falsifiability
// proof for the specificity of the verdict, and the direct regression test for
// the defect: the check used to accept ANY reopen error as proof of fail-stop.
//
// The wrong input is constructed through the reopen seam, because [SimDisk]
// cannot be made to fail an open on demand. Each substituted error is one a real
// reopen could plausibly return for a reason having nothing to do with the
// corruption — and every one of them made the old oracle PASS.
func TestCorruptImage_RejectsANonCorruptionReopenFailure(t *testing.T) {
	t.Parallel()

	unrelated := []struct {
		name string
		err  error
	}{
		{"a permission failure", os.ErrPermission},
		{"a missing file", os.ErrNotExist},
		{"the WAL held by another writer", wal.ErrWALLocked},
		{"a BENIGN torn tail, which recovery is meant to accept", wal.ErrTornFrame},
		{"an unclassified failure", errors.New("some other reason entirely")},
	}
	for _, tc := range unrelated {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkCorruptImageRejected(context.Background(), 0x1, func(*SimDisk) (*SimStore, error) {
				return nil, fmt.Errorf("sim: reopen: %w", tc.err)
			})
			if err == nil {
				t.Fatalf("the oracle ACCEPTED %q as proof of the fail-stop guarantee: the check cannot fail and therefore proves nothing", tc.err)
			}
			if !strings.Contains(err.Error(), "NOT with a WAL corruption verdict") {
				t.Errorf("the failure does not explain that the refusal was the wrong refusal: %v", err)
			}
			t.Logf("refused: %v", err)
		})
	}
}

// TestCorruptImage_AcceptsOnlyTheCorruptionVerdicts is the positive half of the
// same proof: each error that genuinely IS a corrupted-frame verdict must pass.
// Without it the clause above could be satisfied by an oracle that rejects
// everything, which would be just as useless in the other direction.
func TestCorruptImage_AcceptsOnlyTheCorruptionVerdicts(t *testing.T) {
	t.Parallel()

	for _, sentinel := range walCorruptionFailStop {
		t.Run(sentinel.Error(), func(t *testing.T) {
			t.Parallel()
			err := checkCorruptImageRejected(context.Background(), 0x1, func(*SimDisk) (*SimStore, error) {
				return nil, fmt.Errorf("sim: WAL recovery found corruption: %w", sentinel)
			})
			if err != nil {
				t.Errorf("a genuine corruption verdict (%v) was refused: %v", sentinel, err)
			}
		})
	}
}

// TestCorruptImage_DetectsASilentlyAcceptedImage keeps the opposite polarity
// wired: a reopen that SUCCEEDS over a corrupted image is the durability defect
// the check exists to catch.
func TestCorruptImage_DetectsASilentlyAcceptedImage(t *testing.T) {
	t.Parallel()

	err := checkCorruptImageRejected(context.Background(), 0x1, func(*SimDisk) (*SimStore, error) {
		// A reopen that succeeds regardless of the corrupted image, standing in
		// for a recovery path that silently accepts it.
		return OpenSimStore(NewSimDisk(NewSeed(0xFEED), 0), simulatorStoreConfig())
	})
	if err == nil {
		t.Fatalf("the oracle accepted a reopen that SUCCEEDED over a corrupted image")
	}
	if !strings.Contains(err.Error(), "SILENTLY ACCEPTED") {
		t.Errorf("the failure does not name the durability violation: %v", err)
	}
	t.Logf("refused: %v", err)
}
