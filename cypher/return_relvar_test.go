package cypher_test

// return_relvar_test.go — T504: RETURN bare relationship variable emits RelationshipValue.
//
// Tests:
//   - TestEngine_ReturnRelationshipVar: MATCH ()-[r:LIKES]->() RETURN r emits
//     a full expr.RelationshipValue carrying Type, Properties, StartID, EndID.
//   - TestEngine_ReturnAggregateAlias_Rel_NotUpgraded: count(r) AS cnt is NOT
//     upgraded to RelationshipValue (mirrors the NodeValue guard in T503).
//
// The Bolt end-to-end surface is covered by bolt/server/e2e_return_relationship_test.go
// (T784), so no Bolt test is added here.

import (
	"context"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestEngine_ReturnRelationshipVar verifies that MATCH (a)-[r:LIKES]->(b) RETURN r
// emits exactly one row whose "r" column is an expr.RelationshipValue with the
// correct Type, Properties, and endpoint IDs matching id(a)/id(b).
//
// Sequential (no t.Parallel): the test creates its own isolated graph and
// engine so there is no shared mutable state between runs, but running it in
// the sequential phase avoids co-scheduling with memory-intensive parallel
// tests. Under the race detector the combined heap of many large parallel tests
// can cause the runner to be OOM-killed, making the most-recently-scheduled
// test appear to fail non-deterministically. Serial execution is the simplest
// defence against that phenomenon.
func TestEngine_ReturnRelationshipVar(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	// Per-test deadline: if the engine somehow gets stuck under heavy system
	// load, the test should fail with a clear timeout message rather than
	// blocking indefinitely and confusing the failure report.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed: two nodes + one LIKES relationship with since=2020.
	drainRunInTx(t, eng, `CREATE (a:A {id: 1})-[r:LIKES {since: 2020}]->(b:B {id: 2})`)

	// Query: RETURN the bare relationship variable alongside the endpoint IDs,
	// so the StartID/EndID assertions below compare against the real node IDs.
	// Node IDs come from a shared counter and 0 is a valid ID, so a non-zero
	// check would be flaky under parallel test scheduling.
	res, err := eng.RunInTxAny(ctx, `MATCH (a)-[r:LIKES]->(b) RETURN r, id(a) AS ida, id(b) AS idb`, nil)
	if err != nil {
		t.Fatalf("RunInTxAny: %v", err)
	}
	rows := drainRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	raw, ok := rows[0]["r"]
	if !ok {
		t.Fatalf("record missing column %q (cols: %v)", "r", rows[0])
	}

	rv, ok := raw.(expr.RelationshipValue)
	if !ok {
		t.Fatalf("RETURN r must emit expr.RelationshipValue, got %T (%v)", raw, raw)
	}

	if rv.Type != "LIKES" {
		t.Errorf("RelationshipValue.Type = %q, want %q", rv.Type, "LIKES")
	}

	since, ok := rv.Properties["since"].(expr.IntegerValue)
	if !ok {
		t.Errorf("RelationshipValue.Properties[%q] = %v (%T), want expr.IntegerValue(2020)",
			"since", rv.Properties["since"], rv.Properties["since"])
	} else if int64(since) != 2020 {
		t.Errorf("RelationshipValue.Properties[%q] = %d, want 2020", "since", int64(since))
	}

	ida, ok := rows[0]["ida"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("id(a) must be expr.IntegerValue, got %T (%v)", rows[0]["ida"], rows[0]["ida"])
	}
	idb, ok := rows[0]["idb"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("id(b) must be expr.IntegerValue, got %T (%v)", rows[0]["idb"], rows[0]["idb"])
	}
	if rv.StartID != uint64(ida) {
		t.Errorf("RelationshipValue.StartID = %d, want id(a) = %d", rv.StartID, int64(ida))
	}
	if rv.EndID != uint64(idb) {
		t.Errorf("RelationshipValue.EndID = %d, want id(b) = %d", rv.EndID, int64(idb))
	}
	if rv.StartID == rv.EndID {
		t.Errorf("RelationshipValue.StartID (%d) == EndID (%d), expected distinct endpoints",
			rv.StartID, rv.EndID)
	}
}

// TestEngine_ReturnAggregateAlias_Rel_NotUpgraded guards the regression where
// an aggregate output (count(r)) that numerically collides with an existing
// relationship ID is NOT upgraded to RelationshipValue. Mirrors the node-variable
// guard in TestEngine_ReturnAggregateAlias_NotUpgraded (return_node_shape_test.go).
func TestEngine_ReturnAggregateAlias_Rel_NotUpgraded(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed exactly one LIKES relationship so count(r) == 1.
	drainRunInTx(t, eng, `CREATE (a:A {id: 1})-[r:LIKES]->(b:B {id: 2})`)

	res, err := eng.RunInTxAny(ctx, `MATCH ()-[r:LIKES]->() RETURN count(r) AS cnt`, nil)
	if err != nil {
		t.Fatalf("RunInTxAny: %v", err)
	}
	rows := drainRecords(t, res)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	got, ok := rows[0]["cnt"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("count(r) AS cnt must remain expr.IntegerValue, got %T (%v)",
			rows[0]["cnt"], rows[0]["cnt"])
	}
	if int64(got) != 1 {
		t.Errorf("count(r) = %d, want 1", int64(got))
	}
}
