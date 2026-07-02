package cypher_test

// parallel_edge_multigraph_guard_test.go — regression gate for the 2026-07-02
// production-readiness audit finding F1.
//
// openCypher's data model is a multigraph: every CREATE adds a relationship,
// including a second relationship between a node pair that is already
// connected. Before this fix, constructing the engine over the documented
// default configuration (adjlist.Config{Directed: true}, i.e. Multigraph:
// false) made a second CREATE between an existing pair return success while
// silently storing nothing — the edge simply vanished, an Atomicity/Durability
// concern from the caller's perspective and a conformance violation of the
// module's non-negotiable openCypher mandate. The fix makes
// lpgMutatorAdapter/walMutatorAdapter's AddEdge/AddEdgeH fail fast with
// [cypher.ErrParallelEdgeInSimpleGraph] instead of silently no-oping, and
// NewEngine(WithOptions) logs a warning when handed a non-multigraph graph.
//
// These tests drive the PUBLIC Cypher engine (never the adjlist/lpg APIs
// directly) so they exercise exactly what a production caller observes.

import (
	"context"
	"errors"
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
	for res.Next() { //nolint:revive // intentional drain
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

// TestCypher_ParallelEdge_SimpleGraph_DistinctType_Fails pins F1: on a
// non-multigraph engine, CREATE-ing a second, differently-typed relationship
// between an already-connected pair fails loudly instead of silently no-oping.
func TestCypher_ParallelEdge_SimpleGraph_DistinctType_Fails(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	if err := drainQuery(t, eng, `CREATE (a:X {k:'a'}), (b:X {k:'b'})`); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if err := drainQuery(t, eng, `MATCH (a:X {k:'a'}), (b:X {k:'b'}) CREATE (a)-[:T1 {p:'one'}]->(b)`); err != nil {
		t.Fatalf("seed T1 edge: %v", err)
	}

	err := drainQuery(t, eng, `MATCH (a:X {k:'a'}), (b:X {k:'b'}) CREATE (a)-[:T2 {p:'two'}]->(b)`)
	if err == nil {
		t.Fatal("expected the parallel-edge CREATE to fail on a non-multigraph engine, got nil error")
	}
	if !errors.Is(err, cypher.ErrParallelEdgeInSimpleGraph) {
		t.Fatalf("expected errors.Is(err, ErrParallelEdgeInSimpleGraph), got: %v", err)
	}

	// The pre-existing T1 edge must be untouched and T2 must not exist at all
	// (no silent partial state from the rejected write).
	if n := countScalar(t, eng, `MATCH (:X {k:'a'})-[rel]->(:X {k:'b'}) RETURN count(rel) AS c`); n != 1 {
		t.Fatalf("total edges after failed parallel CREATE = %d, want 1 (T1 only)", n)
	}
	if n := countScalar(t, eng, `MATCH (:X {k:'a'})-[rel:T2]->(:X {k:'b'}) RETURN count(rel) AS c`); n != 0 {
		t.Fatalf("T2 edges = %d, want 0 (rejected write must not partially apply)", n)
	}
}

// TestCypher_ParallelEdge_SimpleGraph_SameType_Fails mirrors the previous test
// for a same-typed parallel edge (the audit's second repro case).
func TestCypher_ParallelEdge_SimpleGraph_SameType_Fails(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	if err := drainQuery(t, eng, `CREATE (a:Y {k:'a'}), (b:Y {k:'b'})`); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if err := drainQuery(t, eng, `MATCH (a:Y {k:'a'}), (b:Y {k:'b'}) CREATE (a)-[:R {p:'one'}]->(b)`); err != nil {
		t.Fatalf("seed first R edge: %v", err)
	}

	err := drainQuery(t, eng, `MATCH (a:Y {k:'a'}), (b:Y {k:'b'}) CREATE (a)-[:R {p:'two'}]->(b)`)
	if !errors.Is(err, cypher.ErrParallelEdgeInSimpleGraph) {
		t.Fatalf("expected errors.Is(err, ErrParallelEdgeInSimpleGraph), got: %v", err)
	}
	if n := countScalar(t, eng, `MATCH (:Y {k:'a'})-[rel:R]->(:Y {k:'b'}) RETURN count(rel) AS c`); n != 1 {
		t.Fatalf("R edges after failed parallel CREATE = %d, want 1", n)
	}
}

// TestCypher_ParallelEdge_SimpleGraph_NoPartialMutation exercises the guard
// composed with the engine's mid-pipeline rollback machinery (#1282): a single
// CREATE clause that creates one legitimate new edge and one rejected parallel
// edge in the same statement must leave NEITHER visible afterwards.
func TestCypher_ParallelEdge_SimpleGraph_NoPartialMutation(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	if err := drainQuery(t, eng, `CREATE (a:P {k:'a'}), (b:P {k:'b'}), (c:P {k:'c'})`); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if err := drainQuery(t, eng, `MATCH (a:P {k:'a'}), (b:P {k:'b'}) CREATE (a)-[:R1]->(b)`); err != nil {
		t.Fatalf("seed R1: %v", err)
	}

	// Single statement: (a)-[:NEW]->(c) is a genuinely new edge; (a)-[:R2]->(b)
	// is a rejected parallel edge on the already-connected (a,b) pair. Both
	// patterns are in the same CREATE clause, so the whole write must roll back.
	err := drainQuery(t, eng,
		`MATCH (a:P {k:'a'}), (b:P {k:'b'}), (c:P {k:'c'}) CREATE (a)-[:NEW]->(c), (a)-[:R2]->(b)`)
	if !errors.Is(err, cypher.ErrParallelEdgeInSimpleGraph) {
		t.Fatalf("expected errors.Is(err, ErrParallelEdgeInSimpleGraph), got: %v", err)
	}
	if n := countScalar(t, eng, `MATCH (:P {k:'a'})-[rel:NEW]->(:P {k:'c'}) RETURN count(rel) AS c`); n != 0 {
		t.Fatalf(":NEW edges = %d, want 0 (whole statement must roll back, not just the rejected pattern)", n)
	}
	if n := countScalar(t, eng, `MATCH (:P {k:'a'})-[rel:R1]->(:P {k:'b'}) RETURN count(rel) AS c`); n != 1 {
		t.Fatalf("pre-existing R1 edge = %d, want 1 (untouched by the failed statement)", n)
	}
}

// TestCypher_ParallelEdge_Multigraph_Succeeds proves the engine is fully
// correct once constructed with Multigraph: true — both a distinctly-typed and
// a same-typed parallel edge are stored and independently readable.
func TestCypher_ParallelEdge_Multigraph_Succeeds(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
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

// TestCypher_ParallelEdge_MERGE_SimpleGraph_Fails proves the guard also fires
// on MergeRelationship's create branch (merge_relationship.go), which calls
// AddEdgeH directly for a genuinely new relationship type between an
// already-connected pair — the same silent-drop hazard as a plain CREATE.
func TestCypher_ParallelEdge_MERGE_SimpleGraph_Fails(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	if err := drainQuery(t, eng, `CREATE (a:M {k:'a'})-[:R]->(b:M {k:'b'})`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// :S does not exist between a and b, so MERGE's create branch fires — on a
	// simple graph this is a parallel edge on an already-connected pair.
	err := drainQuery(t, eng, `MATCH (a:M {k:'a'}), (b:M {k:'b'}) MERGE (a)-[:S]->(b)`)
	if !errors.Is(err, cypher.ErrParallelEdgeInSimpleGraph) {
		t.Fatalf("expected errors.Is(err, ErrParallelEdgeInSimpleGraph), got: %v", err)
	}
	if n := countScalar(t, eng, `MATCH (:M {k:'a'})-[rel:S]->(:M {k:'b'}) RETURN count(rel) AS c`); n != 0 {
		t.Fatalf("S edges = %d, want 0", n)
	}
}

// walGuardStoreOpts returns the store/recovery options shared by the
// WAL-backed guard tests.
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

// TestCypher_ParallelEdge_SimpleGraph_WAL_FailsCleanly exercises
// walMutatorAdapter's guard (as opposed to lpgMutatorAdapter's, covered above):
// a rejected parallel-edge write over a WAL-backed store must not corrupt the
// WAL or leave the store unusable for the next query.
func TestCypher_ParallelEdge_SimpleGraph_WAL_FailsCleanly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, walGuardStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	if err := drainQuery(t, eng, `CREATE (a:Z {k:'a'})-[:R]->(b:Z {k:'b'})`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = drainQuery(t, eng, `MATCH (a:Z {k:'a'}), (b:Z {k:'b'}) CREATE (a)-[:S]->(b)`)
	if !errors.Is(err, cypher.ErrParallelEdgeInSimpleGraph) {
		t.Fatalf("expected errors.Is(err, ErrParallelEdgeInSimpleGraph), got: %v", err)
	}

	// The store must still be usable: a subsequent legitimate write succeeds.
	if err := drainQuery(t, eng, `CREATE (:Z {k:'c'})`); err != nil {
		t.Fatalf("post-rejection write failed, store may be corrupted: %v", err)
	}
	if n := countScalar(t, eng, `MATCH (:Z {k:'a'})-[rel:S]->(:Z {k:'b'}) RETURN count(rel) AS c`); n != 0 {
		t.Fatalf("S edges = %d, want 0", n)
	}
	if n := countScalar(t, eng, `MATCH (n:Z) RETURN count(n) AS c`); n != 3 {
		t.Fatalf(":Z node count = %d, want 3 (a, b, c)", n)
	}
}

// TestCypher_ParallelEdge_Multigraph_WAL_Durable proves the guard does not
// interfere with the legitimate multigraph path across a close/reopen cycle:
// two parallel edges created through the WAL-backed engine both survive
// recovery.
func TestCypher_ParallelEdge_Multigraph_WAL_Durable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("open1 wal.Open: %v", err)
	}
	g1 := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
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
