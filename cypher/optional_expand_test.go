package cypher_test

// optional_expand_test.go — Cypher OPTIONAL MATCH null semantics (task-634).
//
// Tests:
//  1. Three fully isolated nodes: every row has m.name = NULL.
//  2. Two connected + one isolated: connected rows have m.name set; isolated
//     row has m.name = NULL.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// buildIsolated builds a directed graph with n nodes and zero edges.
func buildIsolated(tb testing.TB, names ...string) (*lpg.Graph[string, float64], *cypher.Engine) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for _, n := range names {
		if err := g.AddNode(n); err != nil {
			tb.Fatalf("AddNode %q: %v", n, err)
		}
		if err := g.SetNodeProperty(n, "name", lpg.StringValue(n)); err != nil {
			tb.Fatalf("SetNodeProperty %q: %v", n, err)
		}
	}
	return g, cypher.NewEngine(g)
}

// TestOptionalExpand_AllIsolated verifies that OPTIONAL MATCH over a graph with
// no edges emits one NULL-extended row per node.
func TestOptionalExpand_AllIsolated(t *testing.T) {
	_, eng := buildIsolated(t, "a", "b", "c")
	ctx := context.Background()

	res, err := eng.Run(ctx,
		`MATCH (n) OPTIONAL MATCH (n)-[r]->(m) RETURN n.name, m.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (one per node), got %d: %v", len(rows), rows)
	}

	for i, row := range rows {
		// n.name must be a real string.
		nv, ok := row["n.name"].(expr.StringValue)
		if !ok {
			t.Errorf("row %d: n.name expected StringValue, got %T (%v)", i, row["n.name"], row["n.name"])
		} else if string(nv) == "" {
			t.Errorf("row %d: n.name is empty", i)
		}

		// m.name must be NULL because there are no edges.
		mv := row["m.name"]
		if !expr.IsNull(mv.(expr.Value)) {
			t.Errorf("row %d: m.name expected NULL, got %T (%v)", i, mv, mv)
		}
	}
}

// TestOptionalExpand_MixedConnectivity verifies mixed null / non-null semantics:
// "a"→"b" exist, "c" is isolated.
func TestOptionalExpand_MixedConnectivity(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %q: %v", n, err)
		}
		if err := g.SetNodeProperty(n, "name", lpg.StringValue(n)); err != nil {
			t.Fatalf("SetNodeProperty %q: %v", n, err)
		}
	}
	if err := g.AddEdge("a", "b", 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.Run(ctx,
		`MATCH (n) OPTIONAL MATCH (n)-[r]->(m) RETURN n.name, m.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	// 3 nodes: "a" has 1 outgoing → 1 row with m.name="b"; "b" and "c" have
	// zero outgoing → 1 null row each.  Total = 3 rows.
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d: %v", len(rows), rows)
	}

	nullCount := 0
	nonNullCount := 0
	for _, row := range rows {
		mv := row["m.name"]
		v, ok := mv.(expr.Value)
		if !ok {
			t.Fatalf("m.name is not expr.Value: %T", mv)
		}
		if expr.IsNull(v) {
			nullCount++
		} else {
			nonNullCount++
			sv, ok := v.(expr.StringValue)
			if !ok {
				t.Fatalf("m.name non-null but not StringValue: %T (%v)", v, v)
			}
			if string(sv) != "b" {
				t.Errorf("non-null m.name: got %q, want %q", string(sv), "b")
			}
		}
	}
	if nullCount != 2 {
		t.Errorf("expected 2 null m.name rows, got %d", nullCount)
	}
	if nonNullCount != 1 {
		t.Errorf("expected 1 non-null m.name row, got %d", nonNullCount)
	}
}
