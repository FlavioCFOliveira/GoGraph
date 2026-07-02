package csrfile

import (
	"errors"
	"testing"
)

// TestAllocAligned8_ZeroReturnsNil confirms the n<=0 guard: allocAligned8 must
// return nil rather than panic on &backing[0] of an empty backing slice.
// Security assessment 2026-07-02 (#1848).
func TestAllocAligned8_ZeroReturnsNil(t *testing.T) {
	t.Parallel()
	if got := allocAligned8(0); got != nil {
		t.Errorf("allocAligned8(0) = %v, want nil", got)
	}
	if got := allocAligned8(-1); got != nil {
		t.Errorf("allocAligned8(-1) = %v, want nil", got)
	}
	// A positive length still yields a usable, correctly-sized buffer.
	if got := allocAligned8(8); len(got) != 8 {
		t.Errorf("allocAligned8(8) len = %d, want 8", len(got))
	}
}

// TestOpenBytes_EmptyIsCleanError confirms that the byte-backed reader returns
// the clean ErrHeaderTooShort for an empty image instead of panicking inside
// allocAligned8 — the fail-stop path is a returned error, not a crash. #1848.
func TestOpenBytes_EmptyIsCleanError(t *testing.T) {
	t.Parallel()
	if _, err := openBytes(nil); !errors.Is(err, ErrHeaderTooShort) {
		t.Errorf("openBytes(nil) err = %v, want ErrHeaderTooShort", err)
	}
	if _, err := openBytes([]byte{}); !errors.Is(err, ErrHeaderTooShort) {
		t.Errorf("openBytes(empty) err = %v, want ErrHeaderTooShort", err)
	}
}
