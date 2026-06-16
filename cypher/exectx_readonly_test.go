package cypher_test

// exectx_readonly_test.go — regression tests for the read-only explicit
// transaction ([cypher.Engine.BeginReadTx], task #1573). A read-only handle
// acquires NO writer serialisation, NO visibility barrier, and NO WAL
// transaction: it rejects writing/DDL statements before execution with
// [cypher.ErrWriteInReadOnlyTx], routes reads through the engine's concurrent
// read path (per-statement Graph.View snapshot, read-committed across
// statements), and its Commit/Rollback are teardown-only no-ops.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestReadTx_RejectsWriteAndDDL verifies that every writing clause and every
// DDL statement is refused with [cypher.ErrWriteInReadOnlyTx] BEFORE any state
// change, leaving the graph unchanged.
func TestReadTx_RejectsWriteAndDDL(t *testing.T) {
	eng, g := storelessEngineWithGraph(t)

	// Seed one node via an autocommit write so there is a known baseline.
	seed, err := eng.RunInTx(context.Background(), "CREATE (:Seed {v:1})", nil)
	if err != nil {
		t.Fatalf("seed RunInTx: %v", err)
	}
	if err := drainExec(t, seed); err != nil {
		t.Fatalf("seed drain: %v", err)
	}
	baseline := liveNodeCount(g)
	if baseline != 1 {
		t.Fatalf("baseline node count = %d, want 1", baseline)
	}

	writers := []string{
		"CREATE (:X)",
		"MATCH (n:Seed) SET n.v = 2",
		"MATCH (n:Seed) REMOVE n.v",
		"MATCH (n:Seed) DELETE n",
		"MATCH (n:Seed) DETACH DELETE n",
		"MERGE (:Y {k:1})",
		"CREATE INDEX FOR (n:Seed) ON (n.v)",
		"CREATE CONSTRAINT FOR (n:Seed) REQUIRE n.v IS UNIQUE",
	}

	for _, q := range writers {
		t.Run(q, func(t *testing.T) {
			tx, err := eng.BeginReadTx(context.Background())
			if err != nil {
				t.Fatalf("BeginReadTx: %v", err)
			}
			res, err := tx.Exec(q, nil)
			if !errors.Is(err, cypher.ErrWriteInReadOnlyTx) {
				t.Fatalf("Exec(%q) err = %v, want ErrWriteInReadOnlyTx", q, err)
			}
			if res != nil {
				t.Fatalf("Exec(%q) returned non-nil Result on rejection", q)
			}
			if got := liveNodeCount(g); got != baseline {
				t.Fatalf("after rejected %q node count = %d, want %d (no state change)", q, got, baseline)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatalf("Rollback after rejection: %v", err)
			}
		})
	}
}

// TestReadTx_PermitsReads verifies a MATCH ... RETURN runs and yields the
// correct result inside a read-only transaction.
func TestReadTx_PermitsReads(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)

	seed, err := eng.RunInTx(context.Background(), "CREATE (:N {v:7})", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := drainExec(t, seed); err != nil {
		t.Fatalf("seed drain: %v", err)
	}

	tx, err := eng.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec("MATCH (n:N) RETURN n.v AS v", nil)
	if err != nil {
		t.Fatalf("Exec read: %v", err)
	}
	if !res.Next() {
		t.Fatalf("read returned no rows: %v", res.Err())
	}
	row := res.Record()
	iv, ok := row["v"].(expr.IntegerValue)
	if !ok || int64(iv) != 7 {
		t.Fatalf("v = %v (%T), want IntegerValue 7", row["v"], row["v"])
	}
	if res.Next() {
		t.Fatalf("unexpected extra row")
	}
	if err := res.Err(); err != nil {
		t.Fatalf("res.Err: %v", err)
	}
	_ = res.Close()
}

// TestReadTx_ReadCommittedAcrossStatements verifies read-committed isolation:
// a commit made by another (autocommit) transaction BETWEEN two statements of a
// read-only transaction is observed by the second statement. This proves each
// RUN takes its own fresh View snapshot rather than pinning one for the whole
// transaction.
func TestReadTx_ReadCommittedAcrossStatements(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)

	tx, err := eng.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// First read: empty graph.
	count1 := readCount(t, tx, "MATCH (n:M) RETURN count(n) AS c")
	if count1 != 0 {
		t.Fatalf("first read count = %d, want 0", count1)
	}

	// A concurrent autocommit write commits between the two reads. (Same engine;
	// the read-only tx holds no barrier, so this write is not blocked.)
	w, err := eng.RunInTx(context.Background(), "CREATE (:M)", nil)
	if err != nil {
		t.Fatalf("interleaved write: %v", err)
	}
	if err := drainExec(t, w); err != nil {
		t.Fatalf("interleaved write drain: %v", err)
	}

	// Second read: must observe the freshly committed node (read-committed).
	count2 := readCount(t, tx, "MATCH (n:M) RETURN count(n) AS c")
	if count2 != 1 {
		t.Fatalf("second read count = %d, want 1 (read-committed)", count2)
	}
}

// readCount runs an aggregating read through the read-only tx and returns the
// single integer count column.
func readCount(t *testing.T, tx *cypher.ExplicitTx, query string) int64 {
	t.Helper()
	res, err := tx.Exec(query, nil)
	if err != nil {
		t.Fatalf("Exec(%q): %v", query, err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("Exec(%q): no rows: %v", query, res.Err())
	}
	c, ok := res.Record()["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("Exec(%q): c = %v (%T), not an IntegerValue", query, res.Record()["c"], res.Record()["c"])
	}
	return int64(c)
}

// TestReadTx_CommitRollbackNoOps verifies Commit and Rollback on a read-only
// transaction are no-ops that never panic, never leak a lock, and that a second
// finishing call returns ErrTxFinished.
func TestReadTx_CommitRollbackNoOps(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)

	// Commit then a second Commit/Rollback are idempotent/rejected.
	tx1, err := eng.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if err := tx1.Commit(); !errors.Is(err, cypher.ErrTxFinished) {
		t.Fatalf("second Commit err = %v, want ErrTxFinished", err)
	}
	if err := tx1.Rollback(); !errors.Is(err, cypher.ErrTxFinished) {
		t.Fatalf("Rollback after Commit err = %v, want ErrTxFinished", err)
	}

	// Rollback then a second Rollback/Commit are idempotent/rejected.
	tx2, err := eng.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("first Rollback: %v", err)
	}
	if err := tx2.Rollback(); !errors.Is(err, cypher.ErrTxFinished) {
		t.Fatalf("second Rollback err = %v, want ErrTxFinished", err)
	}

	// The writer mutex must not have leaked: a subsequent autocommit write
	// (which acquires the engine writer mutex on a store-less engine) succeeds
	// without blocking.
	done := make(chan error, 1)
	go func() {
		w, werr := eng.RunInTx(context.Background(), "CREATE (:After)", nil)
		if werr != nil {
			done <- werr
			return
		}
		done <- drainExec(t, w)
	}()
	select {
	case werr := <-done:
		if werr != nil {
			t.Fatalf("post-readtx write: %v", werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-readtx write blocked: read-only tx leaked the writer lock")
	}
}

// TestReadTx_ConcurrentReadsDoNotSerialise is the deterministic anti-serialise
// test. Under the old exclusive-barrier path, an explicit transaction held the
// visibility barrier for its whole lifetime, so a concurrent autocommit read
// (Engine.Run takes Graph.View / visMu.RLock) would block until the explicit tx
// finished. A read-only transaction holds no barrier, so a concurrent reader —
// and many concurrent read-only transactions — proceed without blocking on the
// open handle.
//
// The test opens a read-only tx and KEEPS IT OPEN, then runs a concurrent
// autocommit read plus several concurrent read-only transactions; all must
// complete while the first handle is still open. A barrier-holding design would
// deadlock/time out here.
func TestReadTx_ConcurrentReadsDoNotSerialise(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)

	seed, err := eng.RunInTx(context.Background(), "CREATE (:Q {v:1})", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := drainExec(t, seed); err != nil {
		t.Fatalf("seed drain: %v", err)
	}

	// Open a read-only tx and hold it open for the duration of the test.
	held, err := eng.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx (held): %v", err)
	}
	defer func() { _ = held.Rollback() }()
	// Execute one read on the held handle to exercise its read path while open.
	if got := readCount(t, held, "MATCH (n:Q) RETURN count(n) AS c"); got != 1 {
		t.Fatalf("held read count = %d, want 1", got)
	}

	const workers = 16
	var wg sync.WaitGroup
	errCh := make(chan error, workers+1)

	// One concurrent autocommit read (would block under the old barrier design).
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, rerr := eng.Run(context.Background(), "MATCH (n:Q) RETURN n", nil)
		if rerr != nil {
			errCh <- rerr
			return
		}
		errCh <- drainExec(t, r)
	}()

	// Many concurrent read-only transactions.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rtx, rerr := eng.BeginReadTx(context.Background())
			if rerr != nil {
				errCh <- rerr
				return
			}
			if c := readCountNoFatal(rtx); c != 1 {
				errCh <- errCountMismatch(c)
				_ = rtx.Rollback()
				return
			}
			errCh <- rtx.Rollback()
		}()
	}

	doneAll := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneAll)
	}()

	select {
	case <-doneAll:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent reads blocked while a read-only tx was open: serialisation regression")
	}
	close(errCh)
	for e := range errCh {
		if e != nil {
			t.Fatalf("concurrent worker: %v", e)
		}
	}
}

type errCountMismatch int64

func (e errCountMismatch) Error() string { return "unexpected count" }

// readCountNoFatal runs the canonical count read on rtx and returns the count,
// or -1 on any error. Safe to call from a goroutine (no t.Fatal).
func readCountNoFatal(rtx *cypher.ExplicitTx) int64 {
	res, err := rtx.Exec("MATCH (n:Q) RETURN count(n) AS c", nil)
	if err != nil {
		return -1
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		return -1
	}
	c, ok := res.Record()["c"].(expr.IntegerValue)
	if !ok {
		return -1
	}
	return int64(c)
}
