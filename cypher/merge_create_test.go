package cypher_test

// merge_create_test.go — T930
//
// Tests for the MERGE ON CREATE path. The engine's real searchFn (T930)
// returns matches from the live graph, so a second MERGE with the same
// pattern fires the ON MATCH branch instead of ON CREATE.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestMerge_CreateWhenNotPresent verifies idempotent MERGE semantics:
//
//   - On an empty graph, MERGE (n:Person {name:"Alice"}) creates one node.
//   - A second MERGE with the same pattern matches the existing node and
//     does NOT create a duplicate; the final count is still one.
func TestMerge_CreateWhenNotPresent(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// First MERGE on an empty graph — node must be created.
	drainRunInTx(t, eng, `MERGE (n:Person {name: "Alice"})`)

	assertCount(t, eng, ctx, `MATCH (n:Person) RETURN count(n) AS n`, 1)

	// Second MERGE with identical pattern — searchFn finds the existing
	// node, ON MATCH fires, and no duplicate is created.
	drainRunInTx(t, eng, `MERGE (n:Person {name: "Alice"})`)

	assertCount(t, eng, ctx, `MATCH (n:Person) RETURN count(n) AS n`, 1)
}

// TestMerge_OnCreateSet verifies that MERGE ON CREATE SET assigns a property
// to the newly created node.
func TestMerge_OnCreateSet(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `MERGE (n:Person {name: "Bob"}) ON CREATE SET n.created = true`)

	// Verify: some Person node must carry created=true.
	found := false
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		props := g.NodeProperties(key)
		if bv, ok := props["created"]; ok {
			if b, ok2 := bv.Bool(); ok2 && b {
				found = true
				return false
			}
		}
		return true
	})
	if !found {
		t.Fatal("expected created=true on node after MERGE ON CREATE SET")
	}
}
