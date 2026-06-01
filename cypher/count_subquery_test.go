package cypher_test

// count_subquery_test.go — COUNT { … } subquery expression tests (T883).
//
// COUNT { … } is supported as an expression in both RETURN projections and
// WHERE predicates (via the subquery evaluator wired in Engine.Run). The
// operator supports the pattern form: COUNT { (n)-[]->(m) }.
//
// Limitation confirmed by probe: correlated subqueries where the outer variable
// appears on the right-hand side of the inner pattern — e.g. COUNT { (src)-[]->(n) }
// when n is the outer node — do not propagate the outer binding and always
// return 0. Tests in this file only use the outer variable on the left-hand side
// (outgoing patterns) to avoid testing unimplemented behaviour.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newHubSpokeEngine returns an engine backed by a hub-and-spoke graph:
//
//	hub ──E──► sp0
//	hub ──E──► sp1
//	hub ──E──► sp2
//	hub ──E──► sp3
//
// The hub node has out-degree 4; each spoke node has out-degree 0.
// All nodes carry a "name" string property.
func newHubSpokeEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:Node {name: 'hub'})-[:E]->(:Node {name: 'sp0'})`)
	runSetup(t, eng, `MATCH (h:Node {name: 'hub'}) CREATE (h)-[:E]->(:Node {name: 'sp1'})`)
	runSetup(t, eng, `MATCH (h:Node {name: 'hub'}) CREATE (h)-[:E]->(:Node {name: 'sp2'})`)
	runSetup(t, eng, `MATCH (h:Node {name: 'hub'}) CREATE (h)-[:E]->(:Node {name: 'sp3'})`)
	return eng
}

// TestCountSubquery_ReturnsOutDegree verifies that
//
//	MATCH (n) RETURN COUNT { (n)-[]->(m) } AS out_degree
//
// returns the correct out-degree for every node in a hub-spoke graph.
// The hub must report 4; each spoke must report 0.
func TestCountSubquery_ReturnsOutDegree(t *testing.T) {
	t.Parallel()
	eng := newHubSpokeEngine(t)

	const q = `MATCH (n:Node) RETURN n.name AS name, COUNT { (n)-[]->(m) } AS out_degree`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 5 {
		t.Fatalf("expected 5 rows (1 hub + 4 spokes), got %d", len(rows))
	}

	for _, row := range rows {
		name, ok := row["name"].(expr.StringValue)
		if !ok {
			t.Fatalf("name column: expected StringValue, got %T", row["name"])
		}
		cnt, ok := row["out_degree"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("out_degree column: expected IntegerValue, got %T (%v)", row["out_degree"], row["out_degree"])
		}
		switch string(name) {
		case "hub":
			if int64(cnt) != 4 {
				t.Errorf("hub out_degree: got %d, want 4", int64(cnt))
			}
		default:
			if int64(cnt) != 0 {
				t.Errorf("spoke %q out_degree: got %d, want 0", string(name), int64(cnt))
			}
		}
	}
}

// TestCountSubquery_IsolatedNode verifies that COUNT { (n)-[]->(m) } returns 0
// when the matched node has no outgoing edges.
func TestCountSubquery_IsolatedNode(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:Alone {name: 'solo'})`)

	const q = `MATCH (n:Alone) RETURN COUNT { (n)-[]->(m) } AS c`
	rows := drainAll(t, eng, q)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	iv, ok := rows[0]["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("c: expected IntegerValue, got %T", rows[0]["c"])
	}
	if int64(iv) != 0 {
		t.Errorf("isolated node COUNT: got %d, want 0", int64(iv))
	}
}

// TestCountSubquery_OnEmptyGraph verifies that MATCH produces no rows (and
// therefore no COUNT evaluations) when the graph is empty.
func TestCountSubquery_OnEmptyGraph(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	rows := drainAll(t, eng, `MATCH (n) RETURN COUNT { (n)-[]->(m) } AS c`)
	if len(rows) != 0 {
		t.Errorf("empty graph COUNT: got %d rows, want 0", len(rows))
	}
}

// TestCountSubquery_InWherePredicate verifies that COUNT { } used inside a
// WHERE predicate (COUNT { … } > threshold) filters rows correctly.
//
// Query: MATCH (n:Node) WHERE COUNT { (n)-[]->(m) } > 0 RETURN n.name AS name
// Expected: only the hub (out-degree 4); all four spokes are excluded.
func TestCountSubquery_InWherePredicate(t *testing.T) {
	t.Parallel()
	eng := newHubSpokeEngine(t)

	const q = `MATCH (n:Node) WHERE COUNT { (n)-[]->(m) } > 0 RETURN n.name AS name`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("COUNT in WHERE > 0: got %d rows, want 1", len(rows))
	}
	name, ok := rows[0]["name"].(expr.StringValue)
	if !ok {
		t.Fatalf("name: expected StringValue, got %T", rows[0]["name"])
	}
	if string(name) != "hub" {
		t.Errorf("COUNT in WHERE > 0: got %q, want hub", string(name))
	}
}

// TestCountSubquery_ExactThreshold verifies COUNT { } compared with an exact
// equality predicate. The hub has out-degree 4; the test filters for
// COUNT { (n)-[]->(m) } = 4.
func TestCountSubquery_ExactThreshold(t *testing.T) {
	t.Parallel()
	eng := newHubSpokeEngine(t)

	const q = `MATCH (n:Node) WHERE COUNT { (n)-[]->(m) } = 4 RETURN n.name AS name`
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows := collectRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("COUNT = 4: got %d rows, want 1", len(rows))
	}
	name, ok := rows[0]["name"].(expr.StringValue)
	if !ok {
		t.Fatalf("name: expected StringValue, got %T", rows[0]["name"])
	}
	if string(name) != "hub" {
		t.Errorf("COUNT = 4: got %q, want hub", string(name))
	}
}
