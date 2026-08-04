package txn

// begin_ctx_cancellable_test.go — the store's writer-admission contract.
//
// # What changed, and why these tests were rewritten — rmp #2306
//
// This file used to assert SINGLE-WRITER EXCLUSION: that a second BeginCtx could
// not acquire while another transaction was open. That was the contract of the
// capacity-one semaphore, and retiring it is the point of rmp #2306 — under MVCC
// the concurrency control is the version chain and the conflict predicate, not a
// lock that admits one writer at a time. Asserting exclusion would now assert the
// defect.
//
// What survives, and is asserted harder here, is the property the cancellable
// semaphore acquire existed to provide (rmp #1301, rmp #2174): whenever a writer
// CAN block, it must honour the caller's deadline. There is now exactly one thing
// that blocks a writer — a quiesce ([Store.RunUnderCommitLock]) holding the
// admission gate closed — so that is where the deadline tests point.
//
// Added on top: that independent writers genuinely OVERLAP (the inverse of the
// deleted exclusion test, written as a rendezvous so a reintroduced serialiser
// fails the test instead of merely slowing it), and that both finalisation paths
// deregister the writer — proven by a quiesce completing, which a leaked
// registration would hang forever.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// holdQuiesce runs a quiesce on s in a fresh goroutine and returns once the
// admission gate is provably closed. The returned release closure lets the
// quiesce finish and joins the goroutine; the test MUST call it (via defer) so
// the package-level goleak.VerifyTestMain sees no lingering goroutine.
//
// This replaces the old holdWriter helper, which held the single-writer semaphore
// by opening a transaction. An open transaction no longer blocks anybody.
func holdQuiesce[N comparable, W any](t *testing.T, s *Store[N, W]) (release func()) {
	t.Helper()
	closed := make(chan struct{})
	let := make(chan struct{}) // closed to let the quiesce finish
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.RunUnderCommitLock(func() error {
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

// TestStore_BeginCtx_DeadlineUnderQuiesce is the rmp #1301 / rmp #2174 acceptance
// criterion, restated for the mechanism that can actually block a writer: with a
// quiesce holding the admission gate closed, a BeginCtx carrying a short deadline
// must return a deadline error within roughly that deadline rather than blocking
// for the quiesce's full duration.
func TestStore_BeginCtx_DeadlineUnderQuiesce(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	release := holdQuiesce(t, s)
	defer release()

	const deadline = 50 * time.Millisecond
	// The watchdog must be comfortably longer than the deadline (so a healthy
	// deadline-return is not flagged) yet far shorter than how long the quiesce
	// would otherwise block us. The quiesce is ended only by `release`
	// (deferred), so a non-context-aware admission would block effectively
	// forever; any finite watchdog distinguishes the two behaviours.
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
			// Admitted during a quiesce: the gate is not closing. Roll back so we
			// do not wedge the deferred release.
			_ = r.tx.Rollback()
			t.Fatal("BeginCtx admitted a writer while a quiesce held the gate closed. " +
				"The quiesce would then run its store-wide operation (wal.Close, " +
				"wal.Truncate, a snapshot capture) concurrently with a live transaction.")
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("BeginCtx err = %v, want context.DeadlineExceeded", r.err)
		}
		if r.tx != nil {
			t.Fatal("BeginCtx returned a non-nil Tx alongside an error")
		}
		if r.dt >= watchdog {
			t.Fatalf("BeginCtx returned after %v, want ~%v (deadline)", r.dt, deadline)
		}
	case <-time.After(watchdog):
		t.Fatalf("BeginCtx blocked for more than %v while a quiesce held the gate; "+
			"the admission wait is not context-aware (rmp #2174)", watchdog)
	}
}

// TestStore_BeginCtx_CancelUnderQuiesce is the cancellation analogue: an explicit
// cancel while a quiesce holds the gate must unblock BeginCtx promptly with
// context.Canceled.
func TestStore_BeginCtx_CancelUnderQuiesce(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	release := holdQuiesce(t, s)
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

	// Give the admission a moment to be genuinely parked on the gate, then cancel.
	// A short sleep only strengthens the test, and the watchdog still bounds the
	// assertion.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case r := <-done:
		if r.err == nil {
			_ = r.tx.Rollback()
			t.Fatal("BeginCtx admitted a writer while a quiesce held the gate closed")
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("BeginCtx err = %v, want context.Canceled", r.err)
		}
		if r.tx != nil {
			t.Fatal("BeginCtx returned a non-nil Tx alongside an error")
		}
	case <-time.After(watchdog):
		t.Fatalf("BeginCtx blocked for more than %v after cancel; the admission wait "+
			"is not context-aware", watchdog)
	}
}

// TestStore_ConcurrentWritersAreAdmittedTogether replaces the deleted
// single-writer exclusion test with its inverse, which is the contract rmp #2306
// delivers: independent transactions are open at the SAME TIME.
//
// It is written as a rendezvous, not as a timing measurement. Each writer opens a
// transaction, announces itself, and waits for the others. Under any admission
// serialiser the second writer can never reach its announcement, because the
// first holds admission until it finalises — so the rendezvous cannot complete
// and the test fails on its own deadline. A reintroduced serialiser therefore
// makes this RED, never merely slow.
func TestStore_ConcurrentWritersAreAdmittedTogether(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	const writers = 8
	const settle = 5 * time.Second

	var (
		wg       sync.WaitGroup
		arrived  = make(chan struct{}, writers)
		allIn    = make(chan struct{})
		closeAll sync.Once
		errs     = make([]error, writers)
	)
	wg.Add(writers)
	for w := range writers {
		go func(w int) {
			defer wg.Done()
			tx, err := s.BeginCtx(context.Background())
			if err != nil {
				errs[w] = err
				return
			}
			// Inside the transaction: announce, then wait for every peer to be
			// inside too. Only concurrent admission lets this complete.
			arrived <- struct{}{}
			if len(arrived) == writers {
				closeAll.Do(func() { close(allIn) })
			}
			select {
			case <-allIn:
			case <-time.After(settle):
				errs[w] = errors.New("timed out waiting for peers")
			}
			errs[w] = tx.Rollback()
		}(w)
	}

	select {
	case <-allIn:
	case <-time.After(settle):
		wg.Wait()
		t.Fatalf("%d writers were never all inside a transaction at once. Something is "+
			"serialising admission: since rmp #2306 the store admits writers freely and "+
			"blocks only for a quiesce, so either the single-writer semaphore has been "+
			"reintroduced or another store-wide lock has taken its place.", writers)
	}
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", w, err)
		}
	}
}

// TestStore_CommitAndRollback_DeregisterTheWriter pins the exactly-once pairing of
// [Store.enterWriter] with [Store.exitWriter] on both finalisation paths.
//
// The assertion is a QUIESCE, not a follow-up Begin. A follow-up Begin now
// succeeds whether or not the previous writer deregistered — admission is
// concurrent — so it can no longer detect a leaked registration. A quiesce can:
// it drains the in-flight count to zero, so one un-cleared registration hangs it
// forever. That makes this test the gate for the accounting the whole quiesce
// mechanism rests on.
func TestStore_CommitAndRollback_DeregisterTheWriter(t *testing.T) {
	t.Parallel()

	assertQuiesces := func(t *testing.T, s *Store[string, int64]) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- s.RunUnderCommitLock(func() error { return nil }) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("RunUnderCommitLock: %v", err)
			}
		case <-time.After(5 * time.Second):
			// Deliberately not a Fatal from the goroutine: the drain is parked and
			// reporting the leak is the whole result.
			t.Fatal("a quiesce never drained after the transaction finalised: the " +
				"writer's in-flight registration was not cleared, so RunUnderCommitLock " +
				"waits for a transaction that has already finished")
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
		assertQuiesces(t, s)
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
		assertQuiesces(t, s)
	})

	t.Run("empty commit", func(t *testing.T) {
		t.Parallel()
		s, _, cleanup := openTypedStringStore(t)
		defer cleanup()

		// An empty commit mints no sequence and takes the early-return branch, which
		// is a distinct exit path from the one above.
		if err := s.Begin().Commit(); err != nil {
			t.Fatalf("empty Commit: %v", err)
		}
		assertQuiesces(t, s)
	})
}

// TestStore_QuiescesDoNotOverlap asserts that two concurrent
// [Store.RunUnderCommitLock] calls exclude each other. fn is a store-wide
// operation — wal.Close, wal.Truncate, a snapshot capture — and running two at
// once is exactly what the retired capacity-one semaphore prevented as a side
// effect of serialising everything. The gate has to prevent it deliberately.
func TestStore_QuiescesDoNotOverlap(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openTypedStringStore(t)
	defer cleanup()

	var (
		mu      sync.Mutex
		inside  int
		maxSeen int
		wg      sync.WaitGroup
	)
	const quiesces = 8
	wg.Add(quiesces)
	for range quiesces {
		go func() {
			defer wg.Done()
			_ = s.RunUnderCommitLock(func() error {
				mu.Lock()
				inside++
				if inside > maxSeen {
					maxSeen = inside
				}
				mu.Unlock()
				// Hold long enough that an overlap would be observed rather than
				// missed between two instantaneous calls.
				time.Sleep(2 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()

	if maxSeen != 1 {
		t.Fatalf("%d quiesces ran concurrently, want 1 at a time. Two store-wide "+
			"operations overlapping can close or truncate the WAL under each other.", maxSeen)
	}
}
