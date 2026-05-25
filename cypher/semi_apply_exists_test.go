package cypher_test

// semi_apply_exists_test.go — EXISTS { … } subquery via SemiApply / AntiSemiApply
// (T723).
//
// The engine wires *ir.SemiApply → exec.SemiApply and
// *ir.AntiSemiApply → exec.AntiSemiApply in buildOperator (cypher/api.go).
// WHERE EXISTS { (n)-->(m) } is lowered to SemiApply; WHERE NOT EXISTS { … }
// to AntiSemiApply. Both paths are already covered by subquery_eval_test.go
// at the expression level; this file tests the graph-pattern form used in a
// WHERE clause.

import (
	"context"
	"slices"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newSemiApplyGraph creates a directed graph with:
//
//	alice  ──KNOWS──►  bob
//	charlie             (isolated — no edges)
//
// Returns the engine so tests can run queries against it.
func newSemiApplyGraph(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:Person {name: 'alice'})-[:KNOWS]->(:Person {name: 'bob'})`)
	runSetup(t, eng, `CREATE (:Person {name: 'charlie'})`)
	return eng
}

// collectNameCol drains a Result and returns the values of the "name" column
// as a sorted string slice for stable comparison.
func collectNameCol(t *testing.T, eng *cypher.Engine, query string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", query, err)
	}
	rows := collectRecords(t, res)
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		// expr.StringValue is a named type ~string; cast directly to get the
		// raw string without the %v quote wrapping.
		if sv, ok := row["name"].(expr.StringValue); ok {
			names = append(names, string(sv))
		} else {
			t.Errorf("name column: expected expr.StringValue, got %T (%v)", row["name"], row["name"])
		}
	}
	slices.Sort(names)
	return names
}

// TestSemiApply_WithOutgoingEdge verifies that WHERE EXISTS { (n)-->(m) }
// selects only the node that has an outgoing edge.
//
// Expected: only "alice" (has outgoing KNOWS edge).
func TestSemiApply_WithOutgoingEdge(t *testing.T) {
	t.Parallel()
	eng := newSemiApplyGraph(t)

	const q = `MATCH (n:Person) WHERE EXISTS { (n)-[]->(m) } RETURN n.name AS name`
	names := collectNameCol(t, eng, q)

	if len(names) != 1 || names[0] != "alice" {
		t.Errorf("EXISTS outgoing: got %v, want [alice]", names)
	}
}

// TestAntiSemiApply_WithoutOutgoingEdge verifies that WHERE NOT EXISTS { (n)-->(m) }
// selects nodes that have no outgoing edge.
//
// Expected: "bob" (has incoming only) and "charlie" (isolated) — sorted: [bob, charlie].
func TestAntiSemiApply_WithoutOutgoingEdge(t *testing.T) {
	t.Parallel()
	eng := newSemiApplyGraph(t)

	const q = `MATCH (n:Person) WHERE NOT EXISTS { (n)-[]->(m) } RETURN n.name AS name`
	names := collectNameCol(t, eng, q)

	want := []string{"bob", "charlie"}
	if len(names) != len(want) {
		t.Fatalf("NOT EXISTS outgoing: got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("NOT EXISTS outgoing[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

// TestSemiApply_EmptyGraph verifies that EXISTS on an empty graph returns no rows.
func TestSemiApply_EmptyGraph(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(context.Background(),
		`MATCH (n) WHERE EXISTS { (n)-[]->(m) } RETURN n`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 0 {
		t.Errorf("EXISTS on empty graph: got %d rows, want 0", len(rows))
	}
}

// TestSemiApply_AllNodesHaveEdges verifies that EXISTS returns all nodes when
// every node has at least one outgoing edge.
//
// Graph: a → b → c → a (cycle, each has exactly one outgoing edge).
// All three nodes and all three edges are created in a single multi-pattern
// CREATE statement to avoid the known limitation with MATCH+CREATE multi-pattern.
func TestSemiApply_AllNodesHaveEdges(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	// Single CREATE with all nodes and edges: a→b→c→a.
	runSetup(t, eng, `CREATE (a:X {name: 'a'}), (b:X {name: 'b'}), (c:X {name: 'c'}), (a)-[:R]->(b), (b)-[:R]->(c), (c)-[:R]->(a)`)

	res, err := eng.Run(context.Background(),
		`MATCH (n:X) WHERE EXISTS { (n)-[]->(m) } RETURN n.name AS name`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 3 {
		t.Errorf("EXISTS all-nodes-have-edges: got %d rows, want 3", len(rows))
	}
}
