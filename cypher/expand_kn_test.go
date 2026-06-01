package cypher_test

// expand_kn_test.go — Cypher expand (single hop) on a complete directed
// graph K4 (task-627).
//
// K4 nodes: a, b, c, d.
// Directed edges: a→b, a→c, a→d, b→c, b→d, c→d  (6 edges total).

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildK4 constructs K4 with nodes "a","b","c","d" and all upper-triangular
// directed edges.
func buildK4(tb testing.TB) (*lpg.Graph[string, float64], *cypher.Engine) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})

	nodes := []string{"a", "b", "c", "d"}
	for _, n := range nodes {
		if err := g.AddNode(n); err != nil {
			tb.Fatalf("AddNode %q: %v", n, err)
		}
		if err := g.SetNodeProperty(n, "name", lpg.StringValue(n)); err != nil {
			tb.Fatalf("SetNodeProperty %q: %v", n, err)
		}
	}

	edges := [][2]string{
		{"a", "b"}, {"a", "c"}, {"a", "d"},
		{"b", "c"}, {"b", "d"},
		{"c", "d"},
	}
	for _, e := range edges {
		if err := g.AddEdge(e[0], e[1], 1.0); err != nil {
			tb.Fatalf("AddEdge %q→%q: %v", e[0], e[1], err)
		}
	}

	return g, cypher.NewEngine(g)
}

// extractStringCol extracts the string values for column col from rows.
func extractStringCol(tb testing.TB, rows []map[string]interface{}, col string) []string {
	tb.Helper()
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		v, ok := r[col]
		if !ok {
			tb.Fatalf("column %q missing in row %v", col, r)
		}
		sv, ok := v.(expr.StringValue)
		if !ok {
			tb.Fatalf("column %q: expected StringValue, got %T (%v)", col, v, v)
		}
		out = append(out, string(sv))
	}
	return out
}

func TestExpandK4_FromA(t *testing.T) {
	_, eng := buildK4(t)
	ctx := context.Background()

	res, err := eng.Run(ctx, `MATCH (n {name:"a"})-[r]->(m) RETURN m.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (b,c,d), got %d: %v", len(rows), rows)
	}

	got := extractStringCol(t, rows, "m.name")
	sort.Strings(got)
	want := []string{"b", "c", "d"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d: got %q, want %q", i, got[i], w)
		}
	}
}

func TestExpandK4_FromB(t *testing.T) {
	_, eng := buildK4(t)
	ctx := context.Background()

	res, err := eng.Run(ctx, `MATCH (n {name:"b"})-[r]->(m) RETURN m.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (c,d), got %d: %v", len(rows), rows)
	}

	got := extractStringCol(t, rows, "m.name")
	sort.Strings(got)
	want := []string{"c", "d"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d: got %q, want %q", i, got[i], w)
		}
	}
}

func TestExpandK4_AllEdges(t *testing.T) {
	_, eng := buildK4(t)
	ctx := context.Background()

	res, err := eng.Run(ctx, `MATCH (n)-[r]->(m) RETURN n.name, m.name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 6 {
		t.Fatalf("expected 6 rows (all K4 edges), got %d: %v", len(rows), rows)
	}

	type edge struct{ src, dst string }
	got := make([]edge, 0, 6)
	for _, r := range rows {
		sv := string(r["n.name"].(expr.StringValue))
		dv := string(r["m.name"].(expr.StringValue))
		got = append(got, edge{sv, dv})
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].src != got[j].src {
			return got[i].src < got[j].src
		}
		return got[i].dst < got[j].dst
	})
	want := []edge{
		{"a", "b"}, {"a", "c"}, {"a", "d"},
		{"b", "c"}, {"b", "d"},
		{"c", "d"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("edge %d: got %v, want %v", i, got[i], w)
		}
	}
}
