package recovery

// list_cap_hint_test.go — regression for the PropList decode over-reservation
// (2026-07-13 security re-audit, R1 / CWE-770 / CWE-789). decodeRecoveryListProp
// pre-reserved its result slice from the untrusted element count, clamped only
// to remaining/recoveryListElemMinBytes ELEMENTS — but each reserved
// lpg.PropertyValue slot is ~24 bytes, so that clamp is ~4.8× the remaining wire
// bytes, up to several GiB for a large frame with a hostile count (a fatal
// out-of-memory at store-open that re-fires on every restart). The hint is now
// additionally capped at recoveryListCapHintMax.

import "testing"

func TestRecoveryListCapHint_BoundsEagerReservation(t *testing.T) {
	t.Parallel()

	// Hostile count with a large remaining: pre-fix this returned
	// remaining/5 (~20M elements ≈ 480 MiB reserved). Must now be capped.
	if got := recoveryListCapHint(1<<31, 100<<20); got > recoveryListCapHintMax {
		t.Fatalf("cap hint for hostile count = %d, want <= %d (bounded eager reservation)",
			got, recoveryListCapHintMax)
	}
	// A large frame with a large-but-honest count is still capped for the
	// eager reservation (append grows to the real element count).
	if got := recoveryListCapHint(1_000_000, 100<<20); got != recoveryListCapHintMax {
		t.Fatalf("cap hint for large count = %d, want %d", got, recoveryListCapHintMax)
	}
	// A small honest count below the cap is returned as-is.
	if got := recoveryListCapHint(10, 100<<20); got != 10 {
		t.Fatalf("cap hint for honest count 10 = %d, want 10", got)
	}
	// remaining/recoveryListElemMinBytes still bounds when it is smallest.
	if got := recoveryListCapHint(1<<31, 100); got != 100/recoveryListElemMinBytes {
		t.Fatalf("cap hint for tiny remaining = %d, want %d", got, 100/recoveryListElemMinBytes)
	}
}
