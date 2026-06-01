package cypher_test

// proc_db_relationship_types_test.go — tests for db.relationshipTypes()
// procedure (task-894).
//
// db.relationshipTypes() is registered and callable, but its implementation is
// a stub that always returns nil (no rows). Edge labels set via
// g.SetEdgeLabel or Cypher CREATE are stored in the LPG's internal edgeIdx
// and are not surfaced by this procedure. The populated-graph test is skipped
// until the procedure implementation is wired to a data source.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// TestProcDbRelationshipTypes_Empty verifies that CALL db.relationshipTypes()
// returns zero rows on a graph with no edges.
func TestProcDbRelationshipTypes_Empty(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`CALL db.relationshipTypes() YIELD relationshipType`, nil)
	if err != nil {
		t.Fatalf("CALL db.relationshipTypes(): %v", err)
	}
	rows := collectProc(t, res)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty graph, got %d: %v", len(rows), rows)
	}
}

// TestProcDbRelationshipTypes_AfterCreatingEdges documents the intended
// behaviour once db.relationshipTypes() is wired to the edge-label data source.
//
// The test is skipped because the current stub implementation always returns
// nil regardless of which edges exist in the graph. When the procedure is
// implemented, it should return each distinct relationship type registered via
// SetEdgeLabel (stored in the LPG's internal edgeIdx) or via Cypher CREATE.
func TestProcDbRelationshipTypes_AfterCreatingEdges(t *testing.T) {
	t.Skip("db.relationshipTypes() not yet implemented: " +
		"procedure is a stub that returns nil; " +
		"enable this test once the implementation queries the edge-label registry")

	g := newProcTestGraph()
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Create nodes and typed edges.
	for _, q := range []string{
		`CREATE (a:Person {name: "Alice"})`,
		`CREATE (b:Person {name: "Bob"})`,
		`CREATE (c:Company {name: "Acme"})`,
	} {
		if _, err := eng.Run(ctx, q, nil); err != nil {
			t.Fatalf("CREATE node: %v", err)
		}
	}

	// Create edges with distinct relationship types.
	for _, q := range []string{
		`MATCH (a:Person {name: "Alice"}), (b:Person {name: "Bob"}) CREATE (a)-[:KNOWS]->(b)`,
		`MATCH (a:Person {name: "Alice"}), (b:Person {name: "Bob"}) CREATE (a)-[:LIKES]->(b)`,
		`MATCH (a:Person {name: "Alice"}), (c:Company {name: "Acme"}) CREATE (a)-[:WORKS_WITH]->(c)`,
	} {
		if _, err := eng.Run(ctx, q, nil); err != nil {
			t.Fatalf("CREATE edge: %v", err)
		}
	}

	res, err := eng.Run(ctx,
		`CALL db.relationshipTypes() YIELD relationshipType`, nil)
	if err != nil {
		t.Fatalf("CALL db.relationshipTypes(): %v", err)
	}
	rows := collectProc(t, res)

	if len(rows) != 3 {
		t.Fatalf("expected 3 relationship type rows, got %d: %v", len(rows), rows)
	}
	for i, row := range rows {
		if _, ok := row["relationshipType"]; !ok {
			t.Errorf("row[%d] missing 'relationshipType' column", i)
		}
	}
}
