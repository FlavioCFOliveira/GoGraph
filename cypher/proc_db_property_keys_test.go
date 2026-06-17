package cypher_test

// proc_db_property_keys_test.go — tests for db.propertyKeys() procedure
// (task-895, wired in task-1579).
//
// db.propertyKeys() yields one row per distinct property key currently in use
// — that is, present on at least one non-tombstoned node, or on at least one
// edge whose endpoints are both live — sourced from the engine graph via
// lpg.Graph.PropertyKeysInUse. Order is unspecified.
//
// This is a deliberate, openCypher-conformant divergence from Neo4j, whose
// db.propertyKeys() lists every property-key token ever interned (tokens are
// never garbage-collected) and so retains keys no longer borne by any element.
// GoGraph reports only keys in live use; see dbPropertyKeys in
// cypher/procs/builtin_db.go.
//
// Properties are seeded directly on the engine graph (the same approach the
// db.relationshipTypes() integration test uses for edge labels): the procedure
// reads the graph's live node/edge property shards via PropertyKeysInUse, so a
// directly seeded graph exercises the same code path as Cypher writes.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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

// propertyKeysInUse runs CALL db.propertyKeys() through the engine and returns
// the yielded keys as a set.
func propertyKeysInUse(t *testing.T, eng *cypher.Engine) map[string]struct{} {
	t.Helper()
	res, err := eng.Run(context.Background(),
		`CALL db.propertyKeys() YIELD propertyKey`, nil)
	if err != nil {
		t.Fatalf("CALL db.propertyKeys(): %v", err)
	}
	rows := collectProc(t, res)
	set := make(map[string]struct{}, len(rows))
	for i, row := range rows {
		v, ok := row["propertyKey"]
		if !ok {
			t.Errorf("row[%d] missing 'propertyKey' column", i)
			continue
		}
		if _, dup := set[v]; dup {
			t.Errorf("duplicate property key %q in result %v", v, rows)
		}
		set[v] = struct{}{}
	}
	return set
}

// TestProcDbPropertyKeys_AfterCreatingNodes seeds nodes and an edge bearing
// distinct property keys and verifies that CALL db.propertyKeys() yields
// exactly the distinct keys in use. "name" is borne by multiple nodes to
// confirm de-duplication, and the edge property "since" confirms the result is
// the union across the node and edge property stores. Order is unspecified, so
// the result is asserted as a set.
func TestProcDbPropertyKeys_AfterCreatingNodes(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g) // installs the index.Manager and wires PropertyKeys

	for _, n := range []string{"alice", "bob", "inception"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
	}
	mustSetNodeProp(t, g, "alice", "name", lpg.StringValue("Alice"))
	mustSetNodeProp(t, g, "alice", "age", lpg.Int64Value(30))
	mustSetNodeProp(t, g, "bob", "name", lpg.StringValue("Bob"))
	mustSetNodeProp(t, g, "bob", "score", lpg.Float64Value(9.5))
	mustSetNodeProp(t, g, "inception", "name", lpg.StringValue("Inception"))

	if err := g.AddEdge("alice", "bob", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetEdgeProperty("alice", "bob", "since", lpg.Int64Value(2020)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

	got := propertyKeysInUse(t, eng)
	want := []string{"name", "age", "score", "since"}
	if len(got) != len(want) {
		t.Fatalf("expected %d distinct property keys, got %d: %v", len(want), len(got), got)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing expected property key %q in %v", w, got)
		}
	}
}

// TestProcDbPropertyKeys_DroppedAfterDelete verifies the in-use semantics
// end-to-end: a property key borne by exactly one node is no longer listed by
// db.propertyKeys() once that node is deleted (tombstoned). This exercises both
// the in-use filtering and the tombstone filtering of PropertyKeysInUse. A key
// still borne by a surviving node ("name") remains listed.
func TestProcDbPropertyKeys_DroppedAfterDelete(t *testing.T) {
	t.Parallel()
	g := newProcTestGraph()
	eng := cypher.NewEngine(g)

	for _, n := range []string{"alice", "bob"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
	}
	// "rare" is borne only by bob; "name" is borne by both nodes.
	mustSetNodeProp(t, g, "alice", "name", lpg.StringValue("Alice"))
	mustSetNodeProp(t, g, "bob", "name", lpg.StringValue("Bob"))
	mustSetNodeProp(t, g, "bob", "rare", lpg.Int64Value(1))

	before := propertyKeysInUse(t, eng)
	if _, ok := before["rare"]; !ok {
		t.Fatalf("before delete: expected %q to be listed, got %v", "rare", before)
	}
	if _, ok := before["name"]; !ok {
		t.Fatalf("before delete: expected %q to be listed, got %v", "name", before)
	}

	// Delete the only element bearing "rare".
	g.RemoveNode("bob")

	after := propertyKeysInUse(t, eng)
	if _, ok := after["rare"]; ok {
		t.Errorf("after delete: %q must no longer be listed, got %v", "rare", after)
	}
	if _, ok := after["name"]; !ok {
		t.Errorf("after delete: %q must still be listed (borne by alice), got %v", "name", after)
	}
}

// mustSetNodeProp sets a node property or fails the test.
func mustSetNodeProp(t *testing.T, g *lpg.Graph[string, float64], n, key string, v lpg.PropertyValue) {
	t.Helper()
	if err := g.SetNodeProperty(n, key, v); err != nil {
		t.Fatalf("SetNodeProperty(%q, %q): %v", n, key, err)
	}
}
