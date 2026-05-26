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

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestEngine_ReturnRelationshipVar verifies that MATCH (a)-[r:LIKES]->(b) RETURN r
// emits exactly one row whose "r" column is an expr.RelationshipValue with the
// correct Type, Properties, and non-zero endpoint IDs.
func TestEngine_ReturnRelationshipVar(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Seed: two nodes + one LIKES relationship with since=2020.
	drainRunInTx(t, eng, `CREATE (a:A {id: 1})-[r:LIKES {since: 2020}]->(b:B {id: 2})`)

	// Query: RETURN the bare relationship variable.
	res, err := eng.RunInTxAny(ctx, `MATCH (a)-[r:LIKES]->(b) RETURN r`, nil)
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

	if rv.StartID == 0 {
		t.Errorf("RelationshipValue.StartID is zero")
	}
	if rv.EndID == 0 {
		t.Errorf("RelationshipValue.EndID is zero")
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
