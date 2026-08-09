package cypher_test

// begintx_deadline_test.go — rmp #2174 (round-3 comparative audit, finding A1).
//
// Engine.BeginTx acquires two things that could not be cancelled: the engine's
// writer serialisation and the graph's visibility barrier. Neither took a
// context, so a caller's deadline was ignored for the whole wait. The audit
// measured BeginTx with a 50 ms deadline returning after 601 ms in one run and
// after 11.60 s under load — a 232x overrun — and in BOTH cases returning
// err=nil, handing back a live transaction that held the writer semaphore and
// the global barrier on an already-expired context.
//
// That made the Bolt tx_timeout inert at BEGIN, and it contradicted BeginTx's
// own godoc, which promises a prompt error on an elapsed deadline.
//
// The barrier wait is real rather than theoretical: Engine.Run executes an entire
// query inside Graph.View, which holds the barrier's read side, so a writer
// arriving during a slow read queues behind it.
//
// Layer: short.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// A watchdogged BeginTx helper used to live here, because BeginTx could block
// indefinitely on a contended acquisition and a direct call would burn the whole
// package timeout instead of failing with a message. rmp #2305 retired the last
// acquisition BeginTx made, so it cannot block and the helper had no callers left;
// it is deleted rather than kept for a case that no longer exists. The property it
// protected now lives at the statement seam, in
// [TestExplicitTxExec_HonoursDeadlineUnderAnExclusiveBarrierHolder].

// beginTxFixture builds a small engine with a few nodes to read.
func beginTxFixture(t *testing.T) (*lpg.Graph[string, float64], *cypher.Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	res, err := eng.RunAny(context.Background(),
		`UNWIND range(1, 64) AS i CREATE (:P {id: i})`, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("seed result: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
	return g, eng
}

// TestBeginTx_ExpiredContextReturnsError pins the cheapest half: a context that
// has already finished must never yield a transaction.
func TestBeginTx_ExpiredContextReturnsError(t *testing.T) {
	t.Parallel()
	_, eng := beginTxFixture(t)

	for _, tc := range []struct {
		name string
		ctx  func(*testing.T) context.Context
		want error
	}{
		{"cancelled", func(t *testing.T) context.Context {
			c, cancel := context.WithCancel(context.Background())
			cancel()
			return c
		}, context.Canceled},
		{"deadline elapsed", func(t *testing.T) context.Context {
			c, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return c
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := eng.BeginTx(tc.ctx(t))
			if err == nil {
				_ = tx.Rollback()
				t.Fatal("BeginTx returned a transaction on an expired context")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("BeginTx error = %v, want it to wrap %v", err, tc.want)
			}
			if tx != nil {
				t.Fatalf("BeginTx returned a non-nil transaction alongside an error")
			}
		})
	}
}

// TestBeginTx_DoesNotWaitForAnythingAtAll replaced the old "deadline honoured while
// a reader holds the barrier" test.
//
// That test asserted BeginTx returned context.DeadlineExceeded while a
// [lpg.Graph.View] reader held the barrier's read side, because BeginTx took the
// barrier EXCLUSIVELY and so queued behind any reader. rmp #2305 retired that hold:
// BeginTx takes no lock, so it cannot block on a reader, and asserting that it does
// would assert the defect.
//
// What is asserted instead is the stronger property the retirement delivers — BeginTx
// completes PROMPTLY even while a reader holds the barrier — with a ceiling far below
// the reader's hold, so a reintroduced acquisition fails the test rather than merely
// slowing it.
func TestBeginTx_DoesNotWaitForAnythingAtAll(t *testing.T) {
	t.Parallel()
	g, eng := beginTxFixture(t)

	const readerHold = 2 * time.Second

	readerIn := make(chan struct{})
	readerOut := make(chan struct{})
	go func() {
		defer close(readerOut)
		// A READER, as a reader now is: a pinned MVCC snapshot and no lock at
		// all (rmp #2344 removed lpg.Graph.View, which is what this used to
		// hold). Holding it for readerHold is what makes a reintroduced
		// acquisition in BeginTx observable as a delay.
		snap := g.BeginRead()
		defer g.EndRead(snap)
		close(readerIn)
		time.Sleep(readerHold)
	}()
	<-readerIn // a reader now holds an open snapshot

	start := time.Now()
	tx, err := eng.BeginTx(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("BeginTx failed while a reader held the barrier: %v — since rmp #2305 it "+
			"does not interact with the barrier at all", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if elapsed > readerHold/4 {
		t.Fatalf("BeginTx took %v while a reader held the barrier for %v. It is queueing "+
			"behind the reader, so a transaction-lifetime barrier acquisition has been "+
			"reintroduced (rmp #2305 retired it).", elapsed, readerHold)
	}

	<-readerOut
}

// TestExplicitTxExec_HonoursDeadlineUnderAnExclusiveBarrierHolder keeps the deadline
// property the test above used to carry, aimed at the acquisition that still exists.
//
// Since rmp #2305 each Exec takes the schema barrier SHARED for its own duration. A
// shared holder cannot block it, but an EXCLUSIVE one can — a DDL, or any caller
// using [lpg.Graph.ApplyAtomically] / [lpg.Graph.LockBarrier]. When one does, the
// statement must honour the caller's deadline rather than wait out the holder: that
// is the defect rmp #2174 closed for BeginTx, and it must not reappear at the
// statement seam that replaced it.
func TestExplicitTxExec_HonoursDeadlineUnderAnExclusiveBarrierHolder(t *testing.T) {
	t.Parallel()
	g, eng := beginTxFixture(t)

	const budget = 50 * time.Millisecond
	const holderHold = 3 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	// Opened BEFORE the barrier is taken: BEGIN acquires nothing, so it succeeds
	// regardless, and the deadline question belongs to Exec.
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	held := make(chan struct{})
	released := make(chan struct{})
	go func() {
		defer close(released)
		g.LockBarrier()
		close(held)
		time.Sleep(holderHold)
		g.UnlockBarrier()
	}()
	<-held

	start := time.Now()
	_, execErr := tx.Exec("CREATE (:Late {v:1})", nil)
	elapsed := time.Since(start)

	if execErr == nil {
		t.Fatal("Exec succeeded while another goroutine held the schema barrier " +
			"EXCLUSIVELY; its shared acquisition cannot have happened")
	}
	if !errors.Is(execErr, context.DeadlineExceeded) {
		t.Fatalf("Exec error = %v, want it to wrap context.DeadlineExceeded", execErr)
	}
	if elapsed >= holderHold {
		t.Fatalf("Exec waited %v — the holder's full hold; the statement's shared barrier "+
			"acquisition is IGNORING the caller's deadline (rmp #2174's defect at the "+
			"statement seam)", elapsed)
	}
	_ = tx.Rollback()
	<-released
}

// TestBeginTx_SucceedsWhileAnotherTransactionIsOpen is the inversion of the old
// "deadline honoured while another tx holds the writer serialisation" test.
//
// There is no writer serialisation to hold: rmp #2306 retired the engine's writer
// mutex and the store's capacity-one semaphore, and rmp #2305 the barrier hold. A
// second BEGIN while the first is open must therefore SUCCEED — the outcome the old
// test called a failure — and must do so promptly.
func TestBeginTx_SucceedsWhileAnotherTransactionIsOpen(t *testing.T) {
	t.Parallel()
	_, eng := beginTxFixture(t)

	held, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("first BeginTx: %v", err)
	}

	// A short budget on purpose: anything serialising the second BEGIN behind the
	// first expires it and fails the test.
	const budget = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	second, err := eng.BeginTx(ctx)
	if err != nil {
		_ = held.Rollback()
		t.Fatalf("a second BeginTx failed within %v while the first was still open: %v.\n"+
			"Two explicit transactions must be openable at once: concurrency control is "+
			"MVCC alone (rmp #2305, rmp #2306), so BEGIN acquires nothing another "+
			"transaction could be holding.", budget, err)
	}
	if err := second.Rollback(); err != nil {
		t.Fatalf("Rollback of the second: %v", err)
	}
	if err := held.Rollback(); err != nil {
		t.Fatalf("Rollback of the holder: %v", err)
	}
}

// TestBeginTx_UncontendedStillSucceeds guards against over-correction: the
// deadline plumbing must not make an ordinary BeginTx fail or slow down.
func TestBeginTx_UncontendedStillSucceeds(t *testing.T) {
	t.Parallel()
	_, eng := beginTxFixture(t)

	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			cancel()
			t.Fatalf("BeginTx %d on a free engine: %v", i, err)
		}
		if _, err := tx.Exec(`CREATE (:Q {i: 1})`, nil); err != nil {
			_ = tx.Rollback()
			cancel()
			t.Fatalf("Exec %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			cancel()
			t.Fatalf("Commit %d: %v", i, err)
		}
		cancel()
	}
}

// TestBeginTx_ConcurrentContendersAllTerminate is the liveness check: many
// callers with short deadlines contending against a long holder must all return
// — none may hang — and the engine must be fully usable afterwards.
func TestBeginTx_ConcurrentContendersAllTerminate(t *testing.T) {
	t.Parallel()
	_, eng := beginTxFixture(t)

	held, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("holder BeginTx: %v", err)
	}

	const contenders = 24
	var wg sync.WaitGroup
	errs := make([]error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Short, so a reintroduced serialiser expires it rather than merely
			// slowing the test down.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			tx, berr := eng.BeginTx(ctx)
			if berr == nil {
				_ = tx.Rollback()
			}
			errs[i] = berr
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("contenders did not all return; a BeginTx wait is still unbounded")
	}

	// Every contender must have SUCCEEDED. The old form asserted the opposite — that
	// each returned context.DeadlineExceeded because it queued behind the open holder
	// — which is precisely the behaviour rmp #2305 and rmp #2306 retired. A
	// DeadlineExceeded here now means a serialiser has come back.
	for i, e := range errs {
		if e != nil {
			t.Fatalf("contender %d failed with %v while a transaction was open. Every "+
				"contender must be admitted: nothing serialises BEGIN since rmp #2305.", i, e)
		}
	}

	if err := held.Rollback(); err != nil {
		t.Fatalf("holder Rollback: %v", err)
	}
	okCtx, okCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer okCancel()
	tx2, err := eng.BeginTx(okCtx)
	if err != nil {
		t.Fatalf("BeginTx after %d concurrent transactions: %v — one of them leaked "+
			"something", contenders, err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}
