package cypher_test

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// edgeCount returns the number of directed relationships in the engine's graph.
func edgeCount(t *testing.T, eng *cypher.Engine) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), "MATCH ()-[r]->() RETURN count(r)", nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	defer func() { _ = res.Close() }()
	var n int64
	if res.Next() {
		if v, ok := res.ValueAt(0).(expr.IntegerValue); ok {
			n = int64(v)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("count drain: %v", err)
	}
	return n
}

// TestRollback_ParallelEdgeCreate_SimpleGraph_ErrorsAndPreservesExistingEdge
// covers the ACID atomicity concern the disk-full DST scenario originally
// found (#1751) under the corrected contract from the 2026-07-02
// production-readiness audit (finding F1, rmp #1856).
//
// #1751's bug: on a SIMPLE (non-multigraph) graph, re-CREATEing an
// already-existing edge used to be treated as a storage no-op, but the
// in-memory undo log still recorded a RemoveEdge inverse for it. Rolling the
// transaction back then DELETED the pre-existing committed edge.
//
// F1 found that the underlying premise — a simple graph silently treating a
// second CREATE between a connected pair as a no-op — is itself an openCypher
// conformance violation (CREATE never deduplicates; a repeated CREATE must
// always add another relationship) and a silent data-loss hazard for genuinely
// distinct parallel edges. The fix makes that case a hard, fail-fast error
// ([cypher.ErrParallelEdgeInSimpleGraph]) raised BEFORE any mutation, so the
// no-op-with-unsafe-undo scenario #1751 fixed can no longer occur: there is no
// mutation for the undo log to record incorrectly. This test now asserts the
// new contract — the re-CREATE errors, and the pre-existing edge survives
// because nothing was ever touched.
func TestRollback_ParallelEdgeCreate_SimpleGraph_ErrorsAndPreservesExistingEdge(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true}) // simple graph
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Commit two nodes and one edge between them.
	if _, err := eng.RunInTx(ctx, "CREATE (a:Person {name:'A'}), (b:Person {name:'B'})", nil); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if _, err := eng.RunInTx(ctx, "MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS]->(b)", nil); err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	if got := edgeCount(t, eng); got != 1 {
		t.Fatalf("after seed: edge count = %d, want 1", got)
	}

	// In an explicit transaction, attempt to re-CREATE a parallel edge on the
	// SAME pair. On a simple graph this must fail fast, not silently no-op.
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	_, execErr := tx.Exec("MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS]->(b)", nil)
	if !errors.Is(execErr, cypher.ErrParallelEdgeInSimpleGraph) {
		_ = tx.Rollback()
		t.Fatalf("Exec re-create: expected errors.Is(err, ErrParallelEdgeInSimpleGraph), got: %v", execErr)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := edgeCount(t, eng); got != 1 {
		t.Fatalf("ACID atomicity breach: rolled-back rejected CREATE left edge count = %d, want 1 (the committed edge was destroyed)", got)
	}
}

// TestRollback_MultigraphEdgeCreate_StillParallel asserts the fix did NOT change
// the multigraph path: CREATE of an edge between already-connected nodes always
// adds a parallel relationship (openCypher: CREATE never deduplicates), and a
// committed CREATE persists it.
func TestRollback_MultigraphEdgeCreate_StillParallel(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	if _, err := eng.RunInTx(ctx, "CREATE (a:Person {name:'A'}), (b:Person {name:'B'})", nil); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := eng.RunInTx(ctx, "MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS]->(b)", nil); err != nil {
			t.Fatalf("create parallel edge %d: %v", i, err)
		}
	}
	if got := edgeCount(t, eng); got != 3 {
		t.Fatalf("multigraph CREATE should add a parallel edge each time: count = %d, want 3", got)
	}
}

// TestRollback_SameTxDuplicateEdge_SimpleGraph_SecondCreateErrors covers the
// cypher-expert caveat under the F1-corrected contract (rmp #1856): within ONE
// transaction, create an edge then attempt to re-create a parallel edge on the
// same pair. The first CREATE is a genuine new edge (the pair was previously
// unconnected, even within this transaction's own in-flight state); the second
// now fails fast with [cypher.ErrParallelEdgeInSimpleGraph] instead of silently
// no-oping. Rollback then unwinds the first CREATE's undo, so the graph returns
// to empty — exactly its pre-transaction state.
func TestRollback_SameTxDuplicateEdge_SimpleGraph_SecondCreateErrors(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	if _, err := eng.RunInTx(ctx, "CREATE (a:Person {name:'A'}), (b:Person {name:'B'})", nil); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.Exec("MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS]->(b)", nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("Exec create #1: %v", err)
	}
	_, execErr := tx.Exec("MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS]->(b)", nil)
	if !errors.Is(execErr, cypher.ErrParallelEdgeInSimpleGraph) {
		_ = tx.Rollback()
		t.Fatalf("Exec create #2: expected errors.Is(err, ErrParallelEdgeInSimpleGraph), got: %v", execErr)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := edgeCount(t, eng); got != 0 {
		t.Fatalf("rolled-back same-tx edge creates left count = %d, want 0", got)
	}
}
