package lpg

// applydurable_gate_ctx_test.go — rmp #2985: a durable commit honours its context
// while it waits for the visibility gate.
//
// # The defect
//
// [Graph.applyDurableOnce] took the visibility gate shared with the context-free
// WeakLockAuto, although its caller [Graph.ApplyDurable] has a ctx and documents
// that ctx bounds its waits. Every strong holder of the gate — the exclusive write
// brackets [Graph.ApplyAtomically] and [Graph.ApplyAtomicallyTx], the explicit
// barrier [Graph.LockBarrier] / [Graph.LockBarrierCtx] held until
// [Graph.UnlockBarrier], and through them the Cypher engine's index and constraint
// registration with its backfill scan — therefore held a durable commit (the WAL
// commit path, store/txn Tx.CommitCtx) past its deadline for the whole tenure.
//
// # What is asserted
//
// The gate is held strongly by the test through the public LockBarrier, the
// longest-lived holder. A durable commit carrying a 50 ms deadline must return an
// error matching context.DeadlineExceeded BEFORE the barrier is released, having
// run neither apply nor durable, so nothing is claimed and nothing is durable; a
// retry after the release commits, which is the positive control.
//
// The 50 ms deadline is the input under test, not a latency bound. The backstop
// only keeps a regression from hanging the package; reaching it IS the failure.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// durableGateBackstop bounds the wait for a commit that does not honour its
// context, so a regression fails rather than hangs. It is not a latency bound.
const durableGateBackstop = 10 * time.Second

func TestApplyDurable_HonoursDeadlineBehindHeldVisibilityGate(t *testing.T) {
	g := newDirectTxGraph(t)
	requireNoErr(t, g.AddNode("x"))

	// Hold the gate strongly from another goroutine, as an embedder's
	// LockBarrier or a DDL's registration does, and keep it until released.
	locked := make(chan struct{})
	unlock := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		g.LockBarrier()
		close(locked)
		<-unlock
		g.UnlockBarrier()
	}()
	<-locked
	released := false
	release := func() {
		if !released {
			released = true
			close(unlock)
			<-holderDone
		}
	}
	t.Cleanup(release)

	var applyRan, durableRan atomic.Bool
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		done <- g.ApplyDurable(ctx, func(wtx WriteTx) error {
			applyRan.Store(true)
			return g.Writer(wtx).SetNodeProperty("x", "p", Int64Value(1))
		}, func() error {
			durableRan.Store(true)
			return nil
		})
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(durableGateBackstop):
		release()
		err = <-done
		t.Fatalf("ApplyDurable did not return while the visibility gate was held strongly; "+
			"it returned %v only after the release", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ApplyDurable behind a held visibility gate: err = %v, want context.DeadlineExceeded", err)
	}
	if applyRan.Load() || durableRan.Load() {
		t.Fatalf("the refused commit ran apply=%v durable=%v, want neither", applyRan.Load(), durableRan.Load())
	}

	release()
	if _, ok := g.GetNodeProperty("x", "p"); ok {
		t.Fatal("the refused commit left x.p visible")
	}
	// Positive control: the same commit with no deadline succeeds.
	if err := g.ApplyDurable(context.Background(), func(wtx WriteTx) error {
		return g.Writer(wtx).SetNodeProperty("x", "p", Int64Value(2))
	}, func() error { return nil }); err != nil {
		t.Fatalf("retry after the release: %v", err)
	}
	if v, ok := g.GetNodeProperty("x", "p"); !ok {
		t.Fatal("retry after the release: x.p absent")
	} else if n, _ := v.Int64(); n != 2 {
		t.Fatalf("retry after the release: x.p = %d, want 2", n)
	}
}
