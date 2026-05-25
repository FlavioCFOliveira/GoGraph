package cypher_test

// filter_property_test.go — T660
//
// Integration tests for WHERE clauses with property predicates: greater-than,
// equality, and inequality filters on integer-valued node properties.
// All nodes are created through the engine so labels and properties are set
// correctly via the full write pipeline.

import (
	"context"
	"fmt"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newPersonAgeGraph creates a graph with n Person nodes whose ages are
// 10, 20, 30, … (multiples of 10). Names are "p1", "p2", ….
func newPersonAgeGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for i := 1; i <= n; i++ {
		q := fmt.Sprintf(`CREATE (n:Person {name: 'p%d', age: %d})`, i, i*10)
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("newPersonAgeGraph CREATE i=%d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("newPersonAgeGraph drain i=%d: %v", i, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("newPersonAgeGraph close i=%d: %v", i, err)
		}
	}
	return g
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Greater-than filter
// ─────────────────────────────────────────────────────────────────────────────

// TestFilterProperty_GreaterThan verifies that WHERE n.age > 25 returns only
// the three Person nodes whose ages are 30, 40, and 50.
func TestFilterProperty_GreaterThan(t *testing.T) {
	g := newPersonAgeGraph(t, 5) // ages: 10, 20, 30, 40, 50
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n:Person) WHERE n.age > 25 RETURN n.name, n.age`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (age 30, 40, 50), got %d: %v", len(rows), rows)
	}
	for _, row := range rows {
		iv, ok := row["n.age"].(expr.IntegerValue)
		if !ok {
			t.Errorf("n.age: expected IntegerValue, got %T (%v)", row["n.age"], row["n.age"])
			continue
		}
		if int64(iv) <= 25 {
			t.Errorf("expected age > 25, got %d", int64(iv))
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Equality filter
// ─────────────────────────────────────────────────────────────────────────────

// TestFilterProperty_Equality verifies that WHERE n.age = 30 returns exactly
// one row.
func TestFilterProperty_Equality(t *testing.T) {
	g := newPersonAgeGraph(t, 5)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n:Person) WHERE n.age = 30 RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 row (age = 30), got %d: %v", len(rows), rows)
	}
	sv, ok := rows[0]["n.name"].(expr.StringValue)
	if !ok {
		t.Fatalf("n.name: expected StringValue, got %T", rows[0]["n.name"])
	}
	if string(sv) != "p3" {
		t.Errorf("name = %q, want %q", string(sv), "p3")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Inequality filter with count(*)
// ─────────────────────────────────────────────────────────────────────────────

// TestFilterProperty_Inequality verifies that WHERE n.age <> 30 excludes
// exactly one row, so count(*) returns 4.
func TestFilterProperty_Inequality(t *testing.T) {
	g := newPersonAgeGraph(t, 5)
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n:Person) WHERE n.age <> 30 RETURN count(*) AS cnt`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("expected 1 aggregation row, got %d", len(rows))
	}
	iv, ok := rows[0]["cnt"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("cnt: expected IntegerValue, got %T (%v)", rows[0]["cnt"], rows[0]["cnt"])
	}
	if int64(iv) != 4 {
		t.Errorf("count(*) with age <> 30 = %d, want 4", int64(iv))
	}
}
