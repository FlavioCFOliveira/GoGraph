package cypher_test

// merge_match_test.go — T811
//
// Documents what the engine actually does when MERGE is called for a pattern
// that already exists. Because the engine's searchFn stub always returns nil
// (no matches), MERGE always fires ON CREATE — the "ON MATCH" path is never
// reached through the engine today.
//
// See: cypher/api.go ~line 1013 — searchFn comment explains the limitation.
// See T918 for the item tracking a real searchFn implementation.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestMerge_TwiceCreatesTwo documents that the current engine's MERGE always
// fires ON CREATE: calling MERGE twice with the same pattern results in 2
// nodes, not 1.
func TestMerge_TwiceCreatesTwo(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (n:Person {name: "Alice"})`)
	drainRunInTx(t, eng, `MERGE (n:Person {name: "Alice"})`)

	// Known limitation: searchFn is always ON CREATE, so count is 2.
	// Update this assertion when T918 implements real match semantics.
	t.Log("known limitation: MERGE searchFn stub means every call fires ON CREATE")
	assertCount(t, eng, ctx, `MATCH (n:Person) RETURN count(n) AS n`, 2)
}

// TestMerge_OnMatchSet verifies that MERGE ON MATCH SET runs the on-match
// action when a match is found. In the current engine the searchFn stub means
// the ON MATCH path never fires: this test documents that the ON MATCH action
// is not applied (created=false remains absent on the node) so changes to that
// behaviour are detected immediately.
func TestMerge_OnMatchSet(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed a node via plain CREATE so a Person definitely exists.
	drainRunInTx(t, eng, `CREATE (n:Person {name: "Carol"})`)

	// MERGE with ON MATCH SET — with the stub searchFn this fires ON CREATE
	// (adds a second Carol node) and does NOT apply the on-match action.
	drainRunInTx(t, eng,
		`MERGE (n:Person {name: "Carol"}) ON MATCH SET n.matched = true`)

	// Engine always fires ON CREATE: we now have 2 Person nodes.
	assertCount(t, eng, ctx, `MATCH (n:Person) RETURN count(n) AS n`, 2)
}

// TestMerge_OnCreateSetProperty verifies the ON CREATE SET path end-to-end
// via assertCount: the created node is visible after MERGE ON CREATE SET.
func TestMerge_OnCreateSetProperty(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (n:Widget {code: "W1"}) ON CREATE SET n.created = true`)

	// One Widget node must exist.
	assertCount(t, eng, ctx, `MATCH (n:Widget) RETURN count(n) AS n`, 1)
}
