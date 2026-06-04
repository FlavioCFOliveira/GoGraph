package txn

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// holdWriter opens a transaction on s (acquiring the single-writer lock)
// in a fresh goroutine and returns once the lock is provably held. The
// returned release closure rolls the transaction back (freeing the lock)
// and joins the goroutine; the test MUST call it (via defer) so the
// package-level goleak.VerifyTestMain sees no lingering goroutine. Using a
// separate goroutine to hold the lock models a concurrent writer and keeps
// the holder's lifetime independent of the goroutine under test.
func holdWriter[N comparable, W any](t *testing.T, s *Store[N, W]) (release func()) {
	t.Helper()
	held := make(chan struct{})
	let := make(chan struct{}) // closed to let the holder release
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tx := s.Begin() // uncancellable acquire; this goroutine owns the lock
		close(held)
		<-let
		_ = tx.Rollback()
	}()
	<-held
	var once sync.Once
	return func() {
		once.Do(func() {
			close(let)
			wg.Wait()
		})
	}
}

// TestStore_BeginCtx_DeadlineUnderContention is the task #1301 acceptance
// criterion: with the single-writer lock held by another goroutine, a
// BeginCtx with a short deadline must return a deadline error within roughly
// that deadline — it must NOT block until the holder releases.
//
// Pre-fix (the store lock was a sync.Mutex and BeginCtx only checked ctx
// before an unconditional Lock) this blocked for the holder's full duration
// and the watchdog fired; post-fix the cancellable semaphore returns the
// context error promptly.
func TestStore_BeginCtx_DeadlineUnderContention(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	release := holdWriter(t, s)
	defer release()

	const deadline = 50 * time.Millisecond
	// The watchdog must be comfortably longer than the deadline (so a healthy
	// deadline-return is not flagged) yet far shorter than how long the holder
	// would otherwise block us. The holder is released only by `release`
	// (deferred), so pre-fix BeginCtx would block effectively forever; any
	// finite watchdog distinguishes the two behaviours.
	const watchdog = 5 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	type result struct {
		tx  *Tx[string, int64]
		err error
		dt  time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		tx, err := s.BeginCtx(ctx)
		done <- result{tx: tx, err: err, dt: time.Since(start)}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			// Acquired despite contention: the lock is not exclusive. Release
			// so we do not deadlock the deferred holder release.
			_ = r.tx.Rollback()
			t.Fatal("BeginCtx acquired the writer while it was held by another goroutine; exclusion broken")
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("BeginCtx err = %v, want context.DeadlineExceeded", r.err)
		}
		if r.tx != nil {
			t.Fatal("BeginCtx returned a non-nil Tx alongside an error")
		}
		// It returned for the right reason; confirm it returned near the
		// deadline rather than after a long block.
		if r.dt >= watchdog {
			t.Fatalf("BeginCtx returned after %v, want ~%v (deadline)", r.dt, deadline)
		}
	case <-time.After(watchdog):
		t.Fatalf("BeginCtx blocked for more than %v under contention; the acquire is not context-aware", watchdog)
	}
}

// TestStore_BeginCtx_CancelUnderContention is the cancellation analogue of
// the deadline test: an explicit cancel while the writer is held must unblock
// BeginCtx promptly with context.Canceled.
func TestStore_BeginCtx_CancelUnderContention(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	release := holdWriter(t, s)
	defer release()

	const watchdog = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())

	type result struct {
		tx  *Tx[string, int64]
		err error
	}
	done := make(chan result, 1)
	go func() {
		tx, err := s.BeginCtx(ctx)
		done <- result{tx: tx, err: err}
	}()

	// Give the acquire a moment to be genuinely blocked on the held lock, then
	// cancel. A short sleep is acceptable here: it only strengthens the test
	// (the acquire is already parked on the semaphore send), and the watchdog
	// still bounds the assertion.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case r := <-done:
		if r.err == nil {
			_ = r.tx.Rollback()
			t.Fatal("BeginCtx acquired the writer while it was held; exclusion broken")
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("BeginCtx err = %v, want context.Canceled", r.err)
		}
		if r.tx != nil {
			t.Fatal("BeginCtx returned a non-nil Tx alongside an error")
		}
	case <-time.After(watchdog):
		t.Fatalf("BeginCtx blocked for more than %v after cancel; the acquire is not context-aware", watchdog)
	}
}

// TestStore_SingleWriter_Exclusion confirms the capacity-one semaphore still
// gives exact mutual exclusion: while one writer holds the lock a second
// BeginCtx (with a non-expiring context) does NOT acquire, and it acquires
// the instant the first writer releases. This guards against the cancellable
// rewrite accidentally weakening the single-writer contract.
func TestStore_SingleWriter_Exclusion(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	release := holdWriter(t, s)

	ctx := context.Background()
	type result struct {
		tx  *Tx[string, int64]
		err error
	}
	done := make(chan result, 1)
	go func() {
		tx, err := s.BeginCtx(ctx) // background ctx: only the release can unblock it
		done <- result{tx: tx, err: err}
	}()

	// While the first writer holds the lock the second acquire must stay
	// blocked. If it returns here, exclusion is broken.
	select {
	case r := <-done:
		if r.tx != nil {
			_ = r.tx.Rollback()
		}
		release()
		t.Fatalf("second BeginCtx returned (tx=%v, err=%v) while the writer was held; exclusion broken", r.tx, r.err)
	case <-time.After(100 * time.Millisecond):
		// Expected: still blocked.
	}

	// Release the first writer; the second acquire must now succeed promptly.
	release()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("second BeginCtx err = %v after the writer released, want nil", r.err)
		}
		if r.tx == nil {
			t.Fatal("second BeginCtx returned nil Tx after the writer released")
		}
		if err := r.tx.Rollback(); err != nil {
			t.Fatalf("Rollback of the second tx: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second BeginCtx did not acquire after the first writer released; lock not handed over")
	}
}

// TestStore_CommitAndRollback_ReleaseTheWriter confirms both finalisation
// paths free the single-writer lock exactly once: after a normal Commit and
// after a normal Rollback, a follow-up Begin must acquire without blocking.
// This pins the release contract the cancellable semaphore depends on (an
// over- or under-release would either let two writers in or wedge the lock).
func TestStore_CommitAndRollback_ReleaseTheWriter(t *testing.T) {
	t.Parallel()

	assertReacquires := func(t *testing.T, s *Store[string, int64]) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			_ = s.Begin().Rollback()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("writer lock was not released; a follow-up Begin blocked")
		}
	}

	t.Run("commit", func(t *testing.T) {
		t.Parallel()
		s, _, cleanup := openTypedStringStore(t)
		defer cleanup()

		tx := s.Begin()
		if err := tx.AddNode("alice"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		assertReacquires(t, s)
	})

	t.Run("rollback", func(t *testing.T) {
		t.Parallel()
		s, _, cleanup := openTypedStringStore(t)
		defer cleanup()

		tx := s.Begin()
		if err := tx.AddNode("bob"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		assertReacquires(t, s)
	})
}
