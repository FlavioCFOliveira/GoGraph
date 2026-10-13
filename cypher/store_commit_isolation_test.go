package cypher_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// storeIsolationCount runs q as an autocommit read and returns its single
// integer column.
func storeIsolationCount(t *testing.T, eng *cypher.Engine, q string) int64 {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var n int64
	for res.Next() {
		for _, v := range res.Record() {
			if _, serr := fmt.Sscan(fmt.Sprint(v), &n); serr != nil {
				t.Fatalf("%s: value %v: %v", q, v, serr)
			}
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// execAll runs q inside tx and drains it.
func execAll(tx *cypher.ExplicitTx, q string) error {
	r, err := tx.Exec(q, nil)
	if err != nil {
		return err
	}
	for r.Next() {
	}
	return r.Close()
}

// TestStoreCommit_CannotPublishARolledBackCypherTransactionsEdge is the
// end-to-end case of the ACID audit of rmp #2965. A Cypher explicit transaction
// T1 creates a->c on an engine backed by a durable store; a store transaction
// commits an edge on the same source while T1 is open; T1 is then doomed and
// rolled back. Before the fix the store commit's entry embedded T1's
// uncommitted arc: an autocommit reader saw a->c while T1 was open (a dirty
// read), and after T1's rollback a->c stayed in the graph although no WAL
// record holds it. The store commit is now refused retryably while T1 holds a,
// and succeeds once T1 has ended.
func TestStoreCommit_CannotPublishARolledBackCypherTransactionsEdge(t *testing.T) {
	w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{})
	st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)

	seed := st.Begin()
	for _, k := range []string{"a", "c", "d", "z"} {
		if err := seed.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := seed.SetNodeProperty(k, "id", lpg.StringValue(k)); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Commit(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	t1, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := execAll(t1, "MATCH (a {id:'a'}),(c {id:'c'}) CREATE (a)-[:R]->(c)"); err != nil {
		t.Fatal(err)
	}

	other := st.Begin()
	if err := other.AddEdgeWithHandle("a", "d", 1, g.NextEdgeHandle()); err != nil {
		t.Fatal(err)
	}
	if err := other.Commit(); !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("store commit on a while T1 holds a: err = %v; want a retryable serialization conflict", err)
	}
	if n := storeIsolationCount(t, eng, "MATCH ({id:'a'})-[r]->({id:'c'}) RETURN count(r) AS n"); n != 0 {
		t.Errorf("DIRTY READ: an autocommit reader sees %d a->c edges while T1 is open; want 0", n)
	}

	// Doom T1: a peer holds z.k uncommitted, then roll T1 back.
	t3, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := execAll(t3, "MATCH (z {id:'z'}) SET z.k = 1"); err != nil {
		t.Fatal(err)
	}
	if err := execAll(t1, "MATCH (z {id:'z'}) SET z.k = 2"); !errors.Is(err, mvcc.ErrSerializationConflict) {
		t.Fatalf("T1's conflicting statement: err = %v; want a serialization conflict", err)
	}
	if err := t1.Rollback(); err != nil {
		t.Fatalf("T1 rollback: %v", err)
	}
	if err := t3.Rollback(); err != nil {
		t.Fatalf("T3 rollback: %v", err)
	}
	g.ReclaimNow()
	if n := storeIsolationCount(t, eng, "MATCH ({id:'a'})-[r]->({id:'c'}) RETURN count(r) AS n"); n != 0 {
		t.Errorf("ATOMICITY: rolled-back T1's a->c is in the graph (%d); the WAL holds no such edge", n)
	}

	// The refusal was retryable.
	retry := st.Begin()
	if err := retry.AddEdgeWithHandle("a", "d", 1, g.NextEdgeHandle()); err != nil {
		t.Fatal(err)
	}
	if err := retry.Commit(); err != nil {
		t.Fatalf("retried store commit: %v", err)
	}
	if n := storeIsolationCount(t, eng, "MATCH ({id:'a'})-[r]->() RETURN count(r) AS n"); n != 1 {
		t.Errorf("edges out of a = %d; want 1 (the retried store commit's a->d)", n)
	}
}
