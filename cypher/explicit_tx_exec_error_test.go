package cypher_test

// explicit_tx_exec_error_test.go — gate test for #1378:
// ExplicitTx.Exec must return a runtime statement error directly (not silently
// discard it with result.Err() hidden behind a nil Exec error).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// TestExplicitTx_Exec_RuntimeError_Returned is the #1378 gate: a statement
// that raises a runtime error during Exec must surface it as the Exec return
// value, not hide it inside the returned Result. After the error the handle
// must still be open so the caller can issue Rollback.
func TestExplicitTx_Exec_RuntimeError_Returned(t *testing.T) {
	t.Parallel()
	eng, g := storelessEngineWithGraph(t)

	// Seed one node so the SET statement has a row to process.
	if err := runWrite(t, eng, "CREATE (:Item {id:1})"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	// Install a validator that rejects the first write of property "x" so the
	// SET statement fails at runtime (the engine applies mutations eagerly, so
	// the validator fires during the pipeline iteration inside execUnderBarrier).
	g.SetValidator(&nthSetRejector{key: "x", rejN: 1})
	_, execErr := tx.Exec("MATCH (n:Item) SET n.x = 1 RETURN n", nil)
	g.SetValidator(nil)

	// AC1: the runtime error must be returned by Exec directly — not silently
	// swallowed (i.e. execErr must not be nil).
	if execErr == nil {
		t.Fatal("Exec: expected runtime error from validator rejection, got nil")
	}

	// AC2: the handle must still be open — Rollback must succeed, not return
	// ErrTxFinished.
	if rbErr := tx.Rollback(); rbErr != nil {
		t.Fatalf("Rollback after runtime error: %v", rbErr)
	}
}

// TestExplicitTx_Exec_Success_ResultUnchanged confirms that when a statement
// succeeds (no runtime error) Exec still returns a non-nil Result and a nil
// error — i.e. Fix 1 does not regress the happy path.
func TestExplicitTx_Exec_Success_ResultUnchanged(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	res, execErr := tx.Exec("CREATE (:Ok {v:1})", nil)
	if execErr != nil {
		t.Fatalf("Exec: unexpected error on success path: %v", execErr)
	}
	if res == nil {
		t.Fatal("Exec: result is nil on success path")
	}
	if derr := drainExec(t, res); derr != nil {
		t.Fatalf("drain: unexpected error on success path: %v", derr)
	}
}

// TestExplicitTx_Exec_RuntimeError_TxRollbackUnwindsAll confirms that after a
// runtime error is returned by Exec, Rollback correctly unwinds ALL writes
// accumulated in the transaction up to that point (including writes from prior
// successful statements).
func TestExplicitTx_Exec_RuntimeError_TxRollbackUnwindsAll(t *testing.T) {
	t.Parallel()
	eng, g := storelessEngineWithGraph(t)

	// Seed one node outside the transaction so we have a reference count.
	if err := runWrite(t, eng, "CREATE (:Seed {id:0})"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	// First statement inside tx — succeeds.
	res, err := tx.Exec("CREATE (:Tmp {v:1})", nil)
	if err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	if derr := drainExec(t, res); derr != nil {
		t.Fatalf("drain first Exec: %v", derr)
	}

	// Second statement — fails at runtime via validator.
	g.SetValidator(&nthSetRejector{key: "y", rejN: 1})
	_, execErr := tx.Exec("MATCH (n:Seed) SET n.y = 1 RETURN n", nil)
	g.SetValidator(nil)
	if execErr == nil {
		t.Fatal("second Exec: expected runtime error, got nil")
	}

	// Rollback must unwind the first statement's CREATE as well.
	if rbErr := tx.Rollback(); rbErr != nil {
		t.Fatalf("Rollback: %v", rbErr)
	}

	// Only the seeded Seed node should remain; the Tmp node must be gone.
	if n := liveNodeCount(g); n != 1 {
		t.Errorf("post-rollback live node count = %d, want 1 (only seed survives)", n)
	}
}

// TestExplicitTx_Exec_RuntimeError_HandleRemainsOpen confirms that a second
// call to Exec after a runtime error is permitted (the handle stays open);
// only Rollback must be called to release the writer serialisation. A second
// Exec after a non-panic error must NOT return ErrTxFinished.
func TestExplicitTx_Exec_RuntimeError_HandleRemainsOpen(t *testing.T) {
	t.Parallel()
	eng, g := storelessEngineWithGraph(t)

	if err := runWrite(t, eng, "CREATE (:N {id:1})"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	// Trigger a runtime error.
	g.SetValidator(&nthSetRejector{key: "z", rejN: 1})
	_, execErr := tx.Exec("MATCH (n:N) SET n.z = 1 RETURN n", nil)
	g.SetValidator(nil)
	if execErr == nil {
		t.Fatal("first Exec: expected runtime error, got nil")
	}

	// A second Exec must NOT return ErrTxFinished — the handle is still open.
	res2, err2 := tx.Exec("RETURN 1 AS x", nil)
	if err2 != nil {
		if err2.Error() == cypher.ErrTxFinished.Error() {
			t.Fatalf("second Exec returned ErrTxFinished after a non-panic error — handle incorrectly closed")
		}
		// Some other error is acceptable (e.g. if the engine rejects the statement
		// for another reason), but the handle must not be permanently closed.
		t.Logf("second Exec returned non-finished error (acceptable): %v", err2)
		return
	}
	if res2 != nil {
		_ = drainExec(t, res2)
	}
}
