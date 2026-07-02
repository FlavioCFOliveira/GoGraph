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

	// Zero derives from the Go soft memory limit: one eighth when set, else
	// unlimited (0). Read the current limit (do not set it — that is
	// process-global) and assert the mapping matches.
	lim := debug.SetMemoryLimit(-1)
	want := int64(0)
	if lim > 0 && lim < math.MaxInt64 {
		want = lim / 8
	}
	if got := resolveMaxInboundDecodeBytes(0); got != want {
		t.Errorf("zero value → %d, want %d (GOMEMLIMIT/8 or unlimited)", got, want)
	}
}
