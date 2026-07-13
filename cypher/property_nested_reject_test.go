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
func runTxErr(ctx context.Context, t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value) error {
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

func mustReject(ctx context.Context, t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value) {
	t.Helper()
	err := runTxErr(ctx, t, eng, q, params)
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
	nl := map[string]expr.Value{"x": expr.ListValue{expr.ListValue{expr.IntegerValue(1)}}}
	mp := map[string]expr.Value{"x": expr.MapValue{"a": expr.IntegerValue(1)}}

	// single-property SET: nested list + map
	mustReject(ctx, t, eng, "CREATE (n:A) SET n.p = [[1,2],[3,4]]", nil)
	mustReject(ctx, t, eng, "CREATE (n:B) SET n.q = {a:1}", nil)
	// CREATE node inline: literal + param (nested list) + map literal
	mustReject(ctx, t, eng, "CREATE (n:C {p:[[1,2]]})", nil)
	mustReject(ctx, t, eng, "CREATE (n:D {p:$x})", nl)
	mustReject(ctx, t, eng, "CREATE (n:C2 {p:{a:1}})", nil)
	// CREATE relationship inline: map literal
	mustReject(ctx, t, eng, "CREATE (a:R1)-[r:R {p:{a:1}}]->(b:R2)", nil)
	// MERGE ON CREATE SET: nested list + map
	mustReject(ctx, t, eng, "MERGE (n:E {id:1}) ON CREATE SET n.p = [[9]]", nil)
	mustReject(ctx, t, eng, "MERGE (n:E2 {id:2}) ON CREATE SET n.q = {a:1}", nil)
	// MERGE-pattern search props via parameter: nested list + map
	mustReject(ctx, t, eng, "MERGE (n:F {p:$x})", nl)
	mustReject(ctx, t, eng, "MERGE (n:F2 {p:$x})", mp)
	// Whole-entity SET replace / merge, literal + param: nested list + map value
	mustReject(ctx, t, eng, "CREATE (n:G) SET n = {p:[[1]]}", nil)
	mustReject(ctx, t, eng, "CREATE (n:H) SET n += {p:{a:1}}", nil)
	mustReject(ctx, t, eng, "CREATE (n:I) SET n = {p:$x}", mp)
	mustReject(ctx, t, eng, "CREATE (n:J) SET n += {p:$x}", nl)
}

// TestProperty_WholeMapSetAccepted guards against a false positive on the
// LEGITIMATE whole-entity forms: SET n = {scalar map} / SET n = $scalarMap set
// the node's property set from a map of PRIMITIVE values and must succeed — only
// a nested VALUE inside such a map is rejected.
func TestProperty_WholeMapSetAccepted(t *testing.T) {
	t.Parallel()
	eng, ctx := newEng(t)
	sm := map[string]expr.Value{"p": expr.MapValue{"a": expr.IntegerValue(1), "b": expr.StringValue("x")}}
	if err := runTxErr(ctx, t, eng, "CREATE (n:A) SET n = $p", sm); err != nil {
		t.Fatalf("SET n = $scalarMap must succeed, got: %v", err)
	}
	if err := runTxErr(ctx, t, eng, "CREATE (n:B) SET n += $p", sm); err != nil {
		t.Fatalf("SET n += $scalarMap must succeed, got: %v", err)
	}
	if err := runTxErr(ctx, t, eng, "CREATE (n:C) SET n = {a:1, b:'x'}", nil); err != nil {
		t.Fatalf("SET n = {scalar map} must succeed, got: %v", err)
	}
}

// TestProperty_FlatCollectionsAccepted guards against a false positive: a flat
// list of primitives is a valid property value and must still be stored.
func TestProperty_FlatCollectionsAccepted(t *testing.T) {
	t.Parallel()
	eng, ctx := newEng(t)
	if err := runTxErr(ctx, t, eng, "CREATE (n:A {tags:['x','y','z'], nums:[1,2,3]}) SET n.more = [true, false]", nil); err != nil {
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
