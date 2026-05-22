package cypher_test

// multi_pattern_test.go — end-to-end execution tests for multi-pattern MATCH
// and OPTIONAL MATCH binding (task-392).
//
// The tests cover three scenarios that previously failed:
//
//  1. Multi-pattern MATCH with NO shared variable (Cartesian product) — verifies
//     that each pattern emits the correct number of rows and that variables do
//     not collide in the schema layout.
//  2. Multi-pattern MATCH WITH a shared variable — verifies that the shared
//     variable acts as a join key (only rows that agree on the shared
//     variable's value survive).
//  3. OPTIONAL MATCH that fails to match — verifies that an outer row is
//     preserved and the inner-only variable is bound to NULL, instead of the
//     outer row being silently dropped.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newDirectedTestGraph creates a directed labelled property graph for the tests.
func newDirectedTestGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{Directed: true})
}

// nodeID looks up the interned NodeID for a node name.
func nodeID(g *lpg.Graph[string, float64], name string) int64 {
	id, _ := g.AdjList().Mapper().Lookup(name)
	return int64(id)
}

// TestMultiPattern_CartesianProduct verifies that two unrelated patterns
// MATCH (a)-[]->(b), (c) return one row per (edge, c-node) combination, with
// each variable column carrying the correct value.
func TestMultiPattern_CartesianProduct(t *testing.T) {
	g := newDirectedTestGraph()
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("charlie"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddEdge("alice", "bob", 0); err != nil { // single edge alice → bob
		t.Fatalf("AddEdge: %v", err)
	}
	aliceID := nodeID(g, "alice")
	bobID := nodeID(g, "bob")
	charlieID := nodeID(g, "charlie")

	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), "MATCH (a)-[]->(b), (c) RETURN a, b, c", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var got []map[string]int64
	for res.Next() {
		rec := res.Record()
		got = append(got, map[string]int64{
			"a": valueAsInt64(rec["a"]),
			"b": valueAsInt64(rec["b"]),
			"c": valueAsInt64(rec["c"]),
		})
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}

	// Expected: 1 edge × 3 c-nodes = 3 rows.
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3 — rows: %v", len(got), got)
	}
	for i, row := range got {
		if row["a"] != aliceID {
			t.Errorf("row[%d].a = %d, want %d (aliceID)", i, row["a"], aliceID)
		}
		if row["b"] != bobID {
			t.Errorf("row[%d].b = %d, want %d (bobID)", i, row["b"], bobID)
		}
	}
	// c must be each of {alice, bob, charlie} exactly once.
	seenC := make(map[int64]bool)
	for _, row := range got {
		seenC[row["c"]] = true
	}
	for _, want := range []int64{aliceID, bobID, charlieID} {
		if !seenC[want] {
			t.Errorf("c column missing value %d; got %v", want, seenC)
		}
	}
}

// TestMultiPattern_SharedVariableJoin verifies that two patterns sharing a
// variable join on that variable's value, producing only the rows where the
// shared variable binds consistently.
func TestMultiPattern_SharedVariableJoin(t *testing.T) {
	g := newDirectedTestGraph()
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("charlie"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddEdge("alice", "bob", 0); err != nil { // a → b
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("bob", "charlie", 0); err != nil { // b → c
		t.Fatalf("AddEdge: %v", err)
	}
	aliceID := nodeID(g, "alice")
	bobID := nodeID(g, "bob")
	charlieID := nodeID(g, "charlie")

	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), "MATCH (a)-[]->(b), (b)-[]->(c) RETURN a, b, c", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var got []map[string]int64
	for res.Next() {
		rec := res.Record()
		got = append(got, map[string]int64{
			"a": valueAsInt64(rec["a"]),
			"b": valueAsInt64(rec["b"]),
			"c": valueAsInt64(rec["c"]),
		})
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}

	// Expected: exactly 1 row with a=alice, b=bob, c=charlie.
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 — rows: %v", len(got), got)
	}
	row := got[0]
	if row["a"] != aliceID {
		t.Errorf("row.a = %d, want %d (aliceID)", row["a"], aliceID)
	}
	if row["b"] != bobID {
		t.Errorf("row.b = %d, want %d (bobID)", row["b"], bobID)
	}
	if row["c"] != charlieID {
		t.Errorf("row.c = %d, want %d (charlieID)", row["c"], charlieID)
	}
}

// TestOptionalMatch_NullRowEmission verifies that OPTIONAL MATCH still produces
// one row per outer-matching node when the inner pattern fails to match: the
// inner-only variable is bound to NULL.
func TestOptionalMatch_NullRowEmission(t *testing.T) {
	g := newDirectedTestGraph()
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	// No relationships at all → the OPTIONAL MATCH (a)-[:R]->(b) must fail
	// for every a-node, and each row must survive with b = NULL.
	aliceID := nodeID(g, "alice")
	bobID := nodeID(g, "bob")

	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), "MATCH (a) OPTIONAL MATCH (a)-[:R]->(b) RETURN a, b", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var got []map[string]expr.Value
	for res.Next() {
		rec := res.Record()
		row := map[string]expr.Value{}
		if v, ok := rec["a"].(expr.Value); ok {
			row["a"] = v
		}
		if v, ok := rec["b"].(expr.Value); ok {
			row["b"] = v
		}
		got = append(got, row)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}

	// Expected: exactly 2 rows — one per a-node — with b = NULL in each.
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 — rows: %v", len(got), got)
	}
	seen := make(map[int64]bool)
	for i, row := range got {
		aVal, ok := row["a"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("row[%d].a is not IntegerValue: %v", i, row["a"])
		}
		seen[int64(aVal)] = true
		if row["b"] != expr.Null {
			t.Errorf("row[%d].b = %v, want NULL", i, row["b"])
		}
	}
	if !seen[aliceID] || !seen[bobID] {
		t.Errorf("missing a-rows: aliceID=%d, bobID=%d, seen=%v", aliceID, bobID, seen)
	}
}

// TestOptionalMatch_MatchedRow verifies that an OPTIONAL MATCH which DOES match
// emits the joined row (NOT the NULL row).
func TestOptionalMatch_MatchedRow(t *testing.T) {
	g := newDirectedTestGraph()
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	aliceID := nodeID(g, "alice")
	bobID := nodeID(g, "bob")

	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), "MATCH (a) OPTIONAL MATCH (a)-[]->(b) RETURN a, b", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	type pair struct{ a, b expr.Value }
	var rows []pair
	for res.Next() {
		rec := res.Record()
		rows = append(rows, pair{
			a: rec["a"].(expr.Value),
			b: rec["b"].(expr.Value),
		})
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter: %v", err)
	}
	// Expected: 2 rows. alice's row has b=bob; bob's row has b=NULL.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 — rows: %v", len(rows), rows)
	}
	var aliceRow, bobRow pair
	for _, r := range rows {
		if int64(r.a.(expr.IntegerValue)) == aliceID {
			aliceRow = r
		} else {
			bobRow = r
		}
	}
	if int64(aliceRow.b.(expr.IntegerValue)) != bobID {
		t.Errorf("aliceRow.b = %v, want %d (bobID)", aliceRow.b, bobID)
	}
	if bobRow.b != expr.Null {
		t.Errorf("bobRow.b = %v, want NULL", bobRow.b)
	}
}

// valueAsInt64 extracts an int64 from an [expr.IntegerValue] returned as the
// generic value type by the result set. The helper makes test assertions easier
// to read.
func valueAsInt64(v any) int64 {
	if iv, ok := v.(expr.IntegerValue); ok {
		return int64(iv)
	}
	return -1
}
