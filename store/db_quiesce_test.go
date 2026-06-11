package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// quiesceStack stands up the minimal WithQuiesce wiring: a WAL, a codec-only
// store on it, and a composed DB whose Close drains in-flight writers through
// the store's commit lock. No checkpointer — these tests isolate step 3 of
// the teardown (the WAL close), which is where the quiesce hook acts.
func quiesceStack(t *testing.T) (dir string, st *txn.Store[string, int64], db *store.DB) {
	t.Helper()
	dir = t.TempDir()
	wlog, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	st = txn.NewStoreWithCodec(g, wlog, txn.NewStringCodec())
	db = store.New(wlog, store.WithQuiesce(st.RunUnderCommitLock))
	return dir, st, db
}

// TestDB_CloseQuiescesInFlightWriters is the gate test for the composed-Close
// writer quiesce: with concurrent committers in flight, DB.Close must not race
// wal.Close against an in-flight CommitWALOnly. The acknowledged/durable sets
// must coincide exactly — a commit acknowledged with nil is in the WAL, and a
// commit rejected with wal.ErrWriterClosed is not.
//
// Before the fix, closeOnce0 closed the WAL without holding the store's commit
// lock, so Close could flush+fsync the frames of a transaction whose
// CommitWALOnly then failed on the closed writer (durable but unacknowledged),
// or interleave with a commit's append/sync arbitrarily. With the fix
// (WithQuiesce(st.RunUnderCommitLock)) the close waits for the in-flight
// commit and excludes new ones, so the WAL holds one op per acknowledged
// commit, no more and no fewer — asserted by replaying it with recovery.Open.
func TestDB_CloseQuiescesInFlightWriters(t *testing.T) {
	t.Parallel()
	dir, st, db := quiesceStack(t)

	const writers = 16
	var acked atomic.Int64
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := range writers {
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				tx := st.Begin()
				src := fmt.Sprintf("w%d-src%d", w, i)
				dst := fmt.Sprintf("w%d-dst%d", w, i)
				if err := tx.AddEdge(src, dst, 0); err != nil {
					_ = tx.Rollback()
					t.Errorf("writer %d: AddEdge: %v", w, err)
					return
				}
				err := tx.CommitWALOnly()
				switch {
				case err == nil:
					acked.Add(1)
				case errors.Is(err, wal.ErrWriterClosed):
					// The WAL is closed: the clean post-close rejection. Stop.
					return
				default:
					t.Errorf("writer %d: CommitWALOnly: %v", w, err)
					return
				}
			}
		}(w)
	}

	// Let the writers genuinely be mid-commit before tearing down, so the
	// close really overlaps in-flight traffic rather than an idle store.
	deadline := time.Now().Add(2 * time.Second)
	for acked.Load() < 2*writers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if acked.Load() == 0 {
		t.Fatal("no transaction was acknowledged before Close; the race window was never exercised")
	}

	if err := db.CloseCtx(context.Background()); err != nil {
		t.Fatalf("db.CloseCtx: %v", err)
	}
	wg.Wait()

	// Every acknowledged commit — and ONLY acknowledged commits — must be in
	// the WAL. Each transaction carries exactly one op, so the replayed op
	// count equals the acknowledged-commit count. WALOps > acked would mean
	// Close made durable a commit whose CommitWALOnly returned an error;
	// WALOps < acked would mean an acknowledged commit was lost.
	res, err := recovery.Open[string, int64](dir,
		recovery.Options[string, int64]{Codec: txn.NewStringCodec()})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if got, want := int64(res.WALOps), acked.Load(); got != want {
		t.Fatalf("recovered WAL ops = %d; want %d (exactly one per acknowledged commit)", got, want)
	}
}

// TestDB_CloseBlocksUntilInFlightCommitReleases is the deterministic ordering
// gate: with an in-flight writer holding the store's commit lock (the state a
// transaction is in between Begin and Commit), db.CloseCtx must BLOCK until
// the writer releases, and only then close the WAL.
//
// Before the fix, Close did not block — it closed the WAL out from under the
// lock holder (the select below would observe Close returning while the lock
// was still held). With the fix, Close acquires the same commit lock via the
// WithQuiesce callback, so it parks until the holder releases; the released
// flag then proves the ordering: the holder finished strictly before Close
// completed.
func TestDB_CloseBlocksUntilInFlightCommitReleases(t *testing.T) {
	t.Parallel()
	_, st, db := quiesceStack(t)

	lockHeld := make(chan struct{})
	releaseLock := make(chan struct{})
	var released atomic.Bool
	holderDone := make(chan error, 1)
	go func() {
		// Simulates a long in-flight commit: RunUnderCommitLock takes the SAME
		// single-writer semaphore a transaction holds from Begin to Commit.
		holderDone <- st.RunUnderCommitLock(func() error {
			close(lockHeld)
			<-releaseLock
			released.Store(true)
			return nil
		})
	}()
	<-lockHeld

	closeDone := make(chan error, 1)
	go func() { closeDone <- db.CloseCtx(context.Background()) }()

	// Pre-fix failure mode: Close returns while the commit lock is held.
	select {
	case err := <-closeDone:
		t.Fatalf("db.CloseCtx returned (err=%v) while an in-flight commit held the lock; Close must quiesce writers", err)
	case <-time.After(50 * time.Millisecond):
		// Still blocked — Close is waiting on the commit lock, as required.
	}

	close(releaseLock)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("db.CloseCtx after writer release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("db.CloseCtx did not return after the in-flight commit released the lock")
	}
	if !released.Load() {
		t.Fatal("db.CloseCtx completed before the in-flight commit released the commit lock")
	}
	if err := <-holderDone; err != nil {
		t.Fatalf("RunUnderCommitLock: %v", err)
	}

	// After the quiesced close, a new commit is cleanly rejected with the
	// post-close sentinel rather than racing a half-closed writer.
	tx := st.Begin()
	if err := tx.AddEdge("a", "b", 0); err != nil {
		_ = tx.Rollback()
		t.Fatalf("AddEdge after Close: %v", err)
	}
	if err := tx.CommitWALOnly(); !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("CommitWALOnly after quiesced Close = %v; want wal.ErrWriterClosed", err)
	}
}
