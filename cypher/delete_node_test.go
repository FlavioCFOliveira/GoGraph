package cypher_test

// delete_node_test.go — additive DELETE single-node tests (T840).
//
// Existing TestRunInTx_DeleteNode_Isolated covers: create an isolated node,
// DELETE it, verify the label index is empty. Tests here cover:
//   - verifying the deleted node is gone via a subsequent MATCH
//   - deleting a node that carried properties (properties also gone)
//   - deleting 3 nodes in a single transaction

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestDelete_NodeGoneAfterDelete creates a :Target node, deletes it, then
// confirms a subsequent MATCH returns zero rows.
func TestDelete_NodeGoneAfterDelete(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Target)`)
	drainRunInTx(t, eng, `MATCH (n:Target) DELETE n`)

	res, err := eng.RunInTx(context.Background(), `MATCH (n:Target) RETURN count(*) AS cnt`, nil)
	if err != nil {
		t.Fatalf("MATCH after DELETE: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 aggregation row, got %d", len(rows))
	}
	cnt, ok := rows[0]["cnt"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("cnt: expected IntegerValue, got %T", rows[0]["cnt"])
	}
	if int64(cnt) != 0 {
		t.Errorf("MATCH (n:Target) count = %d after DELETE, want 0", int64(cnt))
	}
}

// TestDelete_NodeWithPropertiesGone creates a node with properties, deletes
// it, and verifies the properties are no longer accessible.
func TestDelete_NodeWithPropertiesGone(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (n:Rich {name: "data", score: 100})`)
	drainRunInTx(t, eng, `MATCH (n:Rich) DELETE n`)

	// Verify via MATCH: no Rich node found.
	res, err := eng.RunInTx(context.Background(), `MATCH (n:Rich) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("MATCH after DELETE: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows after DELETE of Rich node, got %d", len(rows))
	}
}

// TestDelete_MultipleNodesOneTransaction creates three isolated nodes with
// a shared label and deletes all three in a single MATCH … DELETE.
func TestDelete_MultipleNodesOneTransaction(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	for range 3 {
		drainRunInTx(t, eng, `CREATE (n:Batch)`)
	}

	// Verify three nodes exist before DELETE.
	preRes, err := eng.RunInTx(context.Background(), `MATCH (n:Batch) RETURN count(*) AS cnt`, nil)
	if err != nil {
		t.Fatalf("pre-DELETE count: %v", err)
	}
	preRows := collectRecords(t, preRes)
	if preCnt, ok := preRows[0]["cnt"].(expr.IntegerValue); !ok || int64(preCnt) != 3 {
		t.Fatalf("expected 3 Batch nodes before DELETE, got %v", preRows[0]["cnt"])
	}

	// Delete all three in one statement.
	drainRunInTx(t, eng, `MATCH (n:Batch) DELETE n`)

	// All must be gone.
	postRes, err := eng.RunInTx(context.Background(), `MATCH (n:Batch) RETURN count(*) AS cnt`, nil)
	if err != nil {
		t.Fatalf("post-DELETE count: %v", err)
	}
	postRows := collectRecords(t, postRes)
	if len(postRows) != 1 {
		t.Fatalf("expected 1 aggregation row, got %d", len(postRows))
	}
	cnt, ok := postRows[0]["cnt"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("cnt: expected IntegerValue, got %T", postRows[0]["cnt"])
	}
	if int64(cnt) != 0 {
		t.Errorf("count after DELETE = %d, want 0", int64(cnt))
	}
}
