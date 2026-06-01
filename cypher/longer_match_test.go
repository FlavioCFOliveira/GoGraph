package cypher_test

// longer_match_test.go — additional end-to-end tests for multi-pattern and
// OPTIONAL MATCH scenarios drawn from the openCypher TCK.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestMatch7_BoundNodesWithoutMatches mirrors Match7 scenario [8]:
//
//	MATCH (a:A), (c:C)
//	OPTIONAL MATCH (a)-->(b)-->(c)
//	RETURN b
//
// With a:A, b:B, c:C and a-->c (no a-->b-->c), the OPTIONAL MATCH fails and
// the row must survive with b=NULL.
func TestMatch7_BoundNodesWithoutMatches(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := g.AddNode("s"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("c"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("s", "Single"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("a", "A"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("b", "B"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("c", "C"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.AddEdge("s", "a", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("s", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("a", "c", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("b", "b", 0); err != nil { // self-loop on b
		t.Fatalf("AddEdge: %v", err)
	}

	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(),
		"MATCH (a:A), (c:C) OPTIONAL MATCH (a)-->(b)-->(c) RETURN b", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var rows []expr.Value
	for res.Next() {
		rec := res.Record()
		rows = append(rows, rec["b"].(expr.Value))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}
	// There is exactly one (a:A, c:C) pair, and no (a)-->(b)-->(c) path
	// passes through b:B (the only intermediate is via a-->c directly, no
	// two-hop path). So we expect 1 row with b = NULL.
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 — rows: %v", len(rows), rows)
	}
	if rows[0] != expr.Null {
		t.Errorf("rows[0] = %v, want NULL", rows[0])
	}
}
