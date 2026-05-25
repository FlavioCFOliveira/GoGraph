package cypher_test

// expand_star_test.go — Cypher expand (single hop) on a star topology (task-630).
//
// Star: "hub" → "spoke0", "hub" → "spoke1", "hub" → "spoke2", "hub" → "spoke3".
// The hub has 4 outgoing edges and zero incoming edges.

import (
	"context"
	"sort"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// buildStar constructs a directed star with one hub and nSpokes spokes.
func buildStar(tb testing.TB, nSpokes int) (*lpg.Graph[string, float64], *cypher.Engine) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})

	hub := "hub"
	if err := g.AddNode(hub); err != nil {
		tb.Fatalf("AddNode %q: %v", hub, err)
	}
	if err := g.SetNodeProperty(hub, "name", lpg.StringValue(hub)); err != nil {
		tb.Fatalf("SetNodeProperty %q: %v", hub, err)
	}

	for i := range nSpokes {
		spoke := "spoke" + string(rune('0'+i))
		if err := g.AddNode(spoke); err != nil {
			tb.Fatalf("AddNode %q: %v", spoke, err)
		}
		if err := g.SetNodeProperty(spoke, "name", lpg.StringValue(spoke)); err != nil {
			tb.Fatalf("SetNodeProperty %q: %v", spoke, err)
		}
		if err := g.AddEdge(hub, spoke, 1.0); err != nil {
			tb.Fatalf("AddEdge hub→%q: %v", spoke, err)
		}
	}

	return g, cypher.NewEngine(g)
}

// TestExpandStar_HubOutgoing confirms that the hub has exactly nSpokes outgoing neighbours.
func TestExpandStar_HubOutgoing(t *testing.T) {
	const nSpokes = 4
	_, eng := buildStar(t, nSpokes)
	ctx := context.Background()

	res, err := eng.Run(ctx, `MATCH (n {name:"hub"})-[r]->(m) RETURN m.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != nSpokes {
		t.Fatalf("expected %d rows (spokes), got %d: %v", nSpokes, len(rows), rows)
	}

	got := make([]string, 0, nSpokes)
	for _, r := range rows {
		sv, ok := r["m.name"].(expr.StringValue)
		if !ok {
			t.Fatalf("m.name: expected StringValue, got %T (%v)", r["m.name"], r["m.name"])
		}
		got = append(got, string(sv))
	}
	sort.Strings(got)
	want := []string{"spoke0", "spoke1", "spoke2", "spoke3"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("spoke %d: got %q, want %q", i, got[i], w)
		}
	}
}

// TestExpandStar_HubNoIncoming confirms the hub has zero incoming edges.
func TestExpandStar_HubNoIncoming(t *testing.T) {
	_, eng := buildStar(t, 4)
	ctx := context.Background()

	res, err := eng.Run(ctx, `MATCH (n)-[r]->(m {name:"hub"}) RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 0 {
		t.Fatalf("expected 0 rows (hub has no incoming), got %d: %v", len(rows), rows)
	}
}

// TestExpandStar_SpokeOutgoing confirms a spoke has zero outgoing edges.
func TestExpandStar_SpokeOutgoing(t *testing.T) {
	_, eng := buildStar(t, 4)
	ctx := context.Background()

	res, err := eng.Run(ctx, `MATCH (n {name:"spoke0"})-[r]->(m) RETURN m.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 0 {
		t.Fatalf("expected 0 rows (spoke has no outgoing), got %d: %v", len(rows), rows)
	}
}
