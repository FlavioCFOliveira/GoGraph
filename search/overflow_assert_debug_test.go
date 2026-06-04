//go:build gograph_debug

package search

import (
	"math"
	"strings"
	"testing"
)

// recoverOverflowPanic runs fn and reports whether it panicked with the
// integer-overflow assertion message from assertNoRelaxOverflow.
func recoverOverflowPanic(t *testing.T, fn func()) (panicked bool, msg string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			msg, _ = r.(string)
		}
	}()
	fn()
	return false, ""
}

// TestDebugAssert_BellmanFordIntOverflowPanics is the overflow half of
// the #1323 acceptance criterion. Under -tags gograph_debug, relaxing a
// path whose int32 cumulative weight runs past math.MaxInt32 wraps the
// distance; the relaxation assertion must trap it with a panic rather
// than let Bellman-Ford return a silently wrong result.
//
// Fail-pre (assertion absent / production build): no panic, wrapped
// distance returned silently.
// Pass-post (this debug build): assertNoRelaxOverflow panics.
func TestDebugAssert_BellmanFordIntOverflowPanics(t *testing.T) {
	const (
		n      = 6
		perHop = int32(math.MaxInt32 / 2) // 5 hops overflow int32
	)
	c, ids := int32PathCSR(t, n, perHop)

	panicked, msg := recoverOverflowPanic(t, func() {
		_, _ = BellmanFord(c, ids[0])
	})
	if !panicked {
		t.Fatal("BellmanFord did not panic on int32 cumulative overflow under gograph_debug")
	}
	if !strings.Contains(msg, "integer distance overflow") {
		t.Fatalf("panic message = %q, want it to mention %q", msg, "integer distance overflow")
	}
}

// TestDebugAssert_JohnsonIntOverflowPanics confirms the reweighting
// pass of Johnson (the most overflow-exposed routine) also traps a
// cumulative-distance wraparound under the debug build.
func TestDebugAssert_JohnsonIntOverflowPanics(t *testing.T) {
	const (
		n      = 6
		perHop = int32(math.MaxInt32 / 2)
	)
	c, _ := int32PathCSR(t, n, perHop)

	panicked, msg := recoverOverflowPanic(t, func() {
		_, _ = JohnsonAPSP(c)
	})
	if !panicked {
		t.Fatal("JohnsonAPSP did not panic on int32 cumulative overflow under gograph_debug")
	}
	if !strings.Contains(msg, "integer distance overflow") {
		t.Fatalf("panic message = %q, want it to mention %q", msg, "integer distance overflow")
	}
}

// TestDebugAssert_InRangeDoesNotPanic guards against a false-positive
// assertion: a path whose int32 cumulative weight stays in range must
// NOT trip the overflow trap even under the debug build, and must still
// produce monotone distances.
func TestDebugAssert_InRangeDoesNotPanic(t *testing.T) {
	const (
		n      = 101
		perHop = int32(1_000_000) // 100 hops = 1e8, well within int32
	)
	c, ids := int32PathCSR(t, n, perHop)

	panicked, _ := recoverOverflowPanic(t, func() {
		d, err := BellmanFord(c, ids[0])
		if err != nil {
			t.Fatalf("BellmanFord: %v", err)
		}
		last, ok := d.Distance(ids[n-1])
		if !ok {
			t.Fatal("deepest node unreachable")
		}
		if want := int32(n-1) * perHop; last != want {
			t.Fatalf("deepest distance = %d, want %d", last, want)
		}
	})
	if panicked {
		t.Fatal("debug assertion fired on an in-range integer path (false positive)")
	}
}

// TestDebugAssert_NegativeWeightOverflowPanics covers the negative side
// of the overflow oracle: a chain of large-magnitude negative weights
// drives the cumulative distance below math.MinInt32, wrapping positive;
// the assertion's (weight < 0 && cand > prev) branch must trap it.
func TestDebugAssert_NegativeWeightOverflowPanics(t *testing.T) {
	const (
		n      = 6
		perHop = int32(math.MinInt32 / 2) // 5 hops underflow int32
	)
	c, ids := int32PathCSR(t, n, perHop)

	panicked, msg := recoverOverflowPanic(t, func() {
		_, _ = BellmanFord(c, ids[0])
	})
	if !panicked {
		t.Fatal("BellmanFord did not panic on negative int32 cumulative underflow under gograph_debug")
	}
	if !strings.Contains(msg, "integer distance overflow") {
		t.Fatalf("panic message = %q, want it to mention %q", msg, "integer distance overflow")
	}
}
