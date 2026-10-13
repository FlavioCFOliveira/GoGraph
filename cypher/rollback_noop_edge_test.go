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

// TestRollback_ParallelEdgeCreate_PreservesExistingEdge covers the ACID
// atomicity concern the disk-full DST scenario found (#1751): rolling back a
// transaction that re-CREATEs a relationship on an already-connected pair must
// remove only the relationship that transaction added, never the committed one.
// Every CREATE adds a parallel relationship (openCypher: CREATE never
// deduplicates), so the re-CREATE succeeds and its undo inverse names the new
// instance only.
func TestRollback_ParallelEdgeCreate_PreservesExistingEdge(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
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

	// In an explicit transaction, re-CREATE a parallel edge on the SAME pair,
	// observe it, then roll back.
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
		t.Fatalf("ACID atomicity breach: rolled-back parallel CREATE left edge count = %d, want 1 (the committed edge was destroyed or the new one survived)", got)
	}
}

// TestRollback_ParallelEdgeCreate_Committed asserts that CREATE of an edge
// between already-connected nodes always adds a parallel relationship
// (openCypher: CREATE never deduplicates), and a committed CREATE persists it.
func TestRollback_ParallelEdgeCreate_Committed(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
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
		t.Fatalf("CREATE should add a parallel edge each time: count = %d, want 3", got)
	}
}

// TestRollback_SameTxParallelEdges_RollBackToEmpty covers the cypher-expert
// caveat of #1751: within ONE transaction, create an edge and then a parallel
// edge on the same pair. Both are genuine additions, and rollback unwinds both,
// so the graph returns to exactly its pre-transaction state.
func TestRollback_SameTxParallelEdges_RollBackToEmpty(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
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
		t.Fatalf("Exec create #2: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := edgeCount(t, eng); got != 0 {
		t.Fatalf("rolled-back same-tx edge creates left count = %d, want 0", got)
	}
}
