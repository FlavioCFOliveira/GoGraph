package cypher_test

// match_undirected_dedup_test.go — T942 regression: undirected MATCH must
// emit each stored edge exactly once.
//
// Background: commit 51feab7 fixed exec.Expand.tryRevEdge so that an
// edge-type filter is correctly applied to reverse-CSR traversals. Before
// that fix, undirected patterns with an edge-type predicate effectively
// only matched the forward side. As a consequence, several fixture
// generators (notably bench/ldbc) created each logical edge in BOTH
// directions to compensate. Once the engine became correct, those
// two-edge fixtures started returning duplicate rows for undirected
// patterns. This file pins the post-fix contract directly at the engine
// level, independent of the LDBC fixture.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestMatch_Undirected_SingleEdgePair_NoDuplicate stores a single
// directed A-KNOWS->B edge and runs an undirected MATCH from A. The
// engine must emit exactly one binding for B.
func TestMatch_Undirected_SingleEdgePair_NoDuplicate(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	seed := []string{
		`CREATE (a:Person {id: 1, name: 'Alice'})`,
		`CREATE (b:Person {id: 2, name: 'Bob'})`,
		`MATCH (a:Person),(b:Person) WHERE a.id = 1 AND b.id = 2 CREATE (a)-[:KNOWS]->(b)`,
	}
	for _, q := range seed {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		// Drain.
		for res.Next() {
		}
		_ = res.Err()
		res.Close()
	}

	const query = `MATCH (start:Person {id: 1})-[:KNOWS]-(friend:Person)
RETURN friend.id`
	res, err := eng.RunAny(ctx, query, nil)
	if err != nil {
		t.Fatalf("undirected MATCH: %v", err)
	}
	defer res.Close()

	var rows int
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if rows != 1 {
		t.Fatalf("undirected MATCH on single A→B edge: got %d rows, want 1 (engine emitted duplicates for the same logical edge)", rows)
	}
}

// TestMatch_Undirected_BothDirections_DistinctEdges stores two distinct
// directed edges A-KNOWS->B and B-KNOWS->A. These are two separate edges
// in the graph; an undirected MATCH from A must report two bindings for
// B (one per edge). This is the symmetric counterpart of the previous
// test: it pins the engine's behaviour when the data model legitimately
// has parallel edges in opposite directions.
func TestMatch_Undirected_BothDirections_DistinctEdges(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	seed := []string{
		`CREATE (a:Person {id: 1, name: 'Alice'})`,
		`CREATE (b:Person {id: 2, name: 'Bob'})`,
		`MATCH (a:Person),(b:Person) WHERE a.id = 1 AND b.id = 2 CREATE (a)-[:KNOWS]->(b)`,
		`MATCH (a:Person),(b:Person) WHERE a.id = 2 AND b.id = 1 CREATE (a)-[:KNOWS]->(b)`,
	}
	for _, q := range seed {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() {
		}
		_ = res.Err()
		res.Close()
	}

	const query = `MATCH (start:Person {id: 1})-[:KNOWS]-(friend:Person)
RETURN friend.id`
	res, err := eng.RunAny(ctx, query, nil)
	if err != nil {
		t.Fatalf("undirected MATCH: %v", err)
	}
	defer res.Close()

	var rows int
	for res.Next() {
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	if rows != 2 {
		t.Fatalf("undirected MATCH on two parallel-but-opposite edges: got %d rows, want 2", rows)
	}
}
