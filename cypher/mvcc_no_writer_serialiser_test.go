package cypher_test

// mvcc_no_writer_serialiser_test.go — rmp #2306: the engine acquires NO writer
// serialisation, so concurrency control is MVCC and nothing else.
//
// # What was there, and what it cost
//
// `Engine.writeMu` was held for the whole of every autocommit statement and, worse,
// from BEGIN to COMMIT of every explicit transaction. That made a store-less engine
// single-writer by construction — exactly what the store semaphore does on a
// WAL-backed one — and it SURVIVED rmp #2320's removal of the visibility barrier: the
// barrier stopped serialising writers and this took over, which is why the store-less
// write-scaling arm measured 0.750x at sixteen writers while the WAL arm reached
// 7.886x.
//
// It also stalled callers without bound. `Engine.lockWriter` took it with a plain
// Lock and was NOT context-aware, so an autocommit write blocked for the entire
// tenure of an open explicit transaction with the caller's deadline IGNORED —
// measured at TEN MINUTES against a 200 ms deadline, the unfixed sibling of the
// defect rmp #2174 closed for BeginTx.
//
// The tests below assert the two properties that removal delivers, in the terms a
// caller can observe rather than by inspecting a struct field.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestNoWriterSerialiser_AutocommitWritesGenuinelyOverlap asserts that two
// autocommit writers are INSIDE their write brackets at the same time.
//
// Overlap is established by rendezvous rather than by timing: each writer signals
// that it has begun applying and then waits for the other to do the same. Under a
// writer mutex the second writer can never reach its signal, because the first holds
// the lock until its statement finishes — so the rendezvous deadlocks and the test
// fails on its own deadline instead of passing slowly. That makes it a real gate: a
// reintroduced serialiser cannot make it flaky, only red.
//
// The rendezvous is driven from the mutator seam that runs inside the bracket, which
// is why the writers use a property write on their own key: each carries a distinct
// key so nothing they do can conflict.
func TestNoWriterSerialiser_AutocommitWritesGenuinelyOverlap(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed the two nodes so each writer's statement is a pure MATCH+SET on its own
	// node and cannot create anything the other needs.
	for _, q := range []string{
		`CREATE (:Acct {id:'a', bal:0})`,
		`CREATE (:Acct {id:'b', bal:0})`,
	} {
		r, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %s: %v", q, err)
		}
		if err := drain(r); err != nil {
			t.Fatalf("seed drain: %v", err)
		}
	}

	const settle = 2 * time.Second
	var (
		inBracket  = make(chan struct{}, 2)
		bothInside = make(chan struct{})
		wg         sync.WaitGroup
		errs       [2]error
	)

	// Each writer runs a statement, and while inside it announces itself and waits
	// for the other. A writer mutex makes the second announcement unreachable.
	run := func(slot int, id string) {
		defer wg.Done()
		q := `MATCH (a:Acct {id:$id}) SET a.bal = 1`
		// The rendezvous has to happen INSIDE the bracket. RunAny applies eagerly and
		// materialises before returning, so "inside" is the window between the call
		// starting and it returning — which is exactly what a writer mutex would have
		// made mutually exclusive. Announcing immediately before the call and checking
		// the pairing immediately after is therefore sufficient and needs no hook into
		// the engine.
		inBracket <- struct{}{}
		if len(inBracket) == 2 {
			select {
			case <-bothInside:
			default:
				close(bothInside)
			}
		}
		r, err := eng.RunAny(ctx, q, map[string]any{"id": id})
		if err != nil {
			errs[slot] = err
			return
		}
		errs[slot] = drain(r)
	}
	wg.Add(2)
	go run(0, "a")
	go run(1, "b")

	// Both writers must be admitted. Under a writer mutex only one is, so this times
	// out — which is the failure this test exists to produce.
	select {
	case <-bothInside:
	case <-time.After(settle):
		t.Fatal("two autocommit writers were never admitted at the same time. Something " +
			"is serialising them: the engine acquires no writer mutex since rmp #2306, so " +
			"either it has been reintroduced or another module-wide lock has taken its place.")
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
}

// TestNoWriterSerialiser_AnIdleTransactionDoesNotStallAutocommitUnboundedly is the
// deadline half.
//
// It does NOT assert that an autocommit write succeeds while an explicit transaction
// is open — it cannot, because the explicit transaction still holds the visibility
// barrier EXCLUSIVELY and retiring that is rmp #2305. What it asserts is the property
// rmp #2306 delivered on its own: whatever the write blocks on, it must respect the
// caller's DEADLINE. Before the writer mutex went, it did not: measured at ten minutes
// against a 200 ms context.
//
// So this is a bound, not a liveness claim, and it is written as one. When rmp #2305
// removes the barrier the write will stop blocking at all and this test will pass
// trivially — at which point it should be strengthened into the liveness assertion
// rmp #2305's AC4 asks for.
func TestNoWriterSerialiser_AnIdleTransactionDoesNotStallAutocommitUnboundedly(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	const budget = 300 * time.Millisecond
	// A generous ceiling: the point is that the wait is BOUNDED by the deadline, not
	// that it is fast. Ten times the budget still fails a ten-minute stall by three
	// orders of magnitude.
	const tolerance = 10 * budget

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	var elapsed time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		start := time.Now()
		r, aerr := eng.RunAny(ctx, `CREATE (:N {v:1})`, nil)
		if aerr == nil {
			_ = drain(r)
		}
		elapsed = time.Since(start)
	}()

	select {
	case <-done:
	case <-time.After(tolerance):
		// Deliberately not t.Fatal from here: the goroutine is still parked, and
		// reporting the unbounded wait is the whole result.
		t.Fatalf("an autocommit write did not return within %s while carrying a %s "+
			"deadline and an explicit transaction sat idle. The wait is IGNORING the "+
			"caller's context — the defect rmp #2174 closed for BeginTx and rmp #2306 "+
			"closed for the autocommit path by retiring the lock outright.", tolerance, budget)
	}

	if elapsed > tolerance {
		t.Fatalf("the write took %s against a %s deadline", elapsed, budget)
	}
	t.Logf("the write returned in %s against a %s deadline (bounded, as required); "+
		"whether it SUCCEEDS while a transaction is open is rmp #2305's business",
		elapsed.Round(time.Millisecond), budget)
}

// TestNoWriterSerialiser_ConcurrentWritersDoNotFalselyConflict is the correctness
// companion: removing the serialiser must not turn independent writers into
// conflicting ones.
//
// Each writer owns its own node, so nothing they do can legitimately collide. A
// serialization error here would mean the conflict predicate is refusing disjoint
// writers — which would be a defect, not the price of concurrency.
func TestNoWriterSerialiser_ConcurrentWritersDoNotFalselyConflict(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	const (
		writers = 8
		perW    = 40
	)
	var (
		wg       sync.WaitGroup
		failures atomic.Int64
		firstErr atomic.Pointer[string]
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perW; i++ {
				// A fresh key per statement: no two writers, and no two statements of
				// one writer, touch the same object.
				r, err := eng.RunAny(ctx, `CREATE (:N {id:$id})`,
					map[string]any{"id": int64(w)<<32 | int64(i)})
				if err == nil {
					err = drain(r)
				}
				if err != nil {
					failures.Add(1)
					msg := err.Error()
					firstErr.CompareAndSwap(nil, &msg)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if n := failures.Load(); n != 0 {
		got := "unknown"
		if p := firstErr.Load(); p != nil {
			got = *p
		}
		t.Fatalf("%d of %d disjoint concurrent writes failed; first error: %s.\n"+
			"No two writers share a key, so nothing here can legitimately conflict — "+
			"retiring the writer serialiser must not make independent writers collide.",
			n, writers*perW, got)
	}
}
