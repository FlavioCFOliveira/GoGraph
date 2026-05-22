package cypher_test

// return_node_shape_test.go — regression test for rmp Sprint 57 task #503.
//
// Asserts that `RETURN u` for a bound node variable emits a full
// [expr.NodeValue] (carrying ID, Labels, Properties), matching the shape
// documented in examples/24_social_network_cli/doc.go and the
// neo4j-go-driver convention. Before the fix in cypher/api.go's projection
// fast path, the bare-variable closure returned the row cell verbatim —
// an [expr.IntegerValue] holding the raw NodeID — so the documented
// NodeValue serialisation was unreachable in practice.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestEngine_ReturnBareNodeVariable_EmitsNodeValue verifies that a Cypher
// query of the shape `MATCH (u:Label {prop: x}) RETURN u` produces a
// NodeValue record cell whose ID, Labels and Properties match the node
// that was matched.
func TestEngine_ReturnBareNodeVariable_EmitsNodeValue(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("alice", "User"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("alice", "username", lpg.StringValue("alice")); err != nil {
		t.Fatalf("SetNodeProperty(username): %v", err)
	}
	if err := g.SetNodeProperty("alice", "age", lpg.Int64Value(30)); err != nil {
		t.Fatalf("SetNodeProperty(age): %v", err)
	}

	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), `MATCH (u:User {username: "alice"}) RETURN u`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var records []map[string]any
	for res.Next() {
		rec := res.Record()
		cp := make(map[string]any, len(rec))
		for k, v := range rec {
			cp[k] = v
		}
		records = append(records, cp)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	uVal, ok := records[0]["u"]
	if !ok {
		t.Fatalf("record missing column %q (have %v)", "u", records[0])
	}
	nv, ok := uVal.(expr.NodeValue)
	if !ok {
		t.Fatalf("RETURN u must emit expr.NodeValue, got %T (%v)", uVal, uVal)
	}
	if nv.ID == 0 {
		t.Errorf("NodeValue.ID is zero")
	}
	wantLabel := "User"
	if !containsString(nv.Labels, wantLabel) {
		t.Errorf("NodeValue.Labels = %v, want a slice containing %q", nv.Labels, wantLabel)
	}
	if got, ok := nv.Properties["username"].(expr.StringValue); !ok || string(got) != "alice" {
		t.Errorf("NodeValue.Properties[%q] = %v, want StringValue(%q)", "username", nv.Properties["username"], "alice")
	}
	if got, ok := nv.Properties["age"].(expr.IntegerValue); !ok || int64(got) != 30 {
		t.Errorf("NodeValue.Properties[%q] = %v, want IntegerValue(30)", "age", nv.Properties["age"])
	}
}

// TestEngine_ReturnAggregateAlias_NotUpgraded guards the regression that
// surfaced while narrowing the fix: an aggregate alias whose value
// numerically collides with a real NodeID (e.g. count(*) returning 7 when
// node 7 exists) must NOT be upgraded into the NodeValue corresponding to
// that NodeID. The schema-name and string-alias fast paths return the
// scalar verbatim.
func TestEngine_ReturnAggregateAlias_NotUpgraded(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
	}

	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), "MATCH (n) RETURN count(n) AS n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var records []map[string]any
	for res.Next() {
		rec := res.Record()
		cp := make(map[string]any, len(rec))
		for k, v := range rec {
			cp[k] = v
		}
		records = append(records, cp)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	got, ok := records[0]["n"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("count(n) AS n must remain an IntegerValue, got %T (%v)", records[0]["n"], records[0]["n"])
	}
	if int64(got) != 7 {
		t.Errorf("count(n) = %d, want 7", int64(got))
	}
}

func containsString(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
