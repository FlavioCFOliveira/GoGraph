package cypher_test

// create_multi_pattern_regression_test.go — T769
//
// Regression tests for CREATE multi-pattern in one clause (ed0063c fix).
// Distinct from TestRunInTx_MultiEdgeSingleCreate (3 nodes + 3 edges cycle)
// and TestRunInTx_MultiEdgeBidirectional (2 nodes + 2 edges).
// These tests focus on the minimal node+edge shape and the large-N shape.

import (
	"context"
	"fmt"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestCreate_MultiPattern_ThreePatterns creates two named nodes and one edge
// between them in a single CREATE clause and asserts exactly 2 nodes, 1 edge,
// and both Person labels.
func TestCreate_MultiPattern_ThreePatterns(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx,
		`CREATE (a:Person {name: "A"}), (b:Person {name: "B"}), (a)-[:KNOWS]->(b)`, nil)
	if err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	_ = drainRecords(t, res)

	// Exactly 2 Person nodes.
	assertCount(ctx, t, eng, `MATCH (n:Person) RETURN count(n) AS n`, 2)

	// Exactly 1 KNOWS edge.
	assertCount(ctx, t, eng, `MATCH ()-[r:KNOWS]->() RETURN count(r) AS n`, 1)

	// Both Person labels in the registry.
	lid, ok := g.Registry().Lookup("Person")
	if !ok {
		t.Fatal("label Person not registered after CREATE")
	}
	bm := g.NodeIndex().Intersect(uint32(lid))
	if bm.IsEmpty() {
		t.Fatal("no Person nodes in index after CREATE")
	}
}

// TestCreate_MultiPattern_TenNodes creates 10 nodes in one CREATE clause and
// verifies that all 10 are present and no extra nodes were created.
func TestCreate_MultiPattern_TenNodes(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Build: CREATE (n0:Batch {i:0}), (n1:Batch {i:1}), …, (n9:Batch {i:9})
	const n = 10
	parts := make([]string, n)
	for i := range n {
		parts[i] = fmt.Sprintf(`(n%d:Batch {i: %d})`, i, i)
	}
	query := "CREATE " + joinStrings(parts, ", ")

	res, err := eng.RunInTx(ctx, query, nil)
	if err != nil {
		t.Fatalf("CREATE 10 nodes: %v", err)
	}
	_ = drainRecords(t, res)

	assertCount(ctx, t, eng, `MATCH (n:Batch) RETURN count(n) AS n`, int64(n))
}

// joinStrings joins a slice of strings with sep.
func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	out := ss[0]
	for _, s := range ss[1:] {
		out += sep + s
	}
	return out
}
