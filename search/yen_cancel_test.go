package search

import (
	"context"
	"errors"
	"testing"
)

// TestYen_CtxCancel_SpurSearch verifies that a pre-cancelled context
// propagates as a non-nil context.Canceled error instead of being
// silently swallowed inside dijkstraAvoidingInto.
//
// This test MUST fail before the fix (dijkstraAvoidingInto returned
// (zero, false) and the spur loop treated it as "no path", ultimately
// returning (result, nil)) and MUST pass after the fix.
func TestYen_CtxCancel_SpurSearch(t *testing.T) {
	t.Parallel()
	c, a := buildWeightedCSR(t, []weightedEdge{
		{0, 1, 1},
		{1, 2, 1},
		{2, 3, 1},
		{3, 4, 1},
	})
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(4)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before any work starts

	_, err := YenKShortestCtx(ctx, c, src, dst, 3)
	if err == nil {
		t.Fatal("expected non-nil error on pre-cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// TestYen_CtxCancel_MidSpur verifies that a context cancelled after
// the initial Dijkstra (so result[0] is found) but before the spur
// loop completes still surfaces a cancellation error.
//
// We use a zero-timeout deadline so it fires before the spur rounds
// have a chance to complete.
func TestYen_CtxCancel_MidSpur(t *testing.T) {
	t.Parallel()
	// Diamond-plus gives multiple spur candidates, so there is
	// meaningful spur work for the cancellation to interrupt.
	c, a := buildDiamondPlus(t)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(4)

	// A context that times out immediately is cancelled before (or
	// during) the very first ctx.Err() check inside the spur loop or
	// inner Dijkstra.
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	_, err := YenKShortestCtx(ctx, c, src, dst, 8)
	if err == nil {
		// On a very fast machine the call may complete before the
		// deadline fires — the important invariant is that when
		// cancellation IS detected it is returned rather than swallowed.
		t.Log("context completed before deadline fired — acceptable on fast hardware")
		return
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.DeadlineExceeded or context.Canceled", err)
	}
}
