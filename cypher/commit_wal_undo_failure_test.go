package cypher

// commit_wal_undo_failure_test.go — gate test for task #1379.
//
// ExplicitTx.Commit's WAL-fsync-failure branch must surface ErrUndoFailed
// when the subsequent in-memory undo replay also fails. Before the fix
// rollbackInBarrierLocked()'s return value is discarded and ErrUndoFailed is
// silently swallowed; after the fix it is captured and wrapUndoFailure wraps
// the WAL error with it.
//
// The test runs in package cypher (internal) because it needs access to the
// unexported ExplicitTx.undo field and undoLog.record method to inject the
// panicking inverse.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestCommit_WALFsyncFailure_UndoFailure_SurfacesErrUndoFailed is the gate
// test for #1379. It verifies that when CommitWALOnly returns an error AND the
// subsequent rollbackInBarrierLocked undo replay also fails (because a
// recorded inverse panics), the Commit error wraps BOTH wal.ErrWriterClosed
// and ErrUndoFailed.
//
// Gate invariant:
//   - FAILS before the fix (rollbackInBarrierLocked return value discarded →
//     ErrUndoFailed is silently swallowed → errors.Is returns false).
//   - PASSES after the fix (undoOK captured → wrapUndoFailure called →
//     errors.Is returns true for both sentinels).
func TestCommit_WALFsyncFailure_UndoFailure_SurfacesErrUndoFailed(t *testing.T) {
	// Silence the slog output produced by the panicking undo inverse so the test
	// output stays readable while still exercising the logging path.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Build a WAL-backed engine over a fresh directed graph, replicating the
	// internal setup used by other internal-package tests (walEngineWithGraph is
	// in package cypher_test and therefore not accessible here).
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := NewEngineWithStore(store)

	// Open an explicit transaction and run one write statement so the undo log
	// is populated with at least one real inverse.
	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	res, execErr := tx.Exec("CREATE (:Person {name: 'alice'})", nil)
	if execErr != nil {
		t.Fatalf("Exec CREATE: %v", execErr)
	}
	// Drain the result to completion before touching the undo log.
	for res.Next() { //nolint:revive // intentional full drain
	}
	if drainErr := res.Err(); drainErr != nil {
		t.Fatalf("result drain: %v", drainErr)
	}
	_ = res.Close()

	// Inject a panicking inverse into the undo log. When rollbackInBarrierLocked
	// calls undo.replay(), this closure will panic, causing replay to return
	// false (undoOK=false). Before the fix that false return is discarded and
	// ErrUndoFailed is never surfaced; after the fix wrapUndoFailure is called.
	tx.undo.record(func() { panic("injected undo panic for task #1379") })

	// Break the WAL writer so that CommitWALOnly returns wal.ErrWriterClosed,
	// which drives the fsync-failure branch in Commit where the bug lived.
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("wal.Close: %v", cerr)
	}

	// Commit must fail: the WAL fsync failed AND the undo replay also panicked.
	commitErr := tx.Commit()

	// (1) Commit must return an error at all.
	if commitErr == nil {
		t.Fatal("Commit returned nil; expected an error wrapping wal.ErrWriterClosed and ErrUndoFailed")
	}

	// (2) The WAL error must be present in the chain.
	if !errors.Is(commitErr, wal.ErrWriterClosed) {
		t.Errorf("errors.Is(commitErr, wal.ErrWriterClosed) = false; commitErr = %v", commitErr)
	}

	// (3) ErrUndoFailed must also be present — this is the sentinel that was
	// silently discarded before the fix.
	if !errors.Is(commitErr, ErrUndoFailed) {
		t.Errorf("errors.Is(commitErr, ErrUndoFailed) = false; commitErr = %v", commitErr)
	}
}
