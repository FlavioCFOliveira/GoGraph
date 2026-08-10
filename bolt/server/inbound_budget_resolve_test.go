package server

import (
	"math"
	"runtime/debug"
	"testing"
)

// TestResolveMaxInboundDecodeBytes pins the option→ceiling mapping (#1845),
// mirroring the cypher result-ceiling contract but with a one-eighth GOMEMLIMIT
// fraction.
func TestResolveMaxInboundDecodeBytes(t *testing.T) {
	t.Parallel()

	if got := resolveMaxInboundDecodeBytes(MaxInboundDecodeBytesUnlimited); got != 0 {
		t.Errorf("unlimited sentinel → %d, want 0", got)
	}
	if got := resolveMaxInboundDecodeBytes(4096); got != 4096 {
		t.Errorf("positive value → %d, want 4096 (verbatim)", got)
	}

	// Zero derives from the Go soft memory limit: one eighth when set, else the
	// absolute DefaultMaxInboundDecodeBytes. Read the current limit (do not set
	// it — that is process-global) and assert the mapping matches.
	//
	// This test previously expected 0 (unlimited) on the no-limit branch, which
	// pinned the defect rather than the contract: an unset GOMEMLIMIT is the Go
	// default, so the engine-wide ceiling was absent in the commonest deployment
	// and aggregate pre-auth inbound memory was bounded only by MaxConnections
	// times the per-connection limits. The zero value now always selects a finite
	// ceiling; MaxInboundDecodeBytesUnlimited above remains the only opt-out.
	lim := debug.SetMemoryLimit(-1)
	want := DefaultMaxInboundDecodeBytes
	if lim > 0 && lim < math.MaxInt64 {
		want = lim / 8
	}
	if got := resolveMaxInboundDecodeBytes(0); got != want {
		t.Errorf("zero value → %d, want %d (GOMEMLIMIT/8, else DefaultMaxInboundDecodeBytes)", got, want)
	}
	if got := resolveMaxInboundDecodeBytes(0); got <= 0 {
		t.Errorf("zero value → %d: the default must never be unlimited", got)
	}
}
