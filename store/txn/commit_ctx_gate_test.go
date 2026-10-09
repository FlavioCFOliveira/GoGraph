package txn_test

// commit_ctx_gate_test.go — rmp #2985: CommitCtx honours its deadline while the
// graph's visibility gate is held strongly, and the refused commit writes nothing
// to the WAL and applies nothing.

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestCommitCtx_HonoursDeadlineBehindHeldVisibilityGate(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	dir := t.TempDir()
	_, g, st := ioOpen(t, dir)
	setup := st.Begin()
	if err := setup.AddNode("x"); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	before := walBytes(t, dir)

	// The longest-lived strong holder: an embedder's LockBarrier, held from
	// another goroutine until released.
	locked, unlock, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
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
	defer release()

	tx := st.Begin()
	if err := tx.SetNodeProperty("x", "p", lpg.Int64Value(1)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		done <- tx.CommitCtx(ctx)
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		release()
		err = <-done
		t.Fatalf("CommitCtx did not return while the visibility gate was held; it returned %v only after the release", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CommitCtx behind a held visibility gate: err = %v, want context.DeadlineExceeded", err)
	}
	release()
	if after := walBytes(t, dir); after != before {
		t.Fatalf("the refused commit grew the WAL from %d to %d bytes", before, after)
	}
	if _, ok := g.GetNodeProperty("x", "p"); ok {
		t.Fatal("the refused commit left x.p visible")
	}
	// Positive control: a new transaction with the same op commits.
	retry := st.Begin()
	if err := retry.SetNodeProperty("x", "p", lpg.Int64Value(1)); err != nil {
		t.Fatal(err)
	}
	if err := retry.Commit(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if v, ok := g.GetNodeProperty("x", "p"); !ok {
		t.Fatal("retry: x.p absent")
	} else if n, _ := v.Int64(); n != 1 {
		t.Fatalf("retry: x.p = %d, want 1", n)
	}
}
