package cypher_test

// project_expressions_test.go — T670
//
// Integration tests for RETURN projections: multi-column projections,
// column aliases, arithmetic expressions, and duplicate column references.
//
// Graph: a single Person node with name="Alice" and age=30.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newSinglePersonGraph creates a graph with one Person node: name="Alice", age=30.
func newSinglePersonGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	res, err := eng.RunInTxAny(context.Background(),
		`CREATE (n:Person {name: 'Alice', age: 30})`, nil)
	if err != nil {
		t.Fatalf("newSinglePersonGraph CREATE: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("newSinglePersonGraph drain: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("newSinglePersonGraph close: %v", err)
	}
	return g
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Multi-column projection with arithmetic literal expression
// ─────────────────────────────────────────────────────────────────────────────

// TestProjectExpressions_MultiColumn verifies that all three projected columns
// (n.name, n.age, and the constant 1+2 aliased as "three") are present in the
// result record with the correct values.
func TestProjectExpressions_MultiColumn(t *testing.T) {
	g := newSinglePersonGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name, n.age, 1+2 AS three`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]

	name, ok := row["n.name"].(expr.StringValue)
	if !ok {
		t.Errorf("n.name: expected StringValue, got %T (%v)", row["n.name"], row["n.name"])
	} else if string(name) != "Alice" {
		t.Errorf("n.name = %q, want %q", string(name), "Alice")
	}

	age, ok := row["n.age"].(expr.IntegerValue)
	if !ok {
		t.Errorf("n.age: expected IntegerValue, got %T (%v)", row["n.age"], row["n.age"])
	} else if int64(age) != 30 {
		t.Errorf("n.age = %d, want 30", int64(age))
	}

	three, ok := row["three"].(expr.IntegerValue)
	if !ok {
		t.Errorf("three: expected IntegerValue, got %T (%v)", row["three"], row["three"])
	} else if int64(three) != 3 {
		t.Errorf("1+2 = %d, want 3", int64(three))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Column alias
// ─────────────────────────────────────────────────────────────────────────────

// TestProjectExpressions_Alias verifies that RETURN n.name AS alias exposes
// the value under the alias key and not under "n.name".
func TestProjectExpressions_Alias(t *testing.T) {
	g := newSinglePersonGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name AS alias`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]

	if _, present := row["n.name"]; present {
		t.Error("column 'n.name' should not appear when aliased to 'alias'")
	}
	sv, ok := row["alias"].(expr.StringValue)
	if !ok {
		t.Fatalf("alias: expected StringValue, got %T (%v)", row["alias"], row["alias"])
	}
	if string(sv) != "Alice" {
		t.Errorf("alias = %q, want %q", string(sv), "Alice")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Arithmetic expression on a property
// ─────────────────────────────────────────────────────────────────────────────

// TestProjectExpressions_ArithmeticMultiply verifies that n.age * 2 produces
// the correct doubled value in the "doubled" column.
func TestProjectExpressions_ArithmeticMultiply(t *testing.T) {
	g := newSinglePersonGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.age * 2 AS doubled`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	iv, ok := rows[0]["doubled"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("doubled: expected IntegerValue, got %T (%v)", rows[0]["doubled"], rows[0]["doubled"])
	}
	if int64(iv) != 60 {
		t.Errorf("n.age * 2 = %d, want 60", int64(iv))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Same expression projected twice with an alias
// ─────────────────────────────────────────────────────────────────────────────

// TestProjectExpressions_DuplicateColumn verifies that RETURN n.name, n.name AS
// also_name produces two separate columns with the same value.
func TestProjectExpressions_DuplicateColumn(t *testing.T) {
	g := newSinglePersonGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) RETURN n.name, n.name AS also_name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]

	orig, ok := row["n.name"].(expr.StringValue)
	if !ok {
		t.Fatalf("n.name: expected StringValue, got %T", row["n.name"])
	}
	alias, ok := row["also_name"].(expr.StringValue)
	if !ok {
		t.Fatalf("also_name: expected StringValue, got %T", row["also_name"])
	}
	if string(orig) != "Alice" || string(alias) != "Alice" {
		t.Errorf("n.name=%q also_name=%q, want both %q", string(orig), string(alias), "Alice")
	}
}
