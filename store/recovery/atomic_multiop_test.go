package recovery

import (
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/txn"
	"gograph/store/wal"
)

// TestRecovery_MultiOpTransactionAtomic_TornAtEveryBoundary is the F1
// headline regression test (docs/acid-audit.md): a multi-op transaction
// must recover all-or-nothing across a crash. It commits one 6-op
// transaction through a typed store (which now writes v3 frames plus an
// OpCommit marker), then truncates the on-disk WAL at EVERY byte offset
// before the end and asserts the recovered graph is empty — because the
// OpCommit marker is the last frame, any truncation that loses any byte of
// it must discard the entire transaction. Only the complete WAL recovers
// the full transaction. A prefix of the ops must NEVER be observable.
//
// Before the fix (no commit record) recovery applied each frame as it was
// read and stopped at the first torn frame, so a truncation inside the
// batch left a partial node/edge — the atomicity violation this proves
// is gone.
func TestRecovery_MultiOpTransactionAtomic_TornAtEveryBoundary(t *testing.T) {
	t.Parallel()

	opts := Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}

	// Build a WAL holding exactly one 6-op transaction; capture its bytes.
	src := t.TempDir()
	srcWAL := filepath.Join(src, "wal")
	w, err := wal.Open(srcWAL)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	tx := s.Begin()
	mustTx(t, tx.AddNode("a"))
	mustTx(t, tx.SetNodeLabel("a", "L"))
	mustTx(t, tx.AddNode("b"))
	mustTx(t, tx.SetNodeProperty("a", "p", lpg.Int64Value(1)))
	mustTx(t, tx.AddEdge("a", "b", 7))
	mustTx(t, tx.SetEdgeLabel("a", "b", "R"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	raw, err := os.ReadFile(srcWAL) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(raw) < 8 {
		t.Fatalf("WAL too small (%d bytes) to exercise truncation", len(raw))
	}

	// observedAny reports whether ANY part of the transaction is visible.
	observedAny := func(rg *lpg.Graph[string, int64]) bool {
		if rg.AdjList().HasEdge("a", "b") {
			return true
		}
		if rg.HasNodeLabel("a", "L") {
			return true
		}
		if _, ok := rg.GetNodeProperty("a", "p"); ok {
			return true
		}
		// "b" is interned by AddNode; a recovered mapper entry for it is
		// also a partial-transaction signal.
		if _, ok := rg.AdjList().Mapper().Lookup("b"); ok {
			return true
		}
		return false
	}

	// Reuse one recovery dir, overwriting dir/wal with each truncated prefix.
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// 1..len(raw)-1: every truncation loses (part of) the marker => empty.
	for off := 1; off < len(raw); off++ {
		if err := os.WriteFile(walPath, raw[:off], 0o600); err != nil { //nolint:gosec // path under t.TempDir
			t.Fatalf("WriteFile(off=%d): %v", off, err)
		}
		res, err := Open[string, int64](dir, opts)
		if err != nil {
			t.Fatalf("Open(off=%d): %v", off, err)
		}
		if observedAny(res.Graph) {
			t.Fatalf("offset %d: a partial transaction was recovered — atomicity violated", off)
		}
		if res.WALOps != 0 {
			t.Fatalf("offset %d: WALOps=%d, want 0 (no committed transaction)", off, res.WALOps)
		}
	}

	// The complete WAL recovers the ENTIRE transaction.
	if err := os.WriteFile(walPath, raw, 0o600); err != nil { //nolint:gosec // path under t.TempDir
		t.Fatalf("WriteFile(full): %v", err)
	}
	res, err := Open[string, int64](dir, opts)
	if err != nil {
		t.Fatalf("Open(full): %v", err)
	}
	rg := res.Graph
	if !rg.AdjList().HasEdge("a", "b") {
		t.Error("full WAL: edge a->b missing")
	}
	if !rg.HasNodeLabel("a", "L") {
		t.Error("full WAL: node label L missing")
	}
	if v, ok := rg.GetNodeProperty("a", "p"); !ok {
		t.Error("full WAL: node property p missing")
	} else if got, _ := v.Int64(); got != 1 {
		t.Errorf("full WAL: node property p = %d, want 1", got)
	}
	if res.WALOps != 6 {
		t.Errorf("full WAL: WALOps = %d, want 6", res.WALOps)
	}
}
