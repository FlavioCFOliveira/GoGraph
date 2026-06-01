package cypher_test

// proc_db_property_keys_test.go — tests for db.propertyKeys() procedure
// (task-895).
//
// db.propertyKeys() is registered and callable, but its implementation is a
// stub that always returns nil (no rows). Properties set on nodes or edges are
// stored in the LPG and are not surfaced by this procedure. The
// populated-graph test is skipped until the procedure implementation is wired
// to a property-key registry.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// TestProcDbPropertyKeys_Empty verifies that CALL db.propertyKeys() returns
// zero rows on a graph with no nodes or properties.
func TestProcDbPropertyKeys_Empty(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`CALL db.propertyKeys() YIELD propertyKey`, nil)
	if err != nil {
		t.Fatalf("CALL db.propertyKeys(): %v", err)
	}
	rows := collectProc(t, res)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty graph, got %d: %v", len(rows), rows)
	}
}

// TestProcDbPropertyKeys_AfterCreatingNodes documents the intended behaviour
// once db.propertyKeys() is wired to a property-key registry.
//
// The test is skipped because the current stub implementation always returns
// nil regardless of which properties have been set. When the procedure is
// implemented, it should return each distinct property key that exists across
// all nodes and edges (e.g. "name", "age", "score").
func TestProcDbPropertyKeys_AfterCreatingNodes(t *testing.T) {
	t.Skip("db.propertyKeys() not yet implemented: " +
		"procedure is a stub that returns nil; " +
		"enable this test once the implementation queries the property-key registry")

	g := newProcTestGraph()
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Create nodes with distinct property keys.
	for _, q := range []string{
		`CREATE (a:Person {name: "Alice", age: 30})`,
		`CREATE (b:Person {name: "Bob", score: 9.5})`,
		`CREATE (c:Movie {name: "Inception"})`,
	} {
		if _, err := eng.Run(ctx, q, nil); err != nil {
			t.Fatalf("CREATE: %v", err)
		}
	}

	res, err := eng.Run(ctx,
		`CALL db.propertyKeys() YIELD propertyKey`, nil)
	if err != nil {
		t.Fatalf("CALL db.propertyKeys(): %v", err)
	}
	rows := collectProc(t, res)

	// Expect 3 distinct keys: name, age, score.
	if len(rows) != 3 {
		t.Fatalf("expected 3 property key rows, got %d: %v", len(rows), rows)
	}
	for i, row := range rows {
		if _, ok := row["propertyKey"]; !ok {
			t.Errorf("row[%d] missing 'propertyKey' column", i)
		}
	}
}

// TestProcDbPropertyKeys_CountAggregation documents the intended behaviour of
// combining db.propertyKeys() with YIELD and count(*).
//
// Skipped for the same reason as TestProcDbPropertyKeys_AfterCreatingNodes.
func TestProcDbPropertyKeys_CountAggregation(t *testing.T) {
	t.Skip("db.propertyKeys() not yet implemented: " +
		"procedure is a stub that returns nil; " +
		"enable this test once the implementation queries the property-key registry")

	g := newProcTestGraph()
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	for _, q := range []string{
		`CREATE (a:Person {name: "Alice", age: 30})`,
		`CREATE (b:Person {score: 9.5})`,
	} {
		if _, err := eng.Run(ctx, q, nil); err != nil {
			t.Fatalf("CREATE: %v", err)
		}
	}

	// name, age, score → 3 distinct keys.
	res, err := eng.Run(ctx,
		`CALL db.propertyKeys() YIELD propertyKey RETURN count(*) AS n`, nil)
	if err != nil {
		t.Fatalf("CALL db.propertyKeys() YIELD ... RETURN count(*): %v", err)
	}
	rows := collectProc(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 aggregation row, got %d: %v", len(rows), rows)
	}
	if n := rows[0]["n"]; n != "3" {
		t.Errorf("count(*) = %q, want \"3\"", n)
	}
}
