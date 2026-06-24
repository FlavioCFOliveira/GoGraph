package bulk

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLoader_CtxCancelPreCancelled asserts that Drain with a
// pre-cancelled context returns context.Canceled immediately and
// ingests no rows. The existing TestLoader_DrainCancelled covers the
// same signal but does not assert Rows() == 0 or check the wrapped
// error type explicitly; this test adds both guarantees.
func TestLoader_CtxCancelPreCancelled(t *testing.T) {
	t.Parallel()
	l := New(Options{Directed: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	ch := make(chan Edge) // unbuffered, never sends
	_, err := l.Drain(ctx, ch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain pre-cancelled = %v, want context.Canceled", err)
	}
	if l.Rows() != 0 {
		t.Fatalf("Rows = %d after pre-cancelled Drain, want 0", l.Rows())
	}
}

// TestLoader_CtxCancelMidDrain verifies that Drain honours cancellation
// while blocked waiting for more edges from a channel that is neither
// closed nor producing items. The test:
//
//  1. Sends 5 edges into a buffered channel before Drain starts.
//  2. Starts Drain in a separate goroutine (channel has no close, so
//     Drain would block after consuming the 5 buffered edges).
//  3. Cancels the context after a short delay.
//  4. Asserts Drain returns context.Canceled within a bounded deadline
//     and that Rows() reflects the 5 rows ingested before the cancel.
func TestLoader_CtxCancelMidDrain(t *testing.T) {
	t.Parallel()

	l := New(Options{Directed: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Channel capacity = 5 so the sender does not block.
	ch := make(chan Edge, 5)
	for i := range 5 {
		ch <- Edge{
			Src:    "s" + string(rune('a'+i)),
			Dst:    "d" + string(rune('a'+i)),
			Weight: int64(i),
		}
	}
	// Do NOT close ch — Drain must block after consuming the 5 edges.

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := l.Drain(ctx, ch)
		done <- result{n, err}
	}()

	// Wait until Drain has consumed all 5 buffered edges, then cancel mid-drain.
	// Synchronising on len(ch)==0 (safe to read concurrently with the receiving
	// goroutine) makes the "n >= 5" guarantee deterministic rather than relying
	// on a fixed sleep that flakes under the loaded -race suite's scheduler
	// pressure (the edges are buffered, so the receiver drains them promptly and
	// then blocks on the un-closed channel — exactly the mid-drain state we want
	// to cancel from).
	consumeDeadline := time.Now().Add(2 * time.Second)
	for len(ch) > 0 {
		if time.Now().After(consumeDeadline) {
			t.Fatal("Drain did not consume the buffered edges within 2s")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Drain mid-drain = %v, want context.Canceled", r.err)
		}
		if r.n < 5 {
			t.Fatalf("Drain returned %d, want at least 5 (buffered edges before cancel)", r.n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return within 2s after context cancel")
	}

	if l.Rows() < 5 {
		t.Fatalf("Rows = %d after mid-drain cancel, want >= 5", l.Rows())
	}
}
