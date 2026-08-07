package cypher_test

// runintx_ctx_contention_test.go — task #1301 / rmp #2174 at the engine boundary,
// restated for the mechanism that can actually block a write.
//
// # Why these tests changed — rmp #2306
//
// They used to hold the store's capacity-one semaphore (by opening an
// uncommitted txn.Tx) and assert that Engine.RunInTx could not commit while it was
// held. That semaphore is retired: concurrency control is MVCC alone, so an open
// transaction blocks nobody, and the old assertion would now assert the defect.
//
// The property worth gating survives and is gated below: whenever a write CAN
// block, it must honour the caller's deadline. There is exactly one thing left
// that blocks a write at this seam — a store quiesce
// (txn.Store.RunUnderCommitLock) holding the admission gate closed — so that is
// what these tests contend against.

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

// newWALStoreEngineWithStore is newWALStoreEngine plus a handle to the underlying
// store, so a test can drive the store directly — open a transaction, or hold a
// quiesce — to contend against the engine.
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

// holdStoreQuiesce closes the store's writer-admission gate and returns once it is
// provably closed. The returned release closure ends the quiesce and joins the
// goroutine; the caller MUST invoke it so goleak sees no lingering goroutine.
func holdStoreQuiesce(t *testing.T, store *txn.Store[string, float64]) (release func()) {
	t.Helper()
	closed := make(chan struct{})
	let := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = store.RunUnderCommitLock(func() error {
			close(closed)
			<-let
			return nil
		})
	}()
	<-closed
	var once sync.Once
	return func() {
		once.Do(func() {
			close(let)
			wg.Wait()
		})
	}
}

// TestRunInTx_HonoursDeadlineUnderQuiesce is the task #1301 / rmp #2174 acceptance
// criterion: with a store quiesce holding the admission gate closed,
// Engine.RunInTx called with a 50 ms-deadline context must return a deadline error
// within roughly that deadline rather than blocking for the quiesce's full
// duration.
//
// The pre-fix defect it guards against is unchanged in kind: an acquire that
// ignores the caller's context blocks for the holder's tenure, measured at ten
// minutes against a 200 ms deadline before rmp #2174. Only the holder changed,
// from a writer to a quiesce.
func TestRunInTx_HonoursDeadlineUnderQuiesce(t *testing.T) {
	t.Parallel()
	eng, store := newWALStoreEngineWithStore(t)

	release := holdStoreQuiesce(t, store)
	defer release()

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
			// Admitted during a quiesce: close the result so the deferred release
			// does not deadlock, then report it.
			_ = r.res.Close()
			t.Fatal("RunInTx committed a write while a store quiesce held the admission " +
				"gate closed. The quiesce would then close or truncate the WAL underneath " +
				"a live commit.")
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
		t.Fatalf("RunInTx blocked for more than %v while a quiesce held the gate; the "+
			"engine write path is not context-aware (task #1301, rmp #2174)", watchdog)
	}
}

// TestRunInTx_AcquiresAfterQuiesceEnds is the liveness companion: once the quiesce
// ends, a RunInTx with a generous deadline must be admitted and commit
// successfully. This confirms the gate is genuinely reopened and that an
// abandoned, deadline-expired admission left nothing behind.
func TestRunInTx_AcquiresAfterQuiesceEnds(t *testing.T) {
	t.Parallel()
	eng, store := newWALStoreEngineWithStore(t)

	release := holdStoreQuiesce(t, store)

	type result struct {
		res *cypher.Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		// A generous deadline: long enough to outlast the short quiesce below, so
		// the admission should succeed rather than time out.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := eng.RunInTx(ctx, "CREATE (:N {v: 1})", nil)
		done <- result{res: res, err: err}
	}()

	// Keep the gate closed briefly so RunInTx is genuinely parked, then reopen it.
	time.Sleep(50 * time.Millisecond)
	release()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("RunInTx err = %v after the quiesce ended, want nil", r.err)
		}
		for r.res.Next() { //nolint:revive // drain the (empty) result set
		}
		if err := r.res.Err(); err != nil {
			t.Fatalf("Result.Err: %v", err)
		}
		if err := r.res.Close(); err != nil {
			t.Fatalf("Result.Close: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunInTx was never admitted after the quiesce ended; the admission gate " +
			"was not reopened")
	}
}

// TestRunInTx_CommitsWhileAnotherTransactionIsOpen is the assertion that replaces
// the deleted exclusion test, and it states the contract rmp #2306 delivers: an
// open, uncommitted store transaction does NOT block an engine write.
//
// The old test held exactly this situation and called a successful RunInTx
// "exclusion broken". It is now the required outcome — concurrency control is the
// version chain and the conflict predicate, not a lock — so the same scenario is
// kept with its verdict inverted, which is the clearest possible record of what
// changed.
func TestRunInTx_CommitsWhileAnotherTransactionIsOpen(t *testing.T) {
	t.Parallel()
	eng, store := newWALStoreEngineWithStore(t)

	// An open, uncommitted transaction on a DISJOINT key, so nothing the engine
	// writes can legitimately conflict with it.
	other := store.Begin()
	if err := other.AddNode("held-by-other"); err != nil {
		t.Fatalf("AddNode on the open transaction: %v", err)
	}
	defer func() { _ = other.Rollback() }()

	const watchdog = 5 * time.Second
	done := make(chan error, 1)
	go func() {
		res, err := eng.RunInTx(context.Background(), "CREATE (:N {v: 1})", nil)
		if err != nil {
			done <- err
			return
		}
		for res.Next() { //nolint:revive // drain the (empty) result set
		}
		if rerr := res.Err(); rerr != nil {
			done <- rerr
			return
		}
		done <- res.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunInTx failed while another transaction was open: %v.\n"+
				"An open transaction on a disjoint key must neither block nor conflict "+
				"with an engine write.", err)
		}
	case <-time.After(watchdog):
		t.Fatalf("RunInTx did not complete within %v while another transaction was open. "+
			"Something is serialising writers: rmp #2306 retired the store's "+
			"capacity-one semaphore, so an open transaction must not block a write.", watchdog)
	}
}
