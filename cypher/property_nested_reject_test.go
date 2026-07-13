package cypher_test

// property_nested_reject_test.go — regression for the nested-collection
// property blocker (2026-07-13 audit, security F3). openCypher restricts a
// property value to a primitive or a flat list of primitives; a nested list or
// a map is InvalidPropertyType. Previously such a value was accepted and stored
// (SET / CREATE) or silently dropped, creating a store inconsistency (the
// snapshot codec cannot serialise a nested PropList) and a checkpoint-stall
// hazard. Every write path now fail-stops.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func newEng(t *testing.T) (*cypher.Engine, context.Context) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngineWithRegistry(g, funcs.DefaultRegistry), context.Background()
}

// runTxErr runs a write statement and returns the error surfaced by either Run
// or the lazy result stream.
func runTxErr(t *testing.T, eng *cypher.Engine, ctx context.Context, q string, params map[string]expr.Value) error {
	t.Helper()
	res, err := eng.RunInTx(ctx, q, params)
	if err != nil {
		return err
	}
	if res == nil {
		return nil
	}
	for res.Next() { //nolint:revive // drain to surface the lazy error
	}
	e := res.Err()
	res.Close()
	return e
}

func mustReject(t *testing.T, eng *cypher.Engine, ctx context.Context, q string, params map[string]expr.Value) {
	t.Helper()
	err := runTxErr(t, eng, ctx, q, params)
	if err == nil {
		t.Fatalf("%q stored/dropped an invalid property silently, want InvalidPropertyType", q)
	}
	if !strings.Contains(err.Error(), "InvalidPropertyType") {
		t.Fatalf("%q error = %v, want InvalidPropertyType", q, err)
	}
}

func TestProperty_NestedList_AllWritePathsRejected(t *testing.T) {
	t.Parallel()
	eng, ctx := newEng(t)
	// SET RHS
	mustReject(t, eng, ctx, "CREATE (n:A) SET n.p = [[1,2],[3,4]]", nil)
	// SET map
	mustReject(t, eng, ctx, "CREATE (n:B) SET n.q = {a:1}", nil)
	// CREATE inline literal
	mustReject(t, eng, ctx, "CREATE (n:C {p:[[1,2]]})", nil)
	// CREATE inline via parameter
	mustReject(t, eng, ctx, "CREATE (n:D {p:$x})",
		map[string]expr.Value{"x": expr.ListValue{expr.ListValue{expr.IntegerValue(1)}}})
	// MERGE ON CREATE SET
	mustReject(t, eng, ctx, "MERGE (n:E {id:1}) ON CREATE SET n.p = [[9]]", nil)
}

// TestProperty_FlatCollectionsAccepted guards against a false positive: a flat
// list of primitives is a valid property value and must still be stored.
func TestProperty_FlatCollectionsAccepted(t *testing.T) {
	t.Parallel()
	eng, ctx := newEng(t)
	if err := runTxErr(t, eng, ctx, "CREATE (n:A {tags:['x','y','z'], nums:[1,2,3]}) SET n.more = [true, false]", nil); err != nil {
		t.Fatalf("flat-list properties must be accepted, got: %v", err)
	}
	res, err := eng.Run(ctx, "MATCH (n:A) RETURN n.tags AS t, n.nums AS m", nil)
	if err != nil {
		t.Fatalf("read-back Run: %v", err)
	}
	defer res.Close()
	if !res.Next() {
		t.Fatal("no rows read back")
	}
}
