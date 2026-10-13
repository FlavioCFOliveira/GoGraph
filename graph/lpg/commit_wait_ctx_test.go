package lpg

// commit_wait_ctx_test.go — a durable commit parked behind another bounded
// commit returns promptly with ctx's error when ctx is cancelled, and leaves
// the wait table, the hand-off tokens and the goroutine set as it found them.

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// parkedWaiters counts the waiters queued in t that have not given up.
func parkedWaiters(t *txWaitTable) int {
	n := 0
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for _, q := range sh.m {
			for w := q.head; w != nil; w = w.next {
				if w.state.Load() == waiterWaiting {
					n++
				}
			}
		}
		sh.mu.Unlock()
	}
	return n
}

// inFlightEntries counts the bounded transactions entered in t.
func inFlightEntries(t *txWaitTable) int {
	n := 0
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

func TestApplyDurable_CancelledWaiterReturnsPromptly(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	g := newDirectTxGraph(t)
	requireNoErr(t, g.AddNode("x"))
	release, blockerDone := holdDurable(t, g, func() error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	durableRan := make(chan struct{}, 1)
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- g.ApplyDurable(ctx, func(wtx WriteTx) error {
			return g.Writer(wtx).SetNodeProperty("x", "p", Int64Value(2))
		}, func() error {
			durableRan <- struct{}{}
			return nil
		})
	}()

	// Construct the precondition: the waiter is parked on the blocker's queue.
	parkDeadline := time.Now().Add(directWaitBudget / 2)
	for parkedWaiters(&g.txWait) != 1 {
		if time.Now().After(parkDeadline) {
			t.Fatal("the waiter never parked behind the blocking commit")
		}
		time.Sleep(time.Millisecond)
	}

	cancelled := time.Now()
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter returned %v, want an error wrapping context.Canceled", err)
		}
		if elapsed := time.Since(cancelled); elapsed > directWaitBudget/4 {
			t.Errorf("cancelled waiter took %v to return, want well under the %v budget", elapsed, directWaitBudget)
		}
	case <-time.After(directWaitBudget / 2):
		t.Fatal("cancelled waiter did not return before half the wait budget")
	}
	select {
	case <-durableRan:
		t.Fatal("the cancelled commit ran its durable step")
	default:
	}

	close(release)
	if err := <-blockerDone; err != nil {
		t.Fatalf("blocker: %v", err)
	}
	// The blocker's value is the committed one: the cancelled commit applied
	// nothing that survived.
	if v, ok := g.GetNodeProperty("x", "p"); !ok || v != Int64Value(1) {
		t.Fatalf("x.p = %v (present %v), want the blocker's 1", v, ok)
	}
	if n := inFlightEntries(&g.txWait); n != 0 {
		t.Errorf("%d bounded transactions still entered in the wait table, want 0", n)
	}
	if n := g.txWait.handoffs.Load(); n != 0 {
		t.Errorf("%d hand-off tokens outstanding, want 0", n)
	}
	// The object is writable again at once: nothing was left claimed.
	if err := g.ApplyDurable(context.Background(), func(wtx WriteTx) error {
		return g.Writer(wtx).SetNodeProperty("x", "p", Int64Value(3))
	}, func() error { return nil }); err != nil {
		t.Fatalf("commit after the cancelled one: %v", err)
	}
}

func TestApplyDurable_DoneContextRunsNothing(t *testing.T) {
	g := newDirectTxGraph(t)
	requireNoErr(t, g.AddNode("x"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := g.ApplyDurable(ctx, func(WriteTx) error { ran = true; return nil }, func() error { ran = true; return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ApplyDurable with a done ctx returned %v, want context.Canceled", err)
	}
	if ran {
		t.Fatal("ApplyDurable with a done ctx ran apply or durable")
	}
}
