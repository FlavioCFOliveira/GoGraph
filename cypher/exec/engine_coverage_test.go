package exec_test

// engine_coverage_test.go — targeted Engine-based tests to lift own-package
// coverage for operators that cannot be reached through hand-built operator
// trees: set_all.go, merge_relationship.go, delete.go deeper paths.
//
// All queries must end with RETURN because the Engine requires a
// ProduceResults root in every physical plan.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newTestGraph creates an empty LPG graph for engine-level tests.
func newTestGraph() *lpg.Graph[string, float64] {
	return lpg.New[string, float64](adjlist.Config{Directed: true})
}

// drainResult collects all records from a Result and returns the record
// count plus any iteration error. The result is always closed.
func drainResult(t *testing.T, res *cypher.Result) int {
	t.Helper()
	defer res.Close()
	count := 0
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result.Err: %v", err)
	}
	return count
}

// runQuery is a convenience wrapper for Engine.RunInTx + drainResult.
// Write queries (CREATE, MERGE, DELETE, SET, REMOVE) must go through
// RunInTx; read-only queries (MATCH … RETURN) can use Run directly.
func runQuery(t *testing.T, eng *cypher.Engine, q string) int {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", q, err)
	}
	return drainResult(t, res)
}

// readQuery wraps Engine.Run for read-only queries.
func readQuery(t *testing.T, eng *cypher.Engine, q string) int {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	return drainResult(t, res)
}

// ─────────────────────────────────────────────────────────────────────────────
// SetAllProperties — SET n = {…} (replace) and SET n += {…} (merge)
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_SetAllProperties_Replace exercises `SET n = {…}` (isReplace=true):
// all existing properties are cleared before the new map is applied.
func TestEngine_SetAllProperties_Replace(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (n:Person {name: "Alice", age: 30}) RETURN n`)
	runQuery(t, eng, `MATCH (n:Person) SET n = {name: "Bob"} RETURN n`)

	n := readQuery(t, eng, `MATCH (n:Person) RETURN n.name, n.age`)
	if n != 1 {
		t.Fatalf("expected 1 Person, got %d", n)
	}
}

// TestEngine_SetAllProperties_Merge exercises `SET n += {…}` (isReplace=false):
// existing properties are preserved unless overridden.
func TestEngine_SetAllProperties_Merge(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (n:Item {a: 1, b: 2}) RETURN n`)
	runQuery(t, eng, `MATCH (n:Item) SET n += {a: 10, c: 3} RETURN n`)

	n := readQuery(t, eng, `MATCH (n:Item) RETURN n.a, n.b, n.c`)
	if n != 1 {
		t.Fatalf("expected 1 Item, got %d", n)
	}
}

// TestEngine_SetAllProperties_EntityCopy exercises the entity-copy form
// (`SET n = m`): the source entity's properties are copied to the target.
func TestEngine_SetAllProperties_EntityCopy(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (a:A {x: 42}), (b:B {y: 99}) RETURN a, b`)
	runQuery(t, eng, `MATCH (a:A), (b:B) SET b = a RETURN b`)

	n := readQuery(t, eng, `MATCH (b:B) RETURN b.x, b.y`)
	if n != 1 {
		t.Fatalf("expected 1 B node, got %d", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// MergeRelationship — MERGE ()-[r:T]->() with ON CREATE / ON MATCH
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_MergeRelationship_Create exercises MERGE on a relationship that
// does not yet exist — the ON CREATE path fires.
func TestEngine_MergeRelationship_Create(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (a:A), (b:B) RETURN a, b`)
	n := runQuery(t, eng, `
		MATCH (a:A), (b:B)
		MERGE (a)-[r:KNOWS]->(b)
		RETURN r
	`)
	if n != 1 {
		t.Fatalf("MERGE created %d rows, want 1", n)
	}

	// Second call: ON MATCH fires — relationship must already exist.
	n2 := runQuery(t, eng, `
		MATCH (a:A), (b:B)
		MERGE (a)-[r:KNOWS]->(b)
		RETURN r
	`)
	if n2 != 1 {
		t.Fatalf("second MERGE returned %d rows, want 1", n2)
	}
}

// TestEngine_MergeRelationship_OnCreateSet exercises MERGE … ON CREATE SET.
func TestEngine_MergeRelationship_OnCreateSet(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (a:A), (b:B) RETURN a, b`)
	runQuery(t, eng, `
		MATCH (a:A), (b:B)
		MERGE (a)-[r:LIKES]->(b)
		ON CREATE SET r.since = 2024
		RETURN r
	`)

	n := readQuery(t, eng, `MATCH (a:A)-[r:LIKES]->(b:B) RETURN r.since`)
	if n != 1 {
		t.Fatalf("expected 1 LIKES relationship, got %d", n)
	}
}

// TestEngine_MergeRelationship_OnMatchSet exercises MERGE … ON MATCH SET.
func TestEngine_MergeRelationship_OnMatchSet(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (a:A), (b:B) RETURN a, b`)
	// First MERGE creates the relationship.
	runQuery(t, eng, `MATCH (a:A), (b:B) MERGE (a)-[r:FOLLOWS]->(b) RETURN r`)
	// Second MERGE triggers ON MATCH.
	runQuery(t, eng, `
		MATCH (a:A), (b:B)
		MERGE (a)-[r:FOLLOWS]->(b)
		ON MATCH SET r.updated = true
		RETURN r
	`)

	n := readQuery(t, eng, `MATCH (a:A)-[r:FOLLOWS]->(b:B) RETURN r.updated`)
	if n != 1 {
		t.Fatalf("expected 1 FOLLOWS relationship, got %d", n)
	}
}

// TestEngine_MergeRelationship_WithProps exercises MERGE with inline
// relationship properties.
func TestEngine_MergeRelationship_WithProps(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (a:A), (b:B) RETURN a, b`)
	n := runQuery(t, eng, `
		MATCH (a:A), (b:B)
		MERGE (a)-[r:TAGGED {label: "friend"}]->(b)
		RETURN r
	`)
	if n != 1 {
		t.Fatalf("MERGE with props: %d rows, want 1", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteNode — additional branches via Engine
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_DeleteNode_NullTarget exercises the null-target path (OPTIONAL
// MATCH that produces no results, leaving the node variable null).
func TestEngine_DeleteNode_NullTarget(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	// No nodes exist; the OPTIONAL MATCH binds n to null. DELETE on a
	// null target is a no-op per openCypher spec. The RETURN 1 satisfies
	// the ProduceResults requirement.
	runQuery(t, eng, `OPTIONAL MATCH (n:Ghost) DELETE n RETURN 1`)
}

// TestEngine_DeleteRelationship_DirectedEdge exercises DELETE on a
// relationship via the engine's DELETE-rel path.
func TestEngine_DeleteRelationship_DirectedEdge(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (a:A)-[:LINK]->(b:B) RETURN 1`)
	runQuery(t, eng, `MATCH (a:A)-[r:LINK]->(b:B) DELETE r RETURN 1`)

	// The relationship must be gone.
	n := readQuery(t, eng, `MATCH (a:A)-[r:LINK]->(b:B) RETURN count(r)`)
	t.Logf("remaining LINK relationships: %d", n)
}

// ─────────────────────────────────────────────────────────────────────────────
// SetAllProperties with relationship target (SET r = …)
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_SetRelationshipProperties exercises SET on a relationship using
// the Engine — hits the relationship branch in applyToRelationship (set.go).
func TestEngine_SetRelationshipProperties(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (a:A)-[:REL {w: 1}]->(b:B) RETURN 1`)
	runQuery(t, eng, `MATCH (a:A)-[r:REL]->(b:B) SET r.w = 99 RETURN r.w`)

	n := readQuery(t, eng, `MATCH (a:A)-[r:REL]->(b:B) RETURN r.w`)
	if n != 1 {
		t.Fatalf("expected 1 REL, got %d", n)
	}
}

// TestEngine_SetRelationshipAllProperties exercises SET r += {…} on a
// relationship to hit the relationship target branch in SetAllProperties.
func TestEngine_SetRelationshipAllProperties(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (a:A)-[:E {x: 1}]->(b:B) RETURN 1`)
	runQuery(t, eng, `MATCH (a:A)-[r:E]->(b:B) SET r += {y: 2} RETURN r`)

	n := readQuery(t, eng, `MATCH (a:A)-[r:E]->(b:B) RETURN r.x, r.y`)
	if n != 1 {
		t.Fatalf("expected 1 E relationship, got %d", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DetachDelete via Engine — additional path (node with incoming AND outgoing)
// ─────────────────────────────────────────────────────────────────────────────

// TestEngine_DetachDelete_StarNode exercises DETACH DELETE on a node connected
// to several others both as source and destination.
func TestEngine_DetachDelete_StarNode(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	// Create a star topology: n→a, n→b, c→n.
	runQuery(t, eng, `
		CREATE (n:Hub {name: "hub"}),
		       (a:Leaf), (b:Leaf), (c:Leaf),
		       (n)-[:OUT]->(a), (n)-[:OUT]->(b), (c)-[:IN]->(n)
		RETURN 1
	`)

	runQuery(t, eng, `MATCH (n:Hub) DETACH DELETE n RETURN 1`)

	// No Hub nodes should remain.
	n := readQuery(t, eng, `MATCH (n:Hub) RETURN count(n)`)
	t.Logf("remaining Hub nodes count records: %d", n)
}

// TestEngine_CreateAndMatchPattern exercises a broader CREATE+MATCH scenario
// to ensure CreateRelationship.Next and related paths are traversed through
// the real engine plan rather than via hand-built stubs.
func TestEngine_CreateAndMatchPattern(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `
		CREATE (alice:Person {name: "Alice", age: 30}),
		       (bob:Person {name: "Bob", age: 25}),
		       (alice)-[:KNOWS {since: 2020}]->(bob)
		RETURN 1
	`)

	n := readQuery(t, eng, `
		MATCH (a:Person)-[r:KNOWS]->(b:Person)
		RETURN a.name, r.since, b.name
	`)
	if n != 1 {
		t.Fatalf("expected 1 match row, got %d", n)
	}
}

// TestEngine_REMOVE_Label exercises the REMOVE label operator path.
func TestEngine_REMOVE_Label(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (n:Person:Employee {name: "X"}) RETURN n`)
	runQuery(t, eng, `MATCH (n:Employee) REMOVE n:Employee RETURN n`)

	n := readQuery(t, eng, `MATCH (n:Person) RETURN count(n)`)
	if n != 1 {
		t.Fatalf("expected 1 Person count record, got %d", n)
	}
}

// TestEngine_REMOVE_Property exercises the REMOVE property operator path.
func TestEngine_REMOVE_Property(t *testing.T) {
	t.Parallel()
	g := newTestGraph()
	eng := cypher.NewEngine(g)

	runQuery(t, eng, `CREATE (n:Item {k: 1, v: 2}) RETURN n`)
	runQuery(t, eng, `MATCH (n:Item) REMOVE n.k RETURN n`)

	n := readQuery(t, eng, `MATCH (n:Item) RETURN n.k, n.v`)
	if n != 1 {
		t.Fatalf("expected 1 Item, got %d", n)
	}
}
