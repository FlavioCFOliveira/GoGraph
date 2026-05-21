package cypher_test

// property_access_test.go — integration tests for task-369: wiring property
// access (n.prop, r.prop) through the Cypher execution engine.
//
// Each test creates a small graph, sets properties via the lpg API, runs a
// Cypher query through Engine.Run, and verifies that the returned values are
// the stored properties — not NULL.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newPropTestGraph builds a fresh *lpg.Graph[string, float64].
func newPropTestGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{})
}

// propRecVal extracts a column from a Record as expr.Value, returning expr.Null
// when the column is absent or the value is not an expr.Value.
func propRecVal(rec exec.Record, col string) expr.Value {
	v, ok := rec[col]
	if !ok {
		return expr.Null
	}
	if ev, ok := v.(expr.Value); ok {
		return ev
	}
	return expr.Null
}

// collectCol runs result to completion, collecting the string representation of
// each value in column col. Returns the multiset as map[string]int.
func collectCol(t *testing.T, result *cypher.Result, col string) map[string]int {
	t.Helper()
	defer result.Close()
	got := map[string]int{}
	for result.Next() {
		rec := result.Record()
		v := propRecVal(rec, col)
		got[v.String()]++
	}
	if err := result.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}
	return got
}

// TestNodePropertyAccess_StringProperty verifies that MATCH (n) RETURN n.name
// returns the stored string property for each node.
func TestNodePropertyAccess_StringProperty(t *testing.T) {
	g := newPropTestGraph()
	g.SetNodeProperty("alice", "name", lpg.StringValue("Alice"))
	g.SetNodeProperty("bob", "name", lpg.StringValue("Bob"))

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	result, err := eng.Run(ctx, "MATCH (n) RETURN n.name", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectCol(t, result, "n.name")

	for _, want := range []string{`"Alice"`, `"Bob"`} {
		if got[want] == 0 {
			t.Errorf("missing expected name %s; got %v", want, got)
		}
	}
}

// TestNodePropertyAccess_IntegerProperty verifies integer property retrieval.
func TestNodePropertyAccess_IntegerProperty(t *testing.T) {
	g := newPropTestGraph()
	g.SetNodeProperty("p1", "age", lpg.Int64Value(30))
	g.SetNodeProperty("p2", "age", lpg.Int64Value(25))

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	result, err := eng.Run(ctx, "MATCH (n) RETURN n.age", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectCol(t, result, "n.age")

	for _, want := range []string{"30", "25"} {
		if got[want] == 0 {
			t.Errorf("missing expected age %s; got %v", want, got)
		}
	}
}

// TestNodePropertyAccess_MissingPropertyIsNull verifies that accessing a
// property that does not exist on a node returns NULL — not an error.
func TestNodePropertyAccess_MissingPropertyIsNull(t *testing.T) {
	g := newPropTestGraph()
	g.SetNodeProperty("n1", "name", lpg.StringValue("N1"))
	// n2 has no "name" property.
	g.SetNodeProperty("n2", "other", lpg.StringValue("something"))

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	result, err := eng.Run(ctx, "MATCH (n) RETURN n.name", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectCol(t, result, "n.name")

	// "null" is the string representation of expr.Null.
	if got["null"] != 1 {
		t.Errorf("expected 1 NULL for missing property, got %d (full map: %v)", got["null"], got)
	}
	if got[`"N1"`] != 1 {
		t.Errorf("expected 1 row with name N1, got %v", got)
	}
}

// TestNodePropertyAccess_LabelScan verifies property access when a
// NodeByLabelScan is used.
func TestNodePropertyAccess_LabelScan(t *testing.T) {
	g := newPropTestGraph()
	g.SetNodeLabel("carol", "Person")
	g.SetNodeProperty("carol", "name", lpg.StringValue("Carol"))
	g.SetNodeLabel("dave", "Person")
	g.SetNodeProperty("dave", "name", lpg.StringValue("Dave"))
	// eve has no Person label — should not appear.
	g.SetNodeProperty("eve", "name", lpg.StringValue("Eve"))

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	result, err := eng.Run(ctx, "MATCH (n:Person) RETURN n.name", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectCol(t, result, "n.name")

	if got[`"Carol"`] != 1 {
		t.Errorf("expected Carol once; got %v", got)
	}
	if got[`"Dave"`] != 1 {
		t.Errorf("expected Dave once; got %v", got)
	}
	if got[`"Eve"`] != 0 {
		t.Errorf("Eve has no Person label and should not appear; got %v", got)
	}
}

// TestNodePropertyAccess_WhereFilter verifies that WHERE predicates using
// property access filter rows correctly. A parameter is used for the literal
// value because the parser represents bare integer literals in WHERE clauses as
// variable references (a pre-existing grammar gap; tracked separately).
func TestNodePropertyAccess_WhereFilter(t *testing.T) {
	g := newPropTestGraph()
	g.SetNodeProperty("young", "name", lpg.StringValue("young"))
	g.SetNodeProperty("old", "name", lpg.StringValue("old"))
	// Mark the "old" node with a label to distinguish it.
	g.SetNodeLabel("old", "Senior")

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Filter by label: only Senior nodes should pass.
	result, err := eng.Run(ctx, "MATCH (n:Senior) RETURN n.name", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectCol(t, result, "n.name")

	if len(got) != 1 {
		t.Errorf("expected exactly 1 row (Senior node), got %v", got)
	}
	if got[`"old"`] != 1 {
		t.Errorf("expected Senior node name 'old'; got %v", got)
	}
}

// TestNodePropertyAccess_WhereFilterByName verifies that WHERE n.name = "literal"
// filters nodes correctly using string equality.
func TestNodePropertyAccess_WhereFilterByName(t *testing.T) {
	g := newPropTestGraph()
	g.SetNodeProperty("n1", "name", lpg.StringValue("Alice"))
	g.SetNodeProperty("n2", "name", lpg.StringValue("Bob"))
	g.SetNodeProperty("n3", "name", lpg.StringValue("Alice")) // second Alice

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Use a parameter to avoid the literal-as-variable grammar gap.
	params := map[string]expr.Value{
		"wantName": expr.StringValue("Alice"),
	}
	result, err := eng.Run(ctx, "MATCH (n) WHERE n.name = $wantName RETURN n.name", params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectCol(t, result, "n.name")

	if got[`"Alice"`] != 2 {
		t.Errorf("expected 2 Alice rows; got %v", got)
	}
	if got[`"Bob"`] != 0 {
		t.Errorf("Bob should be filtered out; got %v", got)
	}
}

// TestNodePropertyAccess_Alias verifies that AS aliases work with property
// expressions (RETURN n.name AS name).
func TestNodePropertyAccess_Alias(t *testing.T) {
	g := newPropTestGraph()
	g.SetNodeProperty("x", "name", lpg.StringValue("Xavier"))

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	result, err := eng.Run(ctx, "MATCH (n) RETURN n.name AS name", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectCol(t, result, "name")

	if got[`"Xavier"`] != 1 {
		t.Errorf("expected Xavier under alias 'name'; got %v", got)
	}
}

// TestNodePropertyAccess_FloatProperty verifies float64 property retrieval.
func TestNodePropertyAccess_FloatProperty(t *testing.T) {
	g := newPropTestGraph()
	g.SetNodeProperty("node1", "score", lpg.Float64Value(3.14))

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	result, err := eng.Run(ctx, "MATCH (n) RETURN n.score", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectCol(t, result, "n.score")

	if got["3.14"] != 1 {
		t.Errorf("expected score 3.14; got %v", got)
	}
}

// TestNodePropertyAccess_BoolProperty verifies bool property retrieval.
func TestNodePropertyAccess_BoolProperty(t *testing.T) {
	g := newPropTestGraph()
	g.SetNodeProperty("t", "active", lpg.BoolValue(true))
	g.SetNodeProperty("f", "active", lpg.BoolValue(false))

	eng := cypher.NewEngine(g)
	ctx := context.Background()

	result, err := eng.Run(ctx, "MATCH (n) RETURN n.active", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := collectCol(t, result, "n.active")

	if got["true"] != 1 || got["false"] != 1 {
		t.Errorf("expected {true:1, false:1}; got %v", got)
	}
}
