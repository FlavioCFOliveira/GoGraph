package cypher_test

// runintx_ctx_contention_test.go — task #1301 acceptance criterion at the
// engine boundary: Engine.RunInTx must honour its context's deadline while
// the store's single-writer lock is held by another writer, instead of
// blocking on the lock for the holder's full duration.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// newWALStoreEngineWithStore is newWALStoreEngine plus a handle to the
// underlying store, so a test can hold the store's single-writer lock
// directly (by opening a txn.Tx) to create write contention against the
// engine.
func newWALStoreEngineWithStore(t *testing.T) (*cypher.Engine, *txn.Store[string, float64]) {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return cypher.NewEngineWithStore(store), store
}

// TestRunInTx_HonoursDeadlineUnderWriteContention is the task #1301
// acceptance criterion: with the store's single-writer lock held by another
// goroutine (an open, uncommitted txn.Tx), Engine.RunInTx called with a
// 50 ms-deadline context must return a deadline error within roughly that
// deadline — NOT block until the holder releases.
//
// Pre-fix RunInTx acquired the lock via the non-cancellable Store.Begin and
// blocked for the holder's full duration; post-fix it acquires via the
// context-aware Store.BeginCtx and returns context.DeadlineExceeded promptly.
func TestRunInTx_HonoursDeadlineUnderWriteContention(t *testing.T) {
	t.Parallel()
	eng, store := newWALStoreEngineWithStore(t)

	// Goroutine A holds the single-writer lock by opening a transaction and
	// not committing it. It is released (rolled back) and joined via the
	// deferred closure so no goroutine outlives the test (goleak).
	held := make(chan struct{})
	let := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tx := store.Begin()
		close(held)
		<-let
		_ = tx.Rollback()
	}()
	<-held
	defer func() {
		close(let)
		wg.Wait()
	}()

	const deadline = 50 * time.Millisecond
	const watchdog = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	type result struct {
		res *cypher.Result
		err error
		dt  time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		res, err := eng.RunInTx(ctx, "CREATE (:N {v: 1})", nil)
		done <- result{res: res, err: err, dt: time.Since(start)}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			// Acquired despite contention: close the result so the writer is
			// released and the deferred holder release does not deadlock.
			_ = r.res.Close()
			t.Fatal("RunInTx committed a write while the single-writer lock was held; exclusion broken")
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("RunInTx err = %v, want context.DeadlineExceeded", r.err)
		}
		if r.res != nil {
			t.Fatal("RunInTx returned a non-nil Result alongside an error")
		}
		if r.dt >= watchdog {
			t.Fatalf("RunInTx returned after %v, want ~%v (deadline)", r.dt, deadline)
		}
	case <-time.After(watchdog):
		t.Fatalf("RunInTx blocked for more than %v under write contention; the engine write path is not context-aware", watchdog)
	}
}

// TestRunInTx_AcquiresAfterHolderReleases is the liveness companion: once the
// contending holder releases the single-writer lock, a RunInTx with a
// generous deadline must acquire and commit successfully. This confirms the
// cancellable rewrite still hands the lock over (a failed/cancelled acquire
// must not have consumed a semaphore token).
func TestRunInTx_AcquiresAfterHolderReleases(t *testing.T) {
	t.Parallel()
	eng, store := newWALStoreEngineWithStore(t)

	tx := store.Begin() // hold the writer

	type result struct {
		res *cypher.Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		// A generous deadline: long enough to outlast the short hold below, so
		// the acquire should succeed rather than time out.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := eng.RunInTx(ctx, "CREATE (:N {v: 1})", nil)
		done <- result{res: res, err: err}
	}()

	// Keep the lock held briefly so RunInTx is genuinely contended, then
	// release it.
	time.Sleep(50 * time.Millisecond)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback of the holding tx: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("RunInTx err = %v after the holder released, want nil", r.err)
		}
		for r.res.Next() { //nolint:revive // drain the (empty) result set
		}
		if err := r.res.Err(); err != nil {
			t.Fatalf("Result.Err: %v", err)
		}
		if err := r.res.Close(); err != nil {
			t.Fatalf("Result.Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunInTx did not acquire the writer after the holder released")
	}
}
