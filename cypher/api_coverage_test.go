package cypher_test

// api_coverage_test.go — targeted tests to lift gograph/cypher above ≥75%
// statement coverage.
//
// Covers: BindParams all numeric/list/map/error paths, RunAny, RunInTxAny,
// edge mutation methods (SetEdgeProperty, RemoveEdge) via Cypher queries, and
// index-seek machinery via CREATE INDEX + parameterized MATCH.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func newDirGraph(nodes ...string) *lpg.Graph[string, float64] {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for _, n := range nodes {
		g.AddNode(n)
	}
	return g
}

func drainRun(t *testing.T, eng *cypher.Engine, query string, params map[string]any) {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, params)
	if err != nil {
		t.Fatalf("RunAny(%q): %v", query, err)
	}
	defer res.Close()
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("RunAny(%q) error: %v", query, err)
	}
}

func drainTx(t *testing.T, eng *cypher.Engine, query string) {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunInTxAny(%q): %v", query, err)
	}
	defer res.Close()
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("RunInTxAny(%q) error: %v", query, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BindParams: all numeric types, list, map, unsupported
// ─────────────────────────────────────────────────────────────────────────────

func TestBindParams_NumericTypes(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"int", int(42)},
		{"int8", int8(42)},
		{"int16", int16(42)},
		{"int32", int32(42)},
		{"int64", int64(42)},
		{"uint", uint(42)},
		{"uint8", uint8(42)},
		{"uint16", uint16(42)},
		{"uint32", uint32(42)},
		{"uint64", uint64(42)},
		{"float32", float32(3.14)},
		{"float64", float64(3.14)},
		{"bool", true},
		{"string", "hello"},
		{"nil", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := cypher.BindParams(map[string]any{"x": tc.value})
			if err != nil {
				t.Fatalf("BindParams(%v): %v", tc.name, err)
			}
			if tc.value != nil && out["x"] == nil {
				t.Errorf("expected non-nil binding for %v", tc.name)
			}
		})
	}
}

func TestBindParams_ListAndMap(t *testing.T) {
	params := map[string]any{
		"list": []any{int64(1), "two", true},
		"m":    map[string]any{"k": "v", "n": int64(99)},
	}
	out, err := cypher.BindParams(params)
	if err != nil {
		t.Fatalf("BindParams: %v", err)
	}
	if out["list"] == nil {
		t.Error("expected bound list")
	}
	if out["m"] == nil {
		t.Error("expected bound map")
	}
}

func TestBindParams_UnsupportedType(t *testing.T) {
	params := map[string]any{"x": complex(1.0, 2.0)}
	_, err := cypher.BindParams(params)
	if err == nil {
		t.Fatal("expected error for complex128 param type")
	}
}

func TestBindParams_NestedListError(t *testing.T) {
	params := map[string]any{"x": []any{complex(1.0, 2.0)}}
	_, err := cypher.BindParams(params)
	if err == nil {
		t.Fatal("expected error for complex128 inside list")
	}
}

func TestBindParams_NestedMapError(t *testing.T) {
	params := map[string]any{"x": map[string]any{"k": complex(1.0, 2.0)}}
	_, err := cypher.BindParams(params)
	if err == nil {
		t.Fatal("expected error for complex128 inside map")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// RunAny / RunInTxAny basic paths
// ─────────────────────────────────────────────────────────────────────────────

func TestRunAny_WithParams(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	for _, n := range []string{"A", "B", "C"} {
		g.AddNode(n)
	}
	eng := cypher.NewEngine(g)
	drainRun(t, eng, "MATCH (n) RETURN n", map[string]any{"x": int(1)})
}

func TestRunInTxAny_WithParams(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	res, err := eng.RunInTxAny(context.Background(), "MATCH (n) RETURN n", map[string]any{"x": int32(1)})
	if err != nil {
		t.Fatalf("RunInTxAny: %v", err)
	}
	defer res.Close()
	for res.Next() {
	}
}

func TestRunAny_BindParamsError(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	// Passing an unsupported type should surface as an error.
	_, err := eng.RunAny(context.Background(), "MATCH (n) RETURN n", map[string]any{"x": complex(1.0, 2.0)})
	if err == nil {
		t.Fatal("expected error for unsupported param type")
	}
}

func TestRunInTxAny_BindParamsError(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	_, err := eng.RunInTxAny(context.Background(), "MATCH (n) RETURN n", map[string]any{"x": complex(1.0, 2.0)})
	if err == nil {
		t.Fatal("expected error for unsupported param type")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Edge mutation: CREATE relationship with properties → SetEdgeProperty
// ─────────────────────────────────────────────────────────────────────────────

func TestRunInTx_EdgeWithProperties(t *testing.T) {
	g := newDirGraph()
	eng := cypher.NewEngine(g)

	drainTx(t, eng, `CREATE (n:Alice)`)
	drainTx(t, eng, `CREATE (n:Bob)`)

	// CREATE relationship with a property → exercises lpgMutatorAdapter.SetEdgeProperty.
	drainTx(t, eng, `MATCH (a:Alice), (b:Bob) CREATE (a)-[:KNOWS {since: 2020}]->(b)`)

	if g.AdjList().Order() < 2 {
		t.Errorf("expected at least 2 nodes after CREATE, got %d", g.AdjList().Order())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Edge mutation: DETACH DELETE node with edges → RemoveEdge
// ─────────────────────────────────────────────────────────────────────────────

func TestRunInTx_DetachDeleteWithEdges(t *testing.T) {
	g := newDirGraph()
	eng := cypher.NewEngine(g)

	drainTx(t, eng, `CREATE (n:Hub)`)
	drainTx(t, eng, `CREATE (n:Spoke)`)
	// Create an edge from Hub to Spoke.
	drainTx(t, eng, `MATCH (h:Hub), (s:Spoke) CREATE (h)-[:LINK]->(s)`)

	// DETACH DELETE removes the Hub and its incident edges → RemoveEdge.
	drainTx(t, eng, `MATCH (h:Hub) DETACH DELETE h`)
}

// ─────────────────────────────────────────────────────────────────────────────
// Index-seek machinery: CREATE INDEX + parameterized MATCH
// ─────────────────────────────────────────────────────────────────────────────

func TestRunInTx_IndexSeekParameterized(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	// Create index so idxMgr has a hash index.
	drainRun(t, eng, `CREATE INDEX pname FOR (n:Person) ON (n.name)`, nil)

	// Create some nodes.
	drainTx(t, eng, `CREATE (n:Person {name: "Alice"})`)
	drainTx(t, eng, `CREATE (n:Person {name: "Bob"})`)

	// Parameterized equality predicate — if the predicate format matches
	// extractEqParamFromPredicate, tryBuildIndexSeekFromSelection rewrites to seek.
	res, err := eng.RunAny(context.Background(),
		`MATCH (n:Person) WHERE n.name = $name RETURN n`,
		map[string]any{"name": "Alice"},
	)
	if err != nil {
		t.Fatalf("parameterized MATCH: %v", err)
	}
	defer res.Close()
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// resolveSeekValue: literal string, bool, integer paths
// ─────────────────────────────────────────────────────────────────────────────

func TestRunInTx_IndexSeekLiteralString(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	drainRun(t, eng, `CREATE INDEX pname2 FOR (n:Person2) ON (n.name)`, nil)
	drainTx(t, eng, `CREATE (n:Person2 {name: "Carol"})`)

	// Literal equality in WHERE — the seek value is a literal string.
	res, err := eng.Run(context.Background(),
		`MATCH (n:Person2) WHERE n.name = "Carol" RETURN n`, nil)
	if err != nil {
		t.Fatalf("literal MATCH: %v", err)
	}
	defer res.Close()
	for res.Next() {
	}
}
