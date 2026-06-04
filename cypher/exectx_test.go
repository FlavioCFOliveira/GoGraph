package cypher_test

// exectx_test.go — regression tests for engine-level explicit transactions
// ([cypher.ExplicitTx], #1280/#1302). They exercise the public engine API
// (BeginTx/Exec/Commit/Rollback) on BOTH wirings:
//
//   - WAL-backed (cypher.NewEngineWithStore): atomic durable rollback — a fresh
//     recovery.Open observes a rolled-back node ABSENT; commit makes writes
//     durable; the store single-writer mutex held BEGIN→COMMIT gives write-write
//     isolation.
//   - store-less (cypher.NewEngine): rollback leaves the in-memory graph
//     unchanged via the accumulated undo log; the engine writer mutex held
//     BEGIN→COMMIT gives write-write isolation.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
)

// storelessEngineWithGraph builds a store-less engine over a fresh directed
// graph and returns both, so a test can inspect the live graph directly.
func storelessEngineWithGraph(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g), g
}

// drainExec drains a Result returned by ExplicitTx.Exec and returns its iteration
// error (the per-statement error), then closes it. A nil return means the
// statement executed cleanly.
func drainExec(t *testing.T, res *cypher.Result) error {
	t.Helper()
	for res.Next() { //nolint:revive // intentional full drain
	}
	err := res.Err()
	_ = res.Close()
	return err
}

// liveNodeCount counts non-tombstoned nodes in g.
func liveNodeCount(g *lpg.Graph[string, float64]) int {
	live := 0
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if realID, ok := g.AdjList().Mapper().Lookup(key); ok && !g.IsTombstoned(realID) {
			_ = id
			live++
		}
		return true
	})
	return live
}

// TestExplicitTx_Rollback_DurableAbsent is the #1280 atomic-rollback AC for the
// WAL-backed wiring: BEGIN; CREATE (:Doomed); ROLLBACK; then a fresh recovery
// must observe :Doomed ABSENT (nothing was made durable) and the live graph must
// also be clean.
func TestExplicitTx_Rollback_DurableAbsent(t *testing.T) {
	eng, g, w, dir := walEngineWithGraph(t)

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	res, err := tx.Exec("CREATE (:Doomed {v:1})", nil)
	if err != nil {
		t.Fatalf("Exec CREATE: %v", err)
	}
	if derr := drainExec(t, res); derr != nil {
		t.Fatalf("drain CREATE: %v", derr)
	}

	// Before rollback the eager write IS visible in the live graph (read-uncommitted
	// for readers — the documented isolation scope).
	if n := liveNodeCount(g); n != 1 {
		t.Fatalf("pre-rollback live node count = %d, want 1 (eager apply)", n)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// (1) The live in-memory graph must be clean: the undo log unwound the CREATE.
	if n := liveNodeCount(g); n != 0 {
		t.Errorf("post-rollback live node count = %d, want 0 (in-memory undo)", n)
	}

	// (2) A fresh recovery from the WAL must observe NO :Doomed node: the
	// transaction never committed, so nothing was fsynced.
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	rres, err := recovery.Open[string, float64](dir, recOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	got := 0
	rres.Graph.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if realID, ok := rres.Graph.AdjList().Mapper().Lookup(key); ok && !rres.Graph.IsTombstoned(realID) {
			got++
		}
		return true
	})
	if got != 0 {
		t.Errorf("recovered graph has %d live nodes, want 0 (durable rollback)", got)
	}
}

// TestExplicitTx_Rollback_StorelessUnchanged is the #1280 atomic-rollback AC for
// the store-less wiring: a multi-statement transaction's writes are fully unwound
// by ROLLBACK, leaving the in-memory graph exactly as it was before BEGIN.
func TestExplicitTx_Rollback_StorelessUnchanged(t *testing.T) {
	eng, g := storelessEngineWithGraph(t)

	// Seed one committed node via autocommit so we can prove rollback restores to
	// the pre-transaction state, not to empty.
	if err := runWrite(t, eng, "CREATE (:Seed {k:1})"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n := liveNodeCount(g); n != 1 {
		t.Fatalf("after seed, live = %d, want 1", n)
	}

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	// Two statements inside the transaction.
	for i, q := range []string{"CREATE (:Tmp {i:1})", "CREATE (:Tmp {i:2})"} {
		res, eerr := tx.Exec(q, nil)
		if eerr != nil {
			t.Fatalf("Exec[%d]: %v", i, eerr)
		}
		if derr := drainExec(t, res); derr != nil {
			t.Fatalf("drain[%d]: %v", i, derr)
		}
	}
	if n := liveNodeCount(g); n != 3 {
		t.Fatalf("mid-tx live = %d, want 3 (eager apply)", n)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// The two Tmp nodes are gone; the seeded node survives.
	if n := liveNodeCount(g); n != 1 {
		t.Errorf("post-rollback live = %d, want 1 (only the seed survives)", n)
	}
}

// TestExplicitTx_Commit_StorelessVisible confirms a committed explicit
// transaction's writes are visible afterwards on the store-less wiring, and the
// handle is finished (a second Commit/Exec returns ErrTxFinished).
func TestExplicitTx_Commit_StorelessVisible(t *testing.T) {
	eng, g := storelessEngineWithGraph(t)

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	res, err := tx.Exec("CREATE (:Kept {v:1})", nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if derr := drainExec(t, res); derr != nil {
		t.Fatalf("drain: %v", derr)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n := liveNodeCount(g); n != 1 {
		t.Errorf("post-commit live = %d, want 1", n)
	}

	// Handle is finished: further use is rejected.
	if err := tx.Commit(); !errors.Is(err, cypher.ErrTxFinished) {
		t.Errorf("second Commit error = %v, want ErrTxFinished", err)
	}
	if _, err := tx.Exec("CREATE (:After)", nil); !errors.Is(err, cypher.ErrTxFinished) {
		t.Errorf("Exec after finish error = %v, want ErrTxFinished", err)
	}
}

// TestExplicitTx_WriteWriteIsolation_WALBacked is the #1280 isolation AC: while
// session A holds an open explicit transaction (after one write), a concurrent
// writer B must BLOCK until A commits. The test proves B does not complete before
// A commits, then completes promptly once A releases the writer mutex.
func TestExplicitTx_WriteWriteIsolation_WALBacked(t *testing.T) {
	eng, _, w, _ := walEngineWithGraph(t)
	t.Cleanup(func() { _ = w.Close() })
	assertWriteWriteIsolation(t, eng)
}

// TestExplicitTx_WriteWriteIsolation_Storeless is the store-less analogue: the
// engine writer mutex (not a store mutex) must serialise a concurrent autocommit
// write behind an open explicit transaction.
func TestExplicitTx_WriteWriteIsolation_Storeless(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)
	assertWriteWriteIsolation(t, eng)
}

// assertWriteWriteIsolation drives the A-blocks-B scenario against eng on either
// wiring: A opens an explicit tx and writes; B (a concurrent autocommit write)
// must not finish until A commits.
func assertWriteWriteIsolation(t *testing.T, eng *cypher.Engine) {
	t.Helper()

	txA, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("A BeginTx: %v", err)
	}
	resA, err := txA.Exec("CREATE (:A {v:1})", nil)
	if err != nil {
		t.Fatalf("A Exec: %v", err)
	}
	if derr := drainExec(t, resA); derr != nil {
		t.Fatalf("A drain: %v", derr)
	}

	// B starts and attempts a write; it must block on the writer serialisation A
	// holds. bStarted is closed just before B issues its write so the test does
	// not race the goroutine launch; bDone carries B's completion.
	bStarted := make(chan struct{})
	bDone := make(chan error, 1)
	go func() {
		close(bStarted)
		bDone <- runWrite(t, eng, "CREATE (:B {v:1})")
	}()
	<-bStarted

	// B must NOT complete while A's transaction is open. Give it a generous
	// window to (incorrectly) finish; if it does, isolation is broken.
	select {
	case e := <-bDone:
		t.Fatalf("B write completed (%v) while A's explicit transaction was still open — write-write isolation broken", e)
	case <-time.After(300 * time.Millisecond):
		// Expected: B is blocked behind A.
	}

	// Commit A — releases the writer serialisation.
	if err := txA.Commit(); err != nil {
		t.Fatalf("A Commit: %v", err)
	}

	// B must now complete promptly.
	select {
	case e := <-bDone:
		if e != nil {
			t.Fatalf("B write failed after A committed: %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B write did not complete after A committed — writer serialisation leaked")
	}
}

// TestExplicitTx_ContextCancelInterruptsExec is the #1302 cancellation AC at the
// engine level: a cancelled BeginTx context interrupts an in-flight Exec, which
// returns a context-wrapped error, and the writer serialisation is then released
// (proven by a subsequent autocommit write completing under a watchdog).
func TestExplicitTx_ContextCancelInterruptsExec(t *testing.T) {
	eng, g := storelessEngineWithGraph(t)

	// Seed enough nodes that a MATCH ... SET statement has work to do.
	for i := 0; i < 50; i++ {
		if err := runWrite(t, eng, fmt.Sprintf("CREATE (:Item {id:%d})", i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	// Cancel before issuing the statement: Exec must observe the cancellation and
	// return promptly without running the statement.
	cancel()
	_, execErr := tx.Exec("MATCH (n:Item) SET n.x = 1", nil)
	if execErr == nil {
		t.Fatal("expected Exec to fail on a cancelled context, got nil")
	}
	if !errors.Is(execErr, context.Canceled) {
		t.Fatalf("Exec error = %v, want wrapping context.Canceled", execErr)
	}

	// Roll the (untouched) transaction back to release the writer mutex.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	_ = g

	// The writer serialisation must have been released: a subsequent autocommit
	// write completes under a watchdog (a leaked mutex would deadlock).
	done := make(chan error, 1)
	go func() { done <- runWrite(t, eng, "CREATE (:After)") }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("post-cancel write failed: %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("post-cancel write deadlocked: writer serialisation leaked")
	}
}

// TestExplicitTx_PanicMidStatementRollsBackAndReleases is the engine-level panic
// AC: a statement that panics mid-pipeline inside an explicit transaction must
// (a) leave NO partial in-memory mutation, (b) return an ErrInternalPanic-wrapped
// error, and (c) release the writer serialisation so a subsequent write completes.
func TestExplicitTx_PanicMidStatementRollsBackAndReleases(t *testing.T) {
	quietLogs(t)
	eng, g := storelessEngineWithGraph(t)

	if err := runWrite(t, eng, "CREATE (:N {name:'a'})"); err != nil {
		t.Fatalf("seed a: %v", err)
	}

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	// boom() panics during exec; setting it as a property value drives the panic
	// mid-pipeline, after the SET operator has bound the row.
	_, execErr := tx.Exec("MATCH (n:N) SET n.touched = 1, n.bad = boom()", nil)
	if execErr == nil {
		t.Fatal("expected panic-converted error, got nil")
	}
	if !errors.Is(execErr, cypher.ErrInternalPanic) {
		t.Fatalf("Exec error %v does not wrap ErrInternalPanic", execErr)
	}

	// (a) No node may carry touched/bad: the panic rolled the eager writes back
	// inside the barrier.
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if _, ok := g.GetNodeProperty(key, "touched"); ok {
			t.Errorf("node %q still has touched after panic rollback", key)
		}
		if _, ok := g.GetNodeProperty(key, "bad"); ok {
			t.Errorf("node %q still has bad after panic rollback", key)
		}
		return true
	})

	// The handle is finished by the panic path; a Rollback is a clean no-op.
	if err := tx.Rollback(); !errors.Is(err, cypher.ErrTxFinished) {
		t.Errorf("Rollback after panic = %v, want ErrTxFinished (handle already finished)", err)
	}

	// (c) Writer serialisation released: a subsequent write completes promptly.
	done := make(chan error, 1)
	go func() { done <- runWrite(t, eng, "CREATE (:N {name:'c'})") }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("post-panic write failed: %v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("post-panic write deadlocked: writer serialisation leaked on panic path")
	}
}

// TestExplicitTx_DDLRejected confirms a DDL statement is rejected inside an
// explicit transaction (schema changes are not transactional here).
func TestExplicitTx_DDLRejected(t *testing.T) {
	eng, _ := storelessEngineWithGraph(t)
	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	if _, err := tx.Exec("CREATE INDEX FOR (n:Person) ON (n.name)", nil); err == nil {
		t.Fatal("expected DDL inside explicit transaction to be rejected, got nil")
	}
}

// TestExplicitTx_PerStatementErrorKeepsTxOpen confirms that a runtime error in
// one statement does NOT auto-roll-back the whole transaction at the engine
// level: the handle stays open so the caller decides. A later Rollback then
// unwinds everything.
func TestExplicitTx_PerStatementErrorKeepsTxOpen(t *testing.T) {
	eng, g := storelessEngineWithGraph(t)

	// Seed two nodes so a SET has rows; install a validator that rejects the 2nd
	// `x` write so the SET statement errors mid-pipeline.
	if err := runWrite(t, eng, "CREATE (:Item {id:1})"); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if err := runWrite(t, eng, "CREATE (:Item {id:2})"); err != nil {
		t.Fatalf("seed 2: %v", err)
	}

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	// First statement succeeds: create a Tmp node inside the tx.
	res, err := tx.Exec("CREATE (:Tmp {v:1})", nil)
	if err != nil {
		t.Fatalf("Exec CREATE: %v", err)
	}
	if derr := drainExec(t, res); derr != nil {
		t.Fatalf("drain CREATE: %v", derr)
	}

	// Second statement fails mid-pipeline.
	g.SetValidator(&nthSetRejector{key: "x", rejN: 2})
	res2, err := tx.Exec("MATCH (n:Item) SET n.x = 1", nil)
	if err != nil {
		// The error may surface either from Exec (build) or from the drain.
		t.Logf("Exec returned error directly: %v", err)
	} else if derr := drainExec(t, res2); derr == nil {
		t.Fatal("expected the SET statement to error mid-pipeline, got nil")
	}
	g.SetValidator(nil)

	// The transaction is still open: a Rollback (not ErrTxFinished) must succeed
	// and unwind BOTH the Tmp node and any partial SET writes.
	if rerr := tx.Rollback(); rerr != nil {
		t.Fatalf("Rollback after per-statement error: %v", rerr)
	}

	// Everything the transaction did is gone; only the two seeded Items remain.
	if n := liveNodeCount(g); n != 2 {
		t.Errorf("post-rollback live = %d, want 2 (only seeds survive)", n)
	}
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if _, ok := g.GetNodeProperty(key, "x"); ok {
			t.Errorf("node %q still has x after rollback (partial SET not unwound)", key)
		}
		return true
	})
}

// concurrencyGuard is a tiny sanity check that two explicit transactions on the
// same engine serialise rather than corrupt shared state under -race: open A,
// write, commit; then open B, write, commit; both visible. Run under -race.
func TestExplicitTx_SequentialTransactionsRace(t *testing.T) {
	eng, g := storelessEngineWithGraph(t)

	var wg sync.WaitGroup
	wg.Add(2)
	run := func(label string) {
		defer wg.Done()
		tx, err := eng.BeginTx(context.Background())
		if err != nil {
			t.Errorf("%s BeginTx: %v", label, err)
			return
		}
		res, err := tx.Exec(fmt.Sprintf("CREATE (:Conc {who:'%s'})", label), nil)
		if err != nil {
			t.Errorf("%s Exec: %v", label, err)
			_ = tx.Rollback()
			return
		}
		for res.Next() { //nolint:revive // drain
		}
		_ = res.Close()
		if err := tx.Commit(); err != nil {
			t.Errorf("%s Commit: %v", label, err)
		}
	}
	go run("x")
	go run("y")
	wg.Wait()

	if n := liveNodeCount(g); n != 2 {
		t.Errorf("after two concurrent committed transactions, live = %d, want 2", n)
	}
}
