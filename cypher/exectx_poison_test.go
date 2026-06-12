package cypher_test

// exectx_poison_test.go — regression gate for task #1413.
//
// ExplicitTx.Exec returns *ErrStatementPipeline when the execution pipeline
// fails inside the visibility barrier. Before this fix, ExplicitTx.Commit
// did not check whether a prior Exec had failed, so a caller that ignored
// the pipeline error and called Commit instead of Rollback would make the
// partial writes of the failed statement durable.
//
// Fix: a failed Exec sets tx.failed = true; Commit returns ErrTxPoisoned
// when the flag is set, forcing the caller to call Rollback instead.
//
// Pre-fix: TestExplicitTx_PoisonedByFailedExec_CommitRejected fails because
// Commit succeeds and the node count after recovery is 1 (the partial write
// from the first failed Exec becomes durable).
// Post-fix: both tests pass.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
)

// failAlwaysRejector is a lpg.SchemaValidator that rejects every write for the
// named property key unconditionally. Used to guarantee a mid-pipeline failure
// on the very first row.
type failAlwaysRejector struct{ key string }

func (v *failAlwaysRejector) Validate(propertyName string, _ lpg.PropertyValue) error {
	if propertyName == v.key {
		return fmt.Errorf("failAlwaysRejector: rejected write to %q", v.key)
	}
	return nil
}

// TestExplicitTx_PoisonedByFailedExec_CommitRejected is the primary AC for
// #1413.
//
// Sequence:
//
//  1. Seed one node outside the explicit tx.
//  2. BeginTx.
//  3. Exec a CREATE that succeeds (adds a node inside the tx).
//  4. Install a validator that rejects the "poison" property.
//  5. Exec a second CREATE that writes "poison" — must return ErrStatementPipeline.
//  6. Call Commit — must return ErrTxPoisoned (not nil).
//  7. Confirm node count (store-less) equals the seed only (partial writes gone).
//  8. On the WAL-backed variant, confirm that the WAL replay also sees only the
//     seed node (no partial writes made it to the WAL).
func TestExplicitTx_PoisonedByFailedExec_CommitRejected(t *testing.T) {
	t.Parallel()

	// ── store-less variant ────────────────────────────────────────────────────
	t.Run("storeless", func(t *testing.T) {
		t.Parallel()
		eng, g := storelessEngineWithGraph(t)

		if err := runWrite(t, eng, `CREATE (:Seed {v:0})`); err != nil {
			t.Fatalf("seed: %v", err)
		}

		tx, err := eng.BeginTx(t.Context())
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })

		// Statement 1: CREATE succeeds — adds (:Good) inside the tx.
		res, err := tx.Exec(`CREATE (:Good {v:1})`, nil)
		if err != nil {
			t.Fatalf("Exec CREATE Good: %v", err)
		}
		if derr := drainExec(t, res); derr != nil {
			t.Fatalf("drain CREATE Good: %v", derr)
		}

		// Install a validator that will cause the next SET to fail.
		g.SetValidator(&failAlwaysRejector{key: "poison"})
		t.Cleanup(func() { g.SetValidator(nil) })

		// Statement 2: CREATE with "poison" — must fail with ErrStatementPipeline.
		res2, execErr := tx.Exec(`CREATE (:Bad {poison:'x'})`, nil)
		if execErr == nil {
			// Error may also come from the drain.
			drainErr := drainExec(t, res2)
			if drainErr == nil {
				t.Fatal("expected second Exec to fail, but it succeeded")
			}
		} else {
			var sp *cypher.ErrStatementPipeline
			if !errors.As(execErr, &sp) {
				t.Fatalf("expected *ErrStatementPipeline, got %T: %v", execErr, execErr)
			}
		}
		g.SetValidator(nil)

		// Commit must now be rejected with ErrTxPoisoned.
		commitErr := tx.Commit()
		if commitErr == nil {
			t.Fatal("Commit succeeded after a failed Exec — partial writes can become durable")
		}
		if !errors.Is(commitErr, cypher.ErrTxPoisoned) {
			t.Fatalf("Commit returned %v (%T), want ErrTxPoisoned", commitErr, commitErr)
		}

		// After the poison-blocked Commit the handle is still live — Rollback
		// must succeed and unwind everything.
		if rerr := tx.Rollback(); rerr != nil {
			t.Fatalf("Rollback after poisoned Commit: %v", rerr)
		}

		// Only the seed remains; the :Good node created inside the tx is gone.
		if n := liveNodeCount(g); n != 1 {
			t.Errorf("post-rollback live = %d, want 1 (only seed)", n)
		}
		g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
			if _, ok := g.GetNodeProperty(key, "v"); ok {
				v, _ := g.GetNodeProperty(key, "v")
				if s, _ := v.String(); s == "1" {
					if g.HasNodeLabel(key, "Good") {
						t.Errorf("node :Good{v:1} survived rollback — partial write persisted")
					}
				}
			}
			return true
		})
	})

	// ── WAL-backed variant: partial writes must NOT be in the WAL ─────────────
	t.Run("wal_backed", func(t *testing.T) {
		t.Parallel()
		eng, g, w, dir := walEngineWithGraph(t)
		t.Cleanup(func() { _ = w.Close() })

		if err := runWrite(t, eng, `CREATE (:Seed {v:0})`); err != nil {
			t.Fatalf("seed: %v", err)
		}

		tx, err := eng.BeginTx(t.Context())
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		t.Cleanup(func() { _ = tx.Rollback() })

		// Statement 1: succeeds inside the tx.
		res, err := tx.Exec(`CREATE (:Good {v:1})`, nil)
		if err != nil {
			t.Fatalf("Exec CREATE Good: %v", err)
		}
		if derr := drainExec(t, res); derr != nil {
			t.Fatalf("drain CREATE Good: %v", derr)
		}

		// Statement 2: install validator, expect pipeline failure.
		g.SetValidator(&failAlwaysRejector{key: "poison"})
		t.Cleanup(func() { g.SetValidator(nil) })

		res2, execErr := tx.Exec(`CREATE (:Bad {poison:'x'})`, nil)
		if execErr == nil {
			drainErr := drainExec(t, res2)
			if drainErr == nil {
				t.Fatal("expected second Exec to fail, but it succeeded")
			}
		} else {
			var sp *cypher.ErrStatementPipeline
			if !errors.As(execErr, &sp) {
				t.Fatalf("expected *ErrStatementPipeline, got %T: %v", execErr, execErr)
			}
		}
		g.SetValidator(nil)

		// Commit must be rejected.
		commitErr := tx.Commit()
		if commitErr == nil {
			t.Fatal("Commit succeeded after a failed Exec (WAL-backed)")
		}
		if !errors.Is(commitErr, cypher.ErrTxPoisoned) {
			t.Fatalf("Commit returned %v, want ErrTxPoisoned", commitErr)
		}

		// Rollback unwinds in-memory state.
		if rerr := tx.Rollback(); rerr != nil {
			t.Fatalf("Rollback after poisoned Commit (WAL): %v", rerr)
		}

		// Close the WAL and recover: only the seed node must exist.
		if err := w.Close(); err != nil {
			t.Fatalf("wal.Close: %v", err)
		}
		res3, err := recovery.Open[string, float64](dir, recOpts())
		if err != nil {
			t.Fatalf("recovery.Open: %v", err)
		}
		liveAfterRecovery := 0
		res3.Graph.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
			if !res3.Graph.IsTombstoned(id) {
				liveAfterRecovery++
			}
			return true
		})
		if liveAfterRecovery != 1 {
			t.Errorf("recovered graph has %d live nodes, want 1 (only seed — no partial writes)", liveAfterRecovery)
		}
	})
}

// TestExplicitTx_PoisonedByFailedExec_RollbackSucceeds verifies that after a
// poisoned Exec a Rollback call succeeds and releases the writer serialisation.
func TestExplicitTx_PoisonedByFailedExec_RollbackSucceeds(t *testing.T) {
	t.Parallel()
	eng, g := storelessEngineWithGraph(t)

	if err := runWrite(t, eng, `CREATE (:Seed {v:0})`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tx, err := eng.BeginTx(t.Context())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	// Cause a pipeline failure.
	g.SetValidator(&failAlwaysRejector{key: "fail"})
	res, execErr := tx.Exec(`CREATE (:X {fail:'y'})`, nil)
	if execErr == nil {
		_ = drainExec(t, res) // consume the drain error; we only need the tx to be poisoned
	}
	g.SetValidator(nil)

	// Rollback must succeed (not ErrTxFinished, not ErrTxPoisoned).
	if rerr := tx.Rollback(); rerr != nil {
		t.Fatalf("Rollback after poisoned Exec: %v", rerr)
	}
}
