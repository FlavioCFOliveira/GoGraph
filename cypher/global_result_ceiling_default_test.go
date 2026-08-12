// global_result_ceiling_default_test.go — the engine-wide result-byte ceiling
// must be FINITE on a default-configured engine.
//
// The per-query budgets are finite by default (DefaultMaxResultRows = 10M,
// DefaultMaxResultBytes = 1 GiB) and that is what the 2026-08-10 certification
// checked. A per-query bound, however, says nothing about the SUM: N concurrent
// clients each staying inside their own 1 GiB may still materialise N GiB, and
// GlobalMaxResultBytes is the only bound that governs the aggregate.
//
// It resolved to 0 — unlimited — whenever the process had no Go soft memory
// limit, which is the Go runtime's default state. So under exactly the extreme
// concurrency this module targets, the bound that matters was the one that was
// off. This is the sibling of bolt/server's inbound-decode ceiling; both sites
// shared the idiom and both are pinned, because a fix that lands on one of two
// call sites is a closed ticket with a live defect.
package cypher

import (
	"math"
	"runtime/debug"
	"testing"
)

// TestResolveGlobalMaxResultBytes_NoMemoryLimit is the regression gate.
func TestResolveGlobalMaxResultBytes_NoMemoryLimit(t *testing.T) {
	if lim := debug.SetMemoryLimit(-1); lim != math.MaxInt64 {
		t.Skipf("this process has GOMEMLIMIT=%d set, so the no-limit branch cannot be "+
			"exercised here; run without GOMEMLIMIT to cover it", lim)
	}

	got := resolveGlobalMaxResultBytes(0)
	if got <= 0 {
		t.Errorf("resolveGlobalMaxResultBytes(0) = %d with no GOMEMLIMIT set, want a finite "+
			"positive ceiling: with no aggregate bound, N concurrent queries each inside the "+
			"finite per-query budget still sum without limit", got)
	}
	if got != DefaultGlobalMaxResultBytes {
		t.Errorf("resolveGlobalMaxResultBytes(0) = %d, want DefaultGlobalMaxResultBytes (%d)",
			got, DefaultGlobalMaxResultBytes)
	}

	// The explicit opt-out must remain the ONLY route to unlimited.
	if got := resolveGlobalMaxResultBytes(GlobalMaxResultBytesUnlimited); got != 0 {
		t.Errorf("resolveGlobalMaxResultBytes(GlobalMaxResultBytesUnlimited) = %d, want 0", got)
	}
	// An explicit positive value is honoured verbatim.
	if got := resolveGlobalMaxResultBytes(8192); got != 8192 {
		t.Errorf("resolveGlobalMaxResultBytes(8192) = %d, want 8192", got)
	}
}

// TestGlobalResultCeiling_AggregateExceedsPerQueryBound records the reason the
// aggregate ceiling has to exist at all: it must sit above the per-query budget
// (otherwise a single legitimate query could never complete) while still being
// finite. Those two requirements together are the whole specification, and
// pinning them stops a future tuning pass from setting the aggregate below the
// per-query cap, which would make every maximal single query fail.
func TestGlobalResultCeiling_AggregateExceedsPerQueryBound(t *testing.T) {
	if DefaultGlobalMaxResultBytes <= DefaultMaxResultBytes {
		t.Errorf("DefaultGlobalMaxResultBytes (%d) must exceed DefaultMaxResultBytes (%d): "+
			"an aggregate ceiling at or below the per-query budget would reject a single "+
			"query that is within its own limit",
			DefaultGlobalMaxResultBytes, DefaultMaxResultBytes)
	}
	if DefaultGlobalMaxResultBytes <= 0 {
		t.Errorf("DefaultGlobalMaxResultBytes = %d, want finite and positive", DefaultGlobalMaxResultBytes)
	}
}

// TestGlobalCeiling_DerivedFromAContainerCap covers rmp #2421: the fixed default
// is not a bound inside a container smaller than it, so a discoverable limit must
// LOWER the ceiling.
//
// The derivation is tested against injected limits rather than against whatever
// the host running this test reports, because a developer machine is not
// memory-constrained and would exercise only the fall-through.
func TestGlobalCeiling_DerivedFromAContainerCap(t *testing.T) {
	t.Parallel()
	const giB = int64(1) << 30
	for _, tc := range []struct {
		name  string
		avail int64
		want  int64
		why   string
	}{
		{"512 MiB container", giB / 2, giB / 4, "half of a cap far below the default"},
		{"2 GiB container", 2 * giB, giB, "half, still below the 4 GiB default"},
		{"8 GiB container", 8 * giB, DefaultGlobalMaxResultBytes, "half is 4 GiB, not BELOW the default, so the default stands"},
		{"64 GiB host", 64 * giB, DefaultGlobalMaxResultBytes, "a large bound may not RAISE the aggregate ceiling"},
		{"absurdly small", 1, DefaultGlobalMaxResultBytes, "half rounds to zero, which is the unlimited sentinel — must not be returned"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := globalCeilingFromAvailable(tc.avail)
			if got != tc.want {
				t.Errorf("globalCeilingFromAvailable(%d) = %d, want %d — %s", tc.avail, got, tc.want, tc.why)
			}
			if got <= 0 {
				t.Errorf("derived ceiling %d is not positive; zero means UNLIMITED here", got)
			}
			if got > DefaultGlobalMaxResultBytes {
				t.Errorf("derived ceiling %d exceeds the fixed default %d; the derivation may only lower it",
					got, DefaultGlobalMaxResultBytes)
			}
		})
	}
}
