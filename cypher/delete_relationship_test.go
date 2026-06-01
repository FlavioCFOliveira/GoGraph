package cypher_test

// delete_relationship_test.go — DELETE relationship tests (T851).
//
// Engine gap (as of T851): the Cypher planner always emits DeleteNode for
// DELETE expressions regardless of whether the target variable is a node or a
// relationship. When MATCH binds a relationship variable [r], that variable
// holds an IntegerValue (edge ID) in the current execution model. DeleteNode
// tries to resolve it as a NodeID via ResolveNodeLabel, gets a miss, and
// treats the operation as a no-op. The edge is NOT removed.
//
// Until the planner is fixed to emit DeleteRelationship for relationship
// variables, MATCH … [r] … DELETE r leaves the edge intact. These tests
// document the current, observable behaviour and will need updating once the
// planner gap is closed.
//
// For edge removal that works today, callers should use DETACH DELETE on the
// source node (removes node + all incident edges) or remove edges directly
// via the lpg.Graph API.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestDelete_RelationshipPlannerGap asserts that `DELETE r` (where r is a
// bare relationship variable) removes the edge. The DeleteNode operator
// dispatches to mutator.RemoveEdge via the bound-rel endpoint lookup
// installed in api.go's DeleteNode case (T1049). Before that fix the
// dispatch treated the edge-id as a node-id and either silently no-op'd
// or raised ErrDeleteNodeHasRelationships.
func TestDelete_RelationshipPlannerGap(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (:RelA {name: "alice"})`)
	drainRunInTx(t, eng, `CREATE (:RelB {name: "bob"})`)

	aliceKey := synthKeyForLabel(t, g, "RelA")
	bobKey := synthKeyForLabel(t, g, "RelB")

	if err := g.AddEdge(aliceKey, bobKey, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	// MATCH finds the edge (1 row expected).
	matchRes, err := eng.RunInTx(ctx, `MATCH (a:RelA)-[r]->(b:RelB) RETURN r`, nil)
	if err != nil {
		t.Fatalf("MATCH: %v", err)
	}
	matchRows := collectRecords(t, matchRes)
	if len(matchRows) != 1 {
		t.Fatalf("expected 1 row from MATCH, got %d", len(matchRows))
	}

	// DELETE r now removes the edge.
	res, runErr := eng.RunInTx(ctx, `MATCH (a:RelA)-[r]->(b:RelB) DELETE r`, nil)
	if runErr != nil {
		t.Fatalf("DELETE r error: %v", runErr)
	}
	for res.Next() {
	}
	if iterErr := res.Err(); iterErr != nil {
		t.Fatalf("DELETE r iter: %v", iterErr)
	}
	res.Close()

	if g.AdjList().HasEdge(aliceKey, bobKey) {
		t.Errorf("DELETE r did not remove the edge between %q and %q", aliceKey, bobKey)
	}
}

// TestDelete_RelationshipViaDetachDelete demonstrates the recommended
// workaround: use DETACH DELETE on the source node to remove both the node
// and its incident edges.
func TestDelete_RelationshipViaDetachDelete(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	drainRunInTx(t, eng, `CREATE (:SrcDel {name: "src"})`)
	drainRunInTx(t, eng, `CREATE (:DstDel {name: "dst"})`)

	srcKey := synthKeyForLabel(t, g, "SrcDel")
	dstKey := synthKeyForLabel(t, g, "DstDel")

	if err := g.AddEdge(srcKey, dstKey, 1.0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	if !g.AdjList().HasEdge(srcKey, dstKey) {
		t.Fatal("edge not present before DETACH DELETE")
	}

	// DETACH DELETE the source: removes it and its outgoing edges.
	drainRunInTx(t, eng, `MATCH (n:SrcDel) DETACH DELETE n`)

	// Edge is gone.
	if g.AdjList().HasEdge(srcKey, dstKey) {
		t.Error("edge still present after DETACH DELETE src")
	}

	// dst (DstDel) must still exist.
	lid, ok := g.Registry().Lookup("DstDel")
	if !ok {
		t.Fatal("DstDel label not registered")
	}
	if bm := g.NodeIndex().Intersect(uint32(lid)); bm.IsEmpty() {
		t.Fatal("DstDel node gone after DETACH DELETE src")
	}

	// Verify via Cypher count.
	countRes, err := eng.RunInTx(context.Background(), `MATCH (n:DstDel) RETURN count(*) AS cnt`, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	rows := collectRecords(t, countRes)
	if cnt, ok := rows[0]["cnt"].(expr.IntegerValue); !ok || int64(cnt) != 1 {
		t.Errorf("DstDel count = %v, want 1", rows[0]["cnt"])
	}
}
