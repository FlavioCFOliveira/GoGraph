package cypher_test

import (
	"context"
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

// TestRollback_NoopEdgeCreate_PreservesExistingEdge is the regression test for
// the ACID atomicity bug the disk-full DST scenario found: on a SIMPLE
// (non-multigraph) graph, re-CREATEing an already-existing edge is a storage
// no-op, but the in-memory undo log used to record a RemoveEdge inverse. Rolling
// the transaction back then DELETED the pre-existing committed edge. After the
// fix the no-op CREATE records no undo, so a rollback leaves the edge intact.
func TestRollback_NoopEdgeCreate_PreservesExistingEdge(t *testing.T) {
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

	// In an explicit transaction, re-CREATE the SAME edge (a simple-graph no-op),
	// then ROLL BACK. The pre-existing edge must survive.
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.Exec("MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS]->(b)", nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("Exec re-create: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := edgeCount(t, eng); got != 1 {
		t.Fatalf("ACID atomicity breach: rolled-back no-op CREATE left edge count = %d, want 1 (the committed edge was destroyed)", got)
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

// TestRollback_SameTxDuplicateEdge_SimpleGraph covers the cypher-expert caveat:
// within ONE transaction, create an edge then re-create it (the second is a
// no-op against the now-live in-tx state). On rollback the FIRST create's undo
// removes the edge it added, the second records nothing, so the graph returns to
// empty — exactly its pre-transaction state.
func TestRollback_SameTxDuplicateEdge_SimpleGraph(t *testing.T) {
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
	if _, err := tx.Exec("MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS]->(b)", nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("Exec create #2 (no-op): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := edgeCount(t, eng); got != 0 {
		t.Fatalf("rolled-back same-tx edge creates left count = %d, want 0", got)
	}
}
