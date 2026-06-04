//go:build !gograph_debug

package search

import (
	"math"
	"testing"
)

// TestShortestPath_Int32OverflowProductionSilentInDefaultBuild pins the
// production contract: without the gograph_debug tag the relaxation
// assertion is a compiled-out no-op, so an out-of-range integer path
// does NOT panic. The result is (by the documented precondition)
// allowed to be wrong — here it is observably wrapped, which is
// precisely why the precondition exists and why the debug assertion is
// provided for development. The test asserts only that production stays
// panic-free; the debug build (overflow_assert_debug_test.go) is what
// turns the same wraparound into a loud failure.
func TestShortestPath_Int32OverflowProductionSilentInDefaultBuild(t *testing.T) {
	t.Parallel()
	// 5 hops of ~0.5*MaxInt32 sum past MaxInt32 and wrap.
	const (
		n      = 6
		perHop = int32(math.MaxInt32 / 2)
	)
	c, ids := int32PathCSR(t, n, perHop)

	d, err := BellmanFord(c, ids[0])
	if err != nil {
		t.Fatalf("BellmanFord must not error on integer overflow (precondition violation is undefined, not an error): %v", err)
	}
	// The deepest node's reported distance has wrapped; we only require
	// that no panic occurred in the production build. Surface the value
	// to make the precondition violation visible in the test log.
	if dist, ok := d.Distance(ids[n-1]); ok {
		if want := int64(n-1) * int64(perHop); int64(dist) == want {
			t.Fatalf("deepest distance = %d did NOT wrap (want a wrapped value != %d); test no longer exercises overflow", dist, want)
		}
		t.Logf("production build: deepest distance = %d (wrapped; no overflow guard, debug build would trap this)", dist)
	}
}
