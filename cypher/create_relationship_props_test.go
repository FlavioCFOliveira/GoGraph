package cypher_test

// create_relationship_props_test.go — T765
//
// Additive tests for CREATE relationship with type and properties.
// Complements TestRunInTx_CreateRelationship which verifies basic creation
// without properties.

import (
	"context"
	"testing"

	"gograph/cypher"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestCreate_RelationshipWithProperties creates a KNOWS relationship with a
// {since: 2020} property and verifies the property is stored on the edge.
func TestCreate_RelationshipWithProperties(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed two nodes.
	drainRunInTx(t, eng, `CREATE (n:Person {name: "Alice"})`)
	drainRunInTx(t, eng, `CREATE (n:Person {name: "Bob"})`)

	// Create a relationship with a property.
	drainRunInTx(t, eng,
		`MATCH (a:Person), (b:Person)
		 WHERE a.name = "Alice" AND b.name = "Bob"
		 CREATE (a)-[r:KNOWS {since: 2020}]->(b)`)

	// Verify the edge count via the engine.
	assertCount(t, eng, ctx, `MATCH ()-[r:KNOWS]->() RETURN count(r) AS n`, 1)

	// Walk all (src, dst) pairs and look for since=2020 on the edge.
	found := false
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, srcKey string) bool {
		g.AdjList().Mapper().Walk(func(_ graph.NodeID, dstKey string) bool {
			props := g.EdgeProperties(srcKey, dstKey)
			if sv, ok := props["since"]; ok {
				if v, ok2 := sv.Int64(); ok2 && v == 2020 {
					found = true
					return false
				}
			}
			return true
		})
		return !found
	})
	if !found {
		t.Fatal("expected since=2020 on KNOWS edge after CREATE")
	}
}

// TestCreate_RelationshipThenVerifyEndpoints creates two nodes and a
// relationship, then confirms both endpoint-label indexes are non-empty.
func TestCreate_RelationshipThenVerifyEndpoints(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:Source)`)
	drainRunInTx(t, eng, `CREATE (n:Sink)`)
	drainRunInTx(t, eng, `MATCH (a:Source), (b:Sink) CREATE (a)-[:FLOWS]->(b)`)

	for _, label := range []string{"Source", "Sink"} {
		lid, ok := g.Registry().Lookup(label)
		if !ok {
			t.Errorf("label %q disappeared after CREATE relationship", label)
			continue
		}
		bm := g.NodeIndex().Intersect(uint32(lid))
		if bm.IsEmpty() {
			t.Errorf("no node with label %q after CREATE relationship", label)
		}
	}

	assertCount(t, eng, ctx, `MATCH ()-[r:FLOWS]->() RETURN count(r) AS n`, 1)
}

// TestCreate_RelationshipType verifies the edge type label is stored on the
// edge after CREATE and the engine reports the correct count via MATCH.
func TestCreate_RelationshipType(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:Animal {species: "cat"})`)
	drainRunInTx(t, eng, `CREATE (n:Animal {species: "dog"})`)
	drainRunInTx(t, eng,
		`MATCH (a:Animal), (b:Animal)
		 WHERE a.species = "cat" AND b.species = "dog"
		 CREATE (a)-[:CHASES]->(b)`)

	// Verify via edge labels in the graph.
	chaseCount := 0
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, srcKey string) bool {
		g.AdjList().Mapper().Walk(func(_ graph.NodeID, dstKey string) bool {
			for _, l := range g.EdgeLabels(srcKey, dstKey) {
				if l == "CHASES" {
					chaseCount++
				}
			}
			return true
		})
		return true
	})
	if chaseCount == 0 {
		t.Fatal("expected at least one CHASES edge label after CREATE relationship")
	}

	assertCount(t, eng, ctx, `MATCH ()-[r:CHASES]->() RETURN count(r) AS n`, 1)
}
