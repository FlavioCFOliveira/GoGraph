package sim

import (
	"testing"

	"go.uber.org/goleak"
)

// TestWire_LockStepReproducible proves the single-connection lock-step wire path
// is deterministic: the same seed yields a byte-identical op stream AND identical
// terminal responses across two independent runs. This is the determinism proof
// the gate requires for the lock-step mode.
func TestWire_LockStepReproducible(t *testing.T) {
	t.Parallel()
	const seed = 0xC0FFEE
	const nOps = 40

	first, err := RunLockStepWire(seed, nOps)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunLockStepWire(seed, nOps)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	if len(first.Exchanges) != nOps {
		t.Fatalf("transcript length %d, want %d", len(first.Exchanges), nOps)
	}
	if !first.Equal(second) {
		// Surface the first divergence for a useful failure message.
		for i := range first.Exchanges {
			if i >= len(second.Exchanges) || first.Exchanges[i] != second.Exchanges[i] {
				t.Fatalf("transcript diverged at op %d:\n  run1: %+v\n  run2: %+v",
					i, first.Exchanges[i], second.Exchanges[i])
			}
		}
		t.Fatal("transcripts unequal but no per-op divergence found")
	}
}

// TestWire_LockStepGoleak proves a lock-step session leaks no goroutine after the
// server is closed.
func TestWire_LockStepGoleak(t *testing.T) {
	defer goleak.VerifyNone(t)
	if _, err := RunLockStepWire(0xABCDEF, 25); err != nil {
		t.Fatalf("RunLockStepWire: %v", err)
	}
}
