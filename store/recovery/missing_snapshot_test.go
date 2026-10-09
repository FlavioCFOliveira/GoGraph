package recovery

// missing_snapshot_test.go — rmp #2990, storage audit F1: a WAL emptied by the
// whole-file wal.Writer.Truncate, with no snapshot left to cover it, is refused
// rather than opened as a clean, empty store.
//
// Layer: short.

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRecovery_RefusesEmptiedWALWithoutSnapshot commits two transactions, empties
// the WAL with Truncate (the "after a self-sufficient snapshot" helper) and
// recovers the directory with no snapshot in it — the state a lost snapshot
// directory leaves. Before the fix recovery returned a nil error, IsClean true
// and an empty graph.
func TestRecovery_RefusesEmptiedWALWithoutSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{})
	st := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewInt64WeightCodec(),
	})
	for _, k := range []string{"a", "b"} {
		tx := st.Begin()
		mustTx(t, tx.AddNode(k))
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%q): %v", k, err)
		}
	}
	if n, err := w.Truncate(); err != nil || n == 0 {
		t.Fatalf("Truncate = (%d, %v), want a non-zero size and nil", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewInt64WeightCodec(),
	})
	if !errors.Is(err, ErrMissingSnapshot) {
		t.Fatalf("Open error = %v, want %v (IsClean=%v)", err, ErrMissingSnapshot, res.IsClean())
	}
	if res.IsClean() {
		t.Error("IsClean() = true on a refused recovery")
	}
}
