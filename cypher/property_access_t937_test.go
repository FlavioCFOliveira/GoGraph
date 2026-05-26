package cypher_test

// property_access_t937_test.go — T937 diagnostic
//
// Exercises the property-access binding path with concrete queries and
// asserts the returned scalar shape matches the openCypher contract.
// These tests are diagnostics for the T937 closure work: they pin the
// current behaviour so the gap to the spec is visible.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestT937_PropertyAccess_BasicReturn checks that MATCH (n) RETURN n.name
// resolves the property from the bound node instead of producing null.
func TestT937_PropertyAccess_BasicReturn(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:Person {name: "Alice", age: 30})`)

	res, err := eng.Run(ctx, `MATCH (n:Person) RETURN n.name AS name, n.age AS age`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = res.Close() }()

	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if got := fmtAny(row["name"]); got != `"Alice"` {
		t.Errorf("row[name] = %s, want %q", got, "Alice")
	}
	if got := fmtAny(row["age"]); got != "30" {
		t.Errorf("row[age] = %s, want 30", got)
	}
}

// TestT937_PropertyAccess_WhereFilter checks that WHERE n.prop = lit works.
func TestT937_PropertyAccess_WhereFilter(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (a:Person {name: "Alice", age: 30})`)
	drainRunInTx(t, eng, `CREATE (b:Person {name: "Bob", age: 40})`)

	res, err := eng.Run(ctx, `MATCH (n:Person) WHERE n.age > 35 RETURN n.name AS name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = res.Close() }()

	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := fmtAny(rows[0]["name"]); got != `"Bob"` {
		t.Errorf("row[name] = %s, want %q", got, "Bob")
	}
}

// TestT937_PropertyAccess_RelProperty checks that r.prop works for
// relationships.
func TestT937_PropertyAccess_RelProperty(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (a:Person {name: "Alice"})-[r:KNOWS {since: 2020}]->(b:Person {name: "Bob"})`)

	res, err := eng.Run(ctx, `MATCH (a)-[r:KNOWS]->(b) RETURN r.since AS since`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = res.Close() }()

	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %v", len(rows), rows)
	}
	if got := fmtAny(rows[0]["since"]); got != "2020" {
		t.Errorf("row[since] = %s, want 2020", got)
	}
}

// TestT937_PropertyAccess_RelReturnBare verifies that `RETURN r` produces
// a full relationship value (not just the edge ID integer).
func TestT937_PropertyAccess_RelReturnBare(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (a:Person {name: "Alice"})-[r:KNOWS {since: 2020}]->(b:Person {name: "Bob"})`)

	res, err := eng.Run(ctx, `MATCH ()-[r:KNOWS]->() RETURN r`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = res.Close() }()

	rows := drainRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %v", len(rows), rows)
	}
	// `r` should render as `[:KNOWS {since: 2020}]` not an integer.
	got := fmtAny(rows[0]["r"])
	if got == "0" || got == "1" {
		t.Errorf("row[r] = %s, want a relationship value", got)
	}
}
