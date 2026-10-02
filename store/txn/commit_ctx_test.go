package txn_test

// commit_ctx_test.go — CommitCtx: a commit parked behind another bounded
// commit returns promptly with ctx's error when ctx is cancelled, having
// written nothing to the WAL and applied nothing.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// walBytes is the total size of the files under dir, the WAL lock excluded.
func walBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() != "wal.lock" {
			n += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCommitCtx_CancelledWhileWaitingLogsNothing(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	dir := t.TempDir()
	w, g, st := ioOpen(t, dir)
	setup := st.Begin()
	if err := setup.AddNode("x"); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	before := walBytes(t, dir)

	// A bounded commit holds an uncommitted version of x.p across its durable
	// step until released.
	inB, releaseB, doneB := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		doneB <- g.ApplyDurable(context.Background(), func(wtx lpg.WriteTx) error {
			return g.Writer(wtx).SetNodeProperty("x", "p", lpg.Int64Value(1))
		}, func() error {
			close(inB)
			<-releaseB
			return errors.New("injected fsync failure")
		})
	}()
	<-inB

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx := st.Begin()
	if err := tx.SetNodeProperty("x", "p", lpg.Int64Value(2)); err != nil {
		t.Fatal(err)
	}
	commitDone := make(chan error, 1)
	go func() { commitDone <- tx.CommitCtx(ctx) }()
	// Give the commit time to meet the claim and park; the assertions below
	// hold whether it parked or saw the cancellation first.
	time.Sleep(20 * time.Millisecond)
	cancelled := time.Now()
	cancel()
	select {
	case err := <-commitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CommitCtx returned %v, want an error wrapping context.Canceled", err)
		}
		if elapsed := time.Since(cancelled); elapsed > 250*time.Millisecond {
			t.Errorf("CommitCtx took %v to return after cancellation", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("CommitCtx did not return after cancellation")
	}
	if after := walBytes(t, dir); after != before {
		t.Errorf("the cancelled commit grew the WAL from %d to %d bytes", before, after)
	}

	close(releaseB)
	if err := <-doneB; err == nil {
		t.Fatal("blocker: want its injected fsync failure")
	}
	if _, ok := g.GetNodeProperty("x", "p"); ok {
		t.Error("x.p is present: the cancelled commit or the failed blocker left a value")
	}
	if err := tx.Commit(); !errors.Is(err, txn.ErrTxFinished) {
		t.Errorf("Commit after a cancelled CommitCtx returned %v, want ErrTxFinished", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	mem := ioDump(g)
	if got := ioRecover(t, dir); got != mem {
		t.Errorf("recovered state differs from memory:\n--- recovered ---\n%s--- memory ---\n%s", got, mem)
	}
}
