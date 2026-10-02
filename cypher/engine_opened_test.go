package cypher_test

// engine_opened_test.go — rmp #2523: an engine built over store.Open re-registers
// the recovered schema.
//
// Layer: short.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// openedOptions is the codec pair the test opens with.
func openedOptions() store.Options[string, float64] {
	return store.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// runOpened runs q on eng, drains it, and returns the first error the run, the
// drain, or the close produced.
func runOpened(ctx context.Context, eng *cypher.Engine, q string) error {
	res, err := eng.RunAny(ctx, q, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// TestNewEngineWithOpened_ReRegistersRecoveredSchema declares a UNIQUE
// constraint and an index, closes the store, reopens it with store.Open, and
// requires the engine built by NewEngineWithOpened to enforce the constraint
// and to list both schema objects. An engine built with NewEngineWithStore over
// the same opened store loses the constraint's name and the index, which is
// the omission the constructor exists to prevent.
func TestNewEngineWithOpened_ReRegistersRecoveredSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	o, err := store.Open(dir, openedOptions())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	eng := cypher.NewEngineWithOpened(o)
	for _, q := range []string{
		`CREATE CONSTRAINT person_id FOR (n:Person) REQUIRE n.id IS UNIQUE`,
		`CREATE INDEX person_name FOR (n:Person) ON (n.name)`,
		`CREATE (:Person {id: 1, name: 'a'})`,
	} {
		if err := runOpened(ctx, eng, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	o2, err := store.Open(dir, openedOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if cerr := o2.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if n := len(o2.Recovery().Constraints); n != 1 {
		t.Fatalf("recovery carried %d constraints, want 1", n)
	}
	if n := len(o2.Recovery().Indexes); n == 0 {
		t.Fatal("recovery carried no index definition")
	}
	eng2 := cypher.NewEngineWithOpened(o2)
	if err := runOpened(ctx, eng2, `CREATE (:Person {id: 1, name: 'b'})`); err == nil {
		t.Fatal("a duplicate id was accepted after reopen: the recovered UNIQUE constraint is not enforced")
	}
	names := map[string]bool{}
	for _, c := range eng2.ConstraintSpecsForSnapshot() {
		names[c.Name] = true
	}
	if !names["person_id"] {
		t.Fatalf("the reopened engine does not carry constraint person_id under its original name; got %v", names)
	}
	idx := map[string]bool{}
	for _, s := range eng2.IndexSpecsForSnapshot() {
		idx[s.Name] = true
	}
	if !idx["person_name"] {
		t.Fatalf("the reopened engine does not carry index person_name; got %v", idx)
	}
}

// flipCRCOfTxn flips the last byte of the first data frame of transaction
// txnSeq in dir/wal, so recovery stops there with a CRC mismatch and keeps
// every earlier transaction.
func flipCRCOfTxn(t *testing.T, dir string, txnSeq uint64) {
	t.Helper()
	walPath := filepath.Join(dir, "wal")
	raw, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	r := bytes.NewReader(raw)
	for {
		f, derr := wal.Decode(r)
		if derr != nil {
			t.Fatalf("no data frame of transaction %d", txnSeq)
		}
		op, oerr := recovery.Decode(f.Payload)
		if oerr != nil {
			t.Fatalf("recovery.Decode: %v", oerr)
		}
		if op.Version == txn.OpRecordV3 && op.TxnSeq == txnSeq && op.Kind != txn.OpCommit {
			raw[len(raw)-r.Len()-1] ^= 0xFF
			break
		}
	}
	if err := os.WriteFile(walPath, raw, 0o600); err != nil { //nolint:gosec // G703: path under t.TempDir
		t.Fatalf("WriteFile: %v", err)
	}
}

// countPeople returns the number of :Person nodes eng sees.
func countPeople(t *testing.T, eng *cypher.Engine) int64 {
	t.Helper()
	res, err := eng.RunAny(context.Background(), `MATCH (n:Person) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("count returned no row: %v", res.Err())
	}
	switch c := res.Record()["c"].(type) {
	case expr.IntegerValue:
		return int64(c)
	case int64:
		return c
	default:
		t.Fatalf("count returned %T", c)
		return 0
	}
}

// TestNewEngineWithOpened_ReadOnlyRefusesWrites opens an unclean directory with
// store.Options.AllowUnclean and requires the engine over it to answer reads
// from the committed prefix and to refuse every write with
// store.ErrReadOnlyStore, leaving the graph and the schema unchanged.
func TestNewEngineWithOpened_ReadOnlyRefusesWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	o, err := store.Open(dir, openedOptions())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	eng := cypher.NewEngineWithOpened(o)
	for _, q := range []string{
		`CREATE (:Person {id: 1})`,
		`CREATE (:Person {id: 2})`,
		`CREATE (:Person {id: 3})`,
	} {
		if err := runOpened(ctx, eng, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	flipCRCOfTxn(t, dir, 3)

	opts := openedOptions()
	opts.AllowUnclean = true
	ro, err := store.Open(dir, opts)
	if err != nil {
		t.Fatalf("store.Open with AllowUnclean: %v", err)
	}
	defer func() {
		if cerr := ro.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if !ro.ReadOnly() {
		t.Fatal("the unclean directory did not open read-only")
	}
	reng := cypher.NewEngineWithOpened(ro)
	if got := countPeople(t, reng); got != 2 {
		t.Fatalf("read-only engine sees %d people, want the committed prefix of 2", got)
	}
	for _, q := range []string{
		`CREATE (:Person {id: 9})`,
		`MERGE (:Person {id: 10})`,
		`MATCH (n:Person {id: 1}) SET n.name = 'x'`,
		`MATCH (n:Person {id: 1}) DETACH DELETE n`,
		`CREATE INDEX person_id FOR (n:Person) ON (n.id)`,
		`CREATE CONSTRAINT person_uniq FOR (n:Person) REQUIRE n.id IS UNIQUE`,
	} {
		if err := runOpened(ctx, reng, q); !errors.Is(err, store.ErrReadOnlyStore) {
			t.Fatalf("%s: got %v, want store.ErrReadOnlyStore", q, err)
		}
	}
	tx, err := reng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	res, err := tx.ExecAny(`CREATE (:Person {id: 11})`, nil)
	if err == nil {
		for res.Next() {
		}
		_ = res.Close()
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if !errors.Is(err, store.ErrReadOnlyStore) {
		t.Fatalf("explicit transaction: got %v, want store.ErrReadOnlyStore", err)
	}
	if got := countPeople(t, reng); got != 2 {
		t.Fatalf("after the refused writes the engine sees %d people, want 2", got)
	}
	var name any
	res, err = reng.RunAny(ctx, `MATCH (n:Person {id: 1}) RETURN n.name AS name`, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Next() {
		name = res.Record()["name"]
	}
	_ = res.Close()
	if v, isValue := name.(expr.Value); name != nil && (!isValue || !expr.IsNull(v)) {
		t.Fatalf("a refused SET is visible: n.name = %v", name)
	}
	if n := len(reng.IndexSpecsForSnapshot()); n != 0 {
		t.Fatalf("a refused CREATE INDEX left %d index definitions", n)
	}
	if n := len(reng.ConstraintSpecsForSnapshot()); n != 0 {
		t.Fatalf("a refused CREATE CONSTRAINT left %d constraints", n)
	}
}
