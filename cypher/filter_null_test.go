package cypher_test

// filter_null_test.go — T664
//
// Integration tests for WHERE clauses involving NULL three-valued logic:
//   - IS NULL / IS NOT NULL predicates
//   - Equality with null literal (always evaluates to NULL → filtered out)
//   - Comparison with a missing property (NULL > N = NULL = not truthy)
//
// Graph: two nodes — "alice" has an age property; "bob" does not.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newNullTestGraph builds a graph with two Person nodes:
//   - "alice" with age=30
//   - "bob" with no age property
func newNullTestGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	for _, q := range []string{
		`CREATE (n:Person {name: 'alice', age: 30})`,
		`CREATE (n:Person {name: 'bob'})`,
	} {
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("newNullTestGraph CREATE: %v", err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("newNullTestGraph drain: %v", err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("newNullTestGraph close: %v", err)
		}
	}
	return g
}

// singleName drains res and returns the one string name it contains. Fails if
// the result does not contain exactly one row with a string n.name column.
func singleName(t *testing.T, res *cypher.Result, col string) string {
	t.Helper()
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	sv, ok := rows[0][col].(expr.StringValue)
	if !ok {
		t.Fatalf("%s: expected StringValue, got %T (%v)", col, rows[0][col], rows[0][col])
	}
	return string(sv)
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. IS NULL — node with missing property
// ─────────────────────────────────────────────────────────────────────────────

// TestFilterNull_IsNull verifies that IS NULL matches only the node whose age
// property is absent ("bob").
func TestFilterNull_IsNull(t *testing.T) {
	g := newNullTestGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) WHERE n.age IS NULL RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := singleName(t, res, "n.name")
	if got != "bob" {
		t.Errorf("IS NULL: name = %q, want %q", got, "bob")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. IS NOT NULL — node with the property present
// ─────────────────────────────────────────────────────────────────────────────

// TestFilterNull_IsNotNull verifies that IS NOT NULL matches only the node
// that has an age value ("alice").
func TestFilterNull_IsNotNull(t *testing.T) {
	g := newNullTestGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) WHERE n.age IS NOT NULL RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := singleName(t, res, "n.name")
	if got != "alice" {
		t.Errorf("IS NOT NULL: name = %q, want %q", got, "alice")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Equality with null literal — three-valued logic returns no rows
// ─────────────────────────────────────────────────────────────────────────────

// TestFilterNull_EqualNull verifies that WHERE n.age = null returns zero rows.
// Per Cypher three-valued logic, NULL = NULL evaluates to NULL (not TRUE), so
// no row passes the filter.
func TestFilterNull_EqualNull(t *testing.T) {
	g := newNullTestGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) WHERE n.age = null RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 0 {
		t.Errorf("WHERE n.age = null: expected 0 rows, got %d: %v", len(rows), rows)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Comparison with absent property — NULL propagation
// ─────────────────────────────────────────────────────────────────────────────

// TestFilterNull_CompareWithMissing verifies that WHERE n.age > 25 returns
// only "alice". "bob" has no age; NULL > 25 evaluates to NULL which is not
// truthy, so that row is excluded.
func TestFilterNull_CompareWithMissing(t *testing.T) {
	g := newNullTestGraph(t)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) WHERE n.age > 25 RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := singleName(t, res, "n.name")
	if got != "alice" {
		t.Errorf("WHERE n.age > 25: name = %q, want %q", got, "alice")
	}
}
