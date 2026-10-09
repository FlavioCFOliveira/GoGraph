package cypher_test

// parallel_edge_multigraph_guard_test.go — regression gate for the 2026-07-02
// production-readiness audit finding F1.
//
// openCypher's data model is a multigraph: every CREATE adds a relationship,
// including a second relationship between a node pair that is already
// connected. F1 found a second CREATE between an existing pair returning
// success while silently storing nothing. Storage now holds only directed
// multigraphs (rmp #3072), so every CREATE adds a slot; these tests pin that a
// parallel relationship is stored, readable, and durable across recovery.
//
// These tests drive the PUBLIC Cypher engine (never the adjlist/lpg APIs
// directly) so they exercise exactly what a production caller observes.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// drainQuery runs query to completion and returns the terminal error, if any
// (either the immediate Run/RunInTx error or one surfaced by iteration).
func drainQuery(t *testing.T, eng *cypher.Engine, query string) error {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		return err
	}
	for res.Next() { // intentional drain
	}
	rerr := res.Err()
	_ = res.Close()
	return rerr
}

// countScalar runs a `RETURN count(...) AS c` style query and returns the
// integer result.
func countScalar(t *testing.T, eng *cypher.Engine, query string) int64 {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var n int64
	for res.Next() {
		v, ok := res.Record()["c"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("column c: expected IntegerValue, got %T", res.Record()["c"])
		}
		n = int64(v)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	return n
}

// TestCypher_ParallelEdge_Succeeds proves a second, differently-typed CREATE
// between an already-connected pair is stored and independently readable.
func TestCypher_ParallelEdge_Succeeds(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	for _, q := range []string{
		`CREATE (a:X {k:'a'}), (b:X {k:'b'})`,
		`MATCH (a:X {k:'a'}), (b:X {k:'b'}) CREATE (a)-[:T1 {p:'one'}]->(b)`,
		`MATCH (a:X {k:'a'}), (b:X {k:'b'}) CREATE (a)-[:T2 {p:'two'}]->(b)`,
	} {
		if err := drainQuery(t, eng, q); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}

	if n := countScalar(t, eng, `MATCH (:X {k:'a'})-[rel]->(:X {k:'b'}) RETURN count(rel) AS c`); n != 2 {
		t.Fatalf("total edges = %d, want 2", n)
	}
	if n := countScalar(t, eng, `MATCH (:X {k:'a'})-[rel:T2]->(:X {k:'b'}) RETURN count(rel) AS c`); n != 1 {
		t.Fatalf("T2 edges = %d, want 1", n)
	}
}

// walGuardStoreOpts returns the store/recovery options shared by the
// WAL-backed parallel-edge tests.
func walGuardStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

func walGuardRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// TestCypher_ParallelEdge_WAL_Durable proves two parallel edges created
// through the WAL-backed engine both survive a close/reopen cycle.
func TestCypher_ParallelEdge_WAL_Durable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open1 wal.Open: %v", err)
	}
	g1 := lpg.New[string, float64](adjlist.Config{})
	store1 := txn.NewStoreWithOptions[string, float64](g1, w1, walGuardStoreOpts())
	eng1 := cypher.NewEngineWithStore(store1)

	for _, q := range []string{
		`CREATE (a:W {k:'a'}), (b:W {k:'b'})`,
		`MATCH (a:W {k:'a'}), (b:W {k:'b'}) CREATE (a)-[:T1]->(b)`,
		`MATCH (a:W {k:'a'}), (b:W {k:'b'}) CREATE (a)-[:T2]->(b)`,
	} {
		if err := drainQuery(t, eng1, q); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("w1.Close: %v", err)
	}

	res, err := recovery.Open[string, float64](dir, walGuardRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open2 wal.Open: %v", err)
	}
	defer func() { _ = w2.Close() }()
	store2 := txn.NewStoreWithOptions[string, float64](res.Graph, w2, walGuardStoreOpts())
	eng2 := cypher.NewEngineWithStore(store2)

	if n := countScalar(t, eng2, `MATCH (:W {k:'a'})-[rel]->(:W {k:'b'}) RETURN count(rel) AS c`); n != 2 {
		t.Fatalf("recovered total edges = %d, want 2", n)
	}
	if n := countScalar(t, eng2, `MATCH (:W {k:'a'})-[rel:T2]->(:W {k:'b'}) RETURN count(rel) AS c`); n != 1 {
		t.Fatalf("recovered T2 edges = %d, want 1", n)
	}
}
