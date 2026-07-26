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

// beginTxResult carries the outcome of a watchdogged BeginTx.
type beginTxResult struct {
	tx      *cypher.ExplicitTx
	err     error
	elapsed time.Duration
}

// beginTxWatchdogged calls BeginTx on its own goroutine and fails the test if it
// has not returned within watchdog.
//
// Every contended case here needs this. Before the fix BeginTx waited for the
// holder unconditionally, so a direct call BLOCKS FOREVER on the old behaviour:
// the test would burn the whole package timeout and report itself as a panic
// with a goroutine dump, rather than as a failure naming the defect. The short
// layer also has a 60 s per-package target, which a hanging test destroys. With
// the watchdog the same regression fails in seconds with a message that says
// what happened.
//
// The abandoned BeginTx goroutine is left running on the failure path; the test
// is already failing at that point, and it cannot be joined precisely because
// the wait it is stuck in is the defect.
func beginTxWatchdogged(ctx context.Context, t *testing.T, eng *cypher.Engine, watchdog time.Duration) beginTxResult {
	t.Helper()
	out := make(chan beginTxResult, 1)
	start := time.Now()
	go func() {
		tx, err := eng.BeginTx(ctx)
		out <- beginTxResult{tx: tx, err: err, elapsed: time.Since(start)}
	}()
	select {
	case r := <-out:
		return r
	case <-time.After(watchdog):
		t.Fatalf("BeginTx did not return within %v; its acquisition is ignoring the context "+
			"deadline entirely, which is the defect A1 describes", watchdog)
		return beginTxResult{}
	}
}

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

// TestBeginTx_DeadlineHonouredWhileBarrierHeldByReader is the audit's scenario:
// a reader holds the barrier far longer than the caller's deadline. BeginTx must
// return at its own deadline, not at the reader's release.
//
// The reader is a Graph.View held open for a fixed duration, which is exactly
// what a slow Engine.Run does — Run brackets the whole query in View.
func TestBeginTx_DeadlineHonouredWhileBarrierHeldByReader(t *testing.T) {
	t.Parallel()
	g, eng := beginTxFixture(t)

	const readerHold = 2 * time.Second
	const budget = 50 * time.Millisecond

	readerIn := make(chan struct{})
	readerOut := make(chan struct{})
	go func() {
		defer close(readerOut)
		g.View(func() {
			close(readerIn)
			time.Sleep(readerHold)
		})
	}()
	<-readerIn // the barrier's read side is now held

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	r := beginTxWatchdogged(ctx, t, eng, 5*time.Second)
	tx, err, elapsed := r.tx, r.err, r.elapsed

	if err == nil {
		_ = tx.Rollback()
		t.Fatalf("BeginTx returned a live transaction after %v despite a %v deadline, "+
			"while a reader held the barrier for %v — this is the defect A1 describes",
			elapsed, budget, readerHold)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BeginTx error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed >= readerHold {
		t.Fatalf("BeginTx waited %v — the reader's full hold; the deadline was ignored", elapsed)
	}
	// Generous margin: this asserts an order of magnitude, not scheduler timing.
	if elapsed > budget+500*time.Millisecond {
		t.Fatalf("BeginTx returned after %v, want close to its %v deadline", elapsed, budget)
	}

	<-readerOut
	// The engine must be usable again: nothing may be left holding the barrier or
	// the writer serialisation. A successful transaction here proves both were
	// released, including by the abandoned acquire ctxlock left behind.
	okCtx, okCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer okCancel()
	tx2, err := eng.BeginTx(okCtx)
	if err != nil {
		t.Fatalf("BeginTx after the cancelled attempt: %v — something was left held", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}

// TestBeginTx_DeadlineHonouredWhileWriterHeldByAnotherTx covers the other
// acquisition: the store-less writer serialisation, held by an open transaction.
func TestBeginTx_DeadlineHonouredWhileWriterHeldByAnotherTx(t *testing.T) {
	t.Parallel()
	_, eng := beginTxFixture(t)

	held, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("first BeginTx: %v", err)
	}

	const budget = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	r := beginTxWatchdogged(ctx, t, eng, 5*time.Second)
	tx, err, elapsed := r.tx, r.err, r.elapsed

	if err == nil {
		_ = tx.Rollback()
		_ = held.Rollback()
		t.Fatal("a second BeginTx succeeded while the first still held the writer serialisation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		_ = held.Rollback()
		t.Fatalf("BeginTx error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed > budget+500*time.Millisecond {
		_ = held.Rollback()
		t.Fatalf("BeginTx returned after %v, want close to its %v deadline", elapsed, budget)
	}

	if err := held.Rollback(); err != nil {
		t.Fatalf("Rollback of the holder: %v", err)
	}
	// Free again.
	okCtx, okCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer okCancel()
	tx2, err := eng.BeginTx(okCtx)
	if err != nil {
		t.Fatalf("BeginTx after the holder released: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
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

	for i, e := range errs {
		if e == nil {
			t.Fatalf("contender %d acquired while the holder was open", i)
		}
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Fatalf("contender %d error = %v, want context.DeadlineExceeded", i, e)
		}
	}

	if err := held.Rollback(); err != nil {
		t.Fatalf("holder Rollback: %v", err)
	}
	okCtx, okCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer okCancel()
	tx2, err := eng.BeginTx(okCtx)
	if err != nil {
		t.Fatalf("BeginTx after %d abandoned acquires: %v — one of them leaked the lock",
			contenders, err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}
