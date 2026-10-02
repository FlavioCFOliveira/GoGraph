package cypher_test

// engine_opened_test.go — rmp #2523: an engine built over store.Open re-registers
// the recovered schema.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
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
