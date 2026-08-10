// inbound_ceiling_default_test.go — the engine-wide inbound-decode ceiling must
// be FINITE on a default-configured server.
//
// The per-message defences in bolt/packstream are thorough: a single hostile
// message cannot amplify past maxDecodedCollectionBytes, and
// bolt/proto's TestChunkedReader_ReassemblyAggregateBoundConcurrent proves the
// aggregate pool works when it is enabled. Neither says anything about whether a
// default-configured server actually TURNS IT ON.
//
// It did not. Options.MaxInboundDecodeBytes left at its zero value resolves
// through resolveMaxInboundDecodeBytes, which derives the ceiling from
// GOMEMLIMIT and returns 0 — the "unlimited" value — whenever no memory limit is
// set. An unset GOMEMLIMIT is the Go runtime's default (debug.SetMemoryLimit(-1)
// reports math.MaxInt64), so on a default deployment the documented
// "engine-wide inbound-memory ceiling" was inert and the real bound was
// MaxConnections x per-connection limits: 1024 x 16 MiB of reassembly buffers
// plus 1024 x 128 MiB of decoded collections. That allocation is reachable
// PRE-AUTHENTICATION, because a HELLO must be decoded before it can be
// authenticated.
//
// The existence of the explicit MaxInboundDecodeBytesUnlimited (-1) opt-out
// sentinel is what settles intent: if the zero value already meant unlimited,
// the sentinel would be redundant.
package server

import (
	"math"
	"runtime/debug"
	"testing"
)

// TestInboundDecodeCeiling_FiniteByDefault is the regression gate. It asserts on
// the server's own budget object rather than on a memory measurement, so it is
// deterministic and cheap, and it fails for the defect rather than for load.
func TestInboundDecodeCeiling_FiniteByDefault(t *testing.T) {
	srv, err := NewServer(nil, Options{Auth: NoAuthHandler{}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.inbound == nil {
		t.Fatal("default server has no inbound budget at all")
	}
	if !srv.inbound.Enabled() {
		t.Errorf("default-configured server's engine-wide inbound-decode ceiling is DISABLED "+
			"(GOMEMLIMIT=%d); aggregate inbound memory is then bounded only by "+
			"MaxConnections x per-connection limits, pre-authentication. A bounded-resources "+
			"mandate requires an explicit finite upper bound; use "+
			"MaxInboundDecodeBytesUnlimited to opt out on purpose",
			debug.SetMemoryLimit(-1))
	}
}

// TestResolveMaxInboundDecodeBytes_NoMemoryLimit pins the resolution itself, so
// the defect cannot come back through the helper while the server-level
// assertion above still passes for some other reason.
func TestResolveMaxInboundDecodeBytes_NoMemoryLimit(t *testing.T) {
	// Confirm the premise this test rests on: an unconfigured Go process reports
	// math.MaxInt64 as its soft memory limit, which is the branch that used to
	// fall through to "unlimited".
	if lim := debug.SetMemoryLimit(-1); lim != math.MaxInt64 {
		t.Skipf("this process has GOMEMLIMIT=%d set, so the no-limit branch cannot be "+
			"exercised here; run without GOMEMLIMIT to cover it", lim)
	}

	if got := resolveMaxInboundDecodeBytes(0); got <= 0 {
		t.Errorf("resolveMaxInboundDecodeBytes(0) = %d with no GOMEMLIMIT set, want a finite "+
			"positive ceiling: the zero value must select a bound, not the absence of one", got)
	}

	// The explicit opt-out must still work, and must be the ONLY way to get 0.
	if got := resolveMaxInboundDecodeBytes(MaxInboundDecodeBytesUnlimited); got != 0 {
		t.Errorf("resolveMaxInboundDecodeBytes(MaxInboundDecodeBytesUnlimited) = %d, want 0", got)
	}
	// An explicit positive value is honoured verbatim.
	if got := resolveMaxInboundDecodeBytes(4096); got != 4096 {
		t.Errorf("resolveMaxInboundDecodeBytes(4096) = %d, want 4096", got)
	}
}
