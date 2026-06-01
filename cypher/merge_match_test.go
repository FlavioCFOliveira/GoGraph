package cypher_test

// merge_match_test.go — T930
//
// Verifies the real MERGE semantics: a MERGE on a pattern that already
// exists fires the ON MATCH branch and does NOT create a duplicate node.
//
// Prior to T930 [exec.Merge]'s searchFn was a stub that always returned no
// matches, so every MERGE call fired ON CREATE. Since T930 the searchFn
// scans the live graph for a node whose labels are a superset of the
// pattern labels and whose properties equal every (key, value) parsed from
// the pattern's property map.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestMerge_TwiceIsIdempotent verifies that calling MERGE twice with the
// same pattern leaves exactly one node in the graph.
func TestMerge_TwiceIsIdempotent(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (n:Person {name: "Alice"})`)
	drainRunInTx(t, eng, `MERGE (n:Person {name: "Alice"})`)

	assertCount(ctx, t, eng, `MATCH (n:Person) RETURN count(n) AS n`, 1)
}

// TestMerge_OnMatchSet verifies that MERGE on an existing node fires the
// ON MATCH branch — exactly one node remains and the on-match property
// assignment is applied.
func TestMerge_OnMatchSet(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed a node via plain CREATE so a Person definitely exists.
	drainRunInTx(t, eng, `CREATE (n:Person {name: "Carol"})`)

	// MERGE matches the seeded node and applies the on-match assignment.
	drainRunInTx(t, eng,
		`MERGE (n:Person {name: "Carol"}) ON MATCH SET n.matched = true`)

	// No duplicate: still exactly one Person.
	assertCount(ctx, t, eng, `MATCH (n:Person) RETURN count(n) AS n`, 1)
	// The on-match assignment landed on the node.
	assertCount(ctx, t, eng, `MATCH (n:Person {matched: true}) RETURN count(n) AS n`, 1)
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
	assertCount(ctx, t, eng, `MATCH (n:Widget) RETURN count(n) AS n`, 1)
}

// TestMerge_PartialPropertyMatchCreatesNew verifies that MERGE on a pattern
// whose property map differs from any existing node creates a fresh node —
// MERGE matches the entire pattern, not a primary-key subset.
func TestMerge_PartialPropertyMatchCreatesNew(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `MERGE (n:Person {name: "Alice", age: 30})`)
	// Same name, different age → distinct pattern → new node.
	drainRunInTx(t, eng, `MERGE (n:Person {name: "Alice", age: 31})`)

	assertCount(ctx, t, eng, `MATCH (n:Person) RETURN count(n) AS n`, 2)
}
