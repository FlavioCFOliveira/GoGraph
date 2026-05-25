package cypher_test

// detach_delete_large_test.go — DETACH DELETE hub with many leaves (T848).
//
// Short layer: hub with 1_000 leaves. Leaves are injected via the lpg.Graph
// API for speed; only the DETACH DELETE itself uses Cypher.
//
// The hub node is created via Cypher to obtain a labelled synthetic key that
// MATCH can locate; leaves are created directly via g.AddNode + g.AddEdge
// to avoid Cypher loop overhead.
//
// Semantics: after DETACH DELETE (hub), the hub node is stripped of labels and
// properties, and all edges to/from it are removed. Leaves remain as isolated
// nodes.

import (
	"context"
	"fmt"
	"testing"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

func TestDetachDelete_Hub1000Leaves(t *testing.T) {
	t.Parallel()

	const leaves = 1_000

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// Create hub via Cypher so the label index knows about it.
	drainRunInTx(t, eng, `CREATE (n:Hub {name: "hub"})`)

	hubKey := synthKeyForLabel(t, g, "Hub")

	// Add leaves and edges via the LPG API for speed.
	for i := range leaves {
		leafKey := fmt.Sprintf("leaf%d", i)
		if err := g.AddNode(leafKey); err != nil {
			t.Fatalf("AddNode leaf%d: %v", i, err)
		}
		if err := g.AddEdge(hubKey, leafKey, 1.0); err != nil {
			t.Fatalf("AddEdge hub->leaf%d: %v", i, err)
		}
	}

	// DETACH DELETE the hub.
	drainRunInTx(t, eng, `MATCH (n:Hub {name:"hub"}) DETACH DELETE n`)

	// Hub must be gone from the label index.
	lid, ok := g.Registry().Lookup("Hub")
	if ok {
		bm := g.NodeIndex().Intersect(uint32(lid))
		if !bm.IsEmpty() {
			t.Fatal("hub node still in Hub label index after DETACH DELETE")
		}
	}

	// Leaves must all survive (they have no labels but they are in the mapper).
	// Verify count via Cypher: MATCH (n) without label-filter over all nodes
	// using the LPG API. We iterate the mapper directly for speed.
	// Expected: 1_000 leaf keys still present.
	leafCount := 0
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		if len(key) >= 4 && key[:4] == "leaf" {
			leafCount++
		}
		return true
	})
	if leafCount != leaves {
		t.Errorf("expected %d leaves after DETACH DELETE hub, got %d", leaves, leafCount)
	}

	// Hub edges must be gone: no node should have edges to/from hubKey.
	if g.AdjList().HasEdge(hubKey, "leaf0") {
		t.Error("edge hub->leaf0 still present after DETACH DELETE")
	}
}

// TestDetachDelete_HubCount verifies via Cypher count(*) that the hub is gone
// after DETACH DELETE.
func TestDetachDelete_HubCount(t *testing.T) {
	t.Parallel()

	const leaves = 10

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (n:HubC {name: "hubC"})`)
	hubKey := synthKeyForLabel(t, g, "HubC")
	for i := range leaves {
		lk := fmt.Sprintf("lc%d", i)
		if err := g.AddNode(lk); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.AddEdge(hubKey, lk, 1.0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}

	drainRunInTx(t, eng, `MATCH (n:HubC) DETACH DELETE n`)

	res, err := eng.RunInTx(ctx, `MATCH (n:HubC) RETURN count(*) AS cnt`, nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("expected 1 aggregation row, got %d", len(rows))
	}
	cnt, ok := rows[0]["cnt"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("cnt: expected IntegerValue, got %T", rows[0]["cnt"])
	}
	if int64(cnt) != 0 {
		t.Errorf("count(HubC) = %d after DETACH DELETE, want 0", int64(cnt))
	}
}
