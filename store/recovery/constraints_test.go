package recovery

// constraints_test.go — recovery-level coverage for the durable constraint ops
// (#1316): a CREATE CONSTRAINT op replays into Result.Constraints, a DROP
// suppresses an earlier CREATE (last-writer-wins), and the recovered set is
// reconciled with a snapshot's constraints.bin component.

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func crRecOpts() Options[string, float64] {
	return Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
}

func crStore(t *testing.T, dir string) (*txn.Store[string, float64], *wal.Writer) {
	t.Helper()
	res, err := Open[string, float64](dir, crRecOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	store := txn.NewStoreWithOptions[string, float64](res.Graph, w,
		txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()})
	return store, w
}

// commitConstraintTx writes one constraint op as its own committed transaction.
func commitConstraintTx(t *testing.T, store *txn.Store[string, float64], create bool, kind txn.ConstraintKind, label, prop, name string) {
	t.Helper()
	tx := store.Begin()
	var err error
	if create {
		err = tx.CreateConstraint(kind, label, prop, name)
	} else {
		err = tx.DropConstraint(kind, label, prop, name)
	}
	if err != nil {
		t.Fatalf("buffer constraint op: %v", err)
	}
	if cerr := tx.CommitWALOnly(); cerr != nil {
		t.Fatalf("CommitWALOnly: %v", cerr)
	}
}

// TestRecovery_ConstraintOpReplay verifies a CREATE CONSTRAINT op committed to
// the WAL is surfaced via Result.Constraints on the next Open.
func TestRecovery_ConstraintOpReplay(t *testing.T) {
	dir := t.TempDir()
	store, w := crStore(t, dir)
	commitConstraintTx(t, store, true, txn.ConstraintUnique, "User", "email", "u_email")
	commitConstraintTx(t, store, true, txn.ConstraintNotNull, "User", "name", "nn_name")
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, float64](dir, crRecOpts())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(res.Constraints) != 2 {
		t.Fatalf("got %d constraints, want 2: %+v", len(res.Constraints), res.Constraints)
	}
	// Deterministic order: kind 0 (UNIQUE) before kind 1 (NOT NULL).
	if res.Constraints[0] != (ConstraintRecord{Kind: txn.ConstraintUnique, Label: "User", Property: "email", Name: "u_email"}) {
		t.Errorf("constraint[0] = %+v", res.Constraints[0])
	}
	if res.Constraints[1] != (ConstraintRecord{Kind: txn.ConstraintNotNull, Label: "User", Property: "name", Name: "nn_name"}) {
		t.Errorf("constraint[1] = %+v", res.Constraints[1])
	}
}

// TestRecovery_ConstraintDropSuppressesCreate verifies last-writer-wins: a DROP
// op committed after a CREATE for the same (kind, label, property) removes it
// from the recovered set. This exercises OpDropConstraint end-to-end.
func TestRecovery_ConstraintDropSuppressesCreate(t *testing.T) {
	dir := t.TempDir()
	store, w := crStore(t, dir)
	commitConstraintTx(t, store, true, txn.ConstraintUnique, "Item", "sku", "u_sku")
	commitConstraintTx(t, store, true, txn.ConstraintUnique, "Item", "ean", "u_ean")
	commitConstraintTx(t, store, false, txn.ConstraintUnique, "Item", "sku", "u_sku") // drop sku
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, float64](dir, crRecOpts())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(res.Constraints) != 1 {
		t.Fatalf("got %d constraints, want 1 (sku dropped): %+v", len(res.Constraints), res.Constraints)
	}
	if res.Constraints[0].Property != "ean" {
		t.Errorf("surviving constraint = %+v, want the ean one", res.Constraints[0])
	}
}

// TestRecovery_ConstraintSnapshotPlusWAL verifies the snapshot's constraints.bin
// is reconciled with WAL ops: a constraint in the snapshot survives, and a WAL
// DROP after the snapshot removes it.
func TestRecovery_ConstraintSnapshotPlusWAL(t *testing.T) {
	dir := t.TempDir()

	// Build a graph and write a snapshot carrying two constraints (the
	// checkpoint-survival source), then truncate the WAL.
	store, w := crStore(t, dir)
	g := store.Graph()
	cs := csr.BuildFromAdjList(g.AdjList())
	specs := []snapshot.ConstraintSpec{
		{Kind: uint8(txn.ConstraintUnique), Label: "A", Property: "x", Name: "ux"},
		{Kind: uint8(txn.ConstraintUnique), Label: "B", Property: "y", Name: "uy"},
	}
	if err := snapshot.WriteSnapshotFullWithMapperCodecAndConstraints(
		filepath.Join(dir, "snapshot"), cs, g, txn.NewStringCodec(), specs); err != nil {
		t.Fatalf("WriteSnapshot...AndConstraints: %v", err)
	}
	if _, err := w.Truncate(); err != nil {
		t.Fatalf("wal.Truncate: %v", err)
	}
	// After truncate, append a WAL op that drops one of the snapshot
	// constraints.
	commitConstraintTx(t, store, false, txn.ConstraintUnique, "A", "x", "ux")
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	res, err := Open[string, float64](dir, crRecOpts())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(res.Constraints) != 1 {
		t.Fatalf("got %d constraints, want 1 (A.x dropped, B.y survives): %+v", len(res.Constraints), res.Constraints)
	}
	if res.Constraints[0].Property != "y" {
		t.Errorf("surviving constraint = %+v, want B.y", res.Constraints[0])
	}
}
