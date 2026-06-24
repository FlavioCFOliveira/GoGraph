package sim

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestReadTx_RejectsWrites asserts a read-only explicit transaction
// ([cypher.Engine.BeginReadTx]) rejects every writing/DDL statement with the
// typed [cypher.ErrWriteInReadOnlyTx], applies nothing, and still serves reads.
func TestReadTx_RejectsWrites(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	if _, err := eng.RunInTxAny(ctx, "CREATE (:N {id:1})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := eng.BeginReadTx(ctx)
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, w := range []string{
		"CREATE (:N {id:2})",
		"MATCH (n:N {id:1}) SET n.x = 1",
		"MATCH (n:N {id:1}) DETACH DELETE n",
		"CREATE INDEX ix FOR (n:N) ON (n.id)",
	} {
		if _, werr := tx.ExecAny(w, nil); !errors.Is(werr, cypher.ErrWriteInReadOnlyTx) {
			t.Fatalf("read-tx write %q: err=%v, want ErrWriteInReadOnlyTx", w, werr)
		}
	}

	// Reads still work inside the read-only tx.
	res, rerr := tx.ExecAny("MATCH (n:N) RETURN count(n)", nil)
	if rerr != nil {
		t.Fatalf("read inside read-tx: %v", rerr)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("read drain: %v", err)
	}
	_ = res.Close()

	// The rejected writes must have applied nothing: the count is still 1.
	if got := nodeCountViaEngine(t, eng); got != 1 {
		t.Fatalf("rejected read-tx writes mutated state: count=%d, want 1", got)
	}
}

// TestReadTx_NoDirtyReads is the isolation test: a writer commits nodes in atomic
// batches of 5, while concurrent read-only transactions repeatedly count them.
// The engine's visibility barrier guarantees a transaction's writes flip visible
// as one step, so every observed count MUST be a multiple of 5 — observing an
// intermediate value would be a dirty/partial read (an Isolation breach). It is a
// concurrent test (not bit-reproducible) guarded by goleak and a deadline.
func TestReadTx_NoDirtyReads(t *testing.T) {
	defer goleak.VerifyNone(t)
	const batch = 5
	const batches = 200
	const readers = 6

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	createBatch := "CREATE (:N),(:N),(:N),(:N),(:N)" // 5 nodes, one atomic transaction

	var dirty atomic.Int64 // a count not divisible by batch (an isolation breach)
	var readErr atomic.Pointer[error]
	var wg sync.WaitGroup
	done := make(chan struct{})

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for {
				select {
				case <-done:
					return
				default:
				}
				tx, err := eng.BeginReadTx(ctx)
				if err != nil {
					e := err
					readErr.CompareAndSwap(nil, &e)
					return
				}
				res, err := tx.ExecAny("MATCH (n:N) RETURN count(n)", nil)
				if err != nil {
					_ = tx.Rollback()
					e := err
					readErr.CompareAndSwap(nil, &e)
					return
				}
				var c int64
				if res.Next() {
					c = scalarIntOf(res)
				}
				_ = res.Close()
				_ = tx.Rollback()
				if c%batch != 0 {
					dirty.Store(c) // partial-transaction read observed
				}
			}
		}()
	}

	ctx := context.Background()
	for i := 0; i < batches; i++ {
		if _, err := eng.RunInTxAny(ctx, createBatch, nil); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("writer batch %d: %v", i, err)
		}
	}
	close(done)

	waitWG(t, &wg, 30*time.Second)

	if ep := readErr.Load(); ep != nil {
		t.Fatalf("read-only transaction errored under contention: %v", *ep)
	}
	if d := dirty.Load(); d != 0 {
		t.Fatalf("DIRTY READ: a read-only tx observed count=%d, not a multiple of %d (a partial transaction leaked across the isolation barrier)", d, batch)
	}
	if got := nodeCountViaEngine(t, eng); got != batch*batches {
		t.Fatalf("final node count = %d, want %d", got, batch*batches)
	}
}

// nodeCountViaEngine reads MATCH (n) RETURN count(n) through the engine.
func nodeCountViaEngine(t *testing.T, eng *cypher.Engine) int64 {
	t.Helper()
	res, err := eng.RunAny(context.Background(), "MATCH (n) RETURN count(n)", nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	defer func() { _ = res.Close() }()
	var c int64
	if res.Next() {
		c = scalarIntOf(res)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("count drain: %v", err)
	}
	return c
}

// scalarIntOf reads the first column of the current row as an int64.
func scalarIntOf(res *cypher.Result) int64 {
	if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
		return int64(iv)
	}
	return -1
}

// waitWG waits for wg with a deadline, failing the test on a hang (a deadlock in
// the read path would otherwise hang the whole run).
func waitWG(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("read-only transactions did not drain within %s (possible deadlock)", d)
	}
}
