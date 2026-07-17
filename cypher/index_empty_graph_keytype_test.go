package cypher_test

// index_empty_graph_keytype_test.go — regression for rmp #1983.
//
// CREATE INDEX (hash, the default kind) over an EMPTY graph used to pin the
// index key type permanently to String: the engine's parameter type-inference
// (indexedPropKind → hashIndexKind) read the string-keyed hash index as an
// authoritative "this property is String" signal even though the graph held no
// data at creation time. Seeding an INTEGER-valued property afterwards did not
// retype the index, so a later `MATCH (n:L {p:$id})` with an INTEGER parameter
// was rejected with "cypher: parameter $id: expected String value, got Integer"
// instead of returning the node.
//
// The fix defers key-type determination until the first value is indexed: an
// EMPTY string hash index yields no authoritative type signal, so the parameter
// is accepted and the query falls back to scan+filter, returning the correct
// rows per openCypher. A non-empty string hash index still proves String (see
// TestRun_ParamTypeMismatch_Error), so the deliberate type-mismatch rejection
// for a genuinely string-populated property is unchanged.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestCreateIndexEmptyGraph_IntegerParamMatches is the #1983 repro: CREATE INDEX
// on an empty graph, then seed an integer-valued property, then MATCH with an
// integer parameter — the node must be returned.
func TestCreateIndexEmptyGraph_IntegerParamMatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	// CREATE INDEX over the EMPTY graph — the hash index defaults to string-keyed.
	res, err := eng.Run(ctx, `CREATE INDEX l_p FOR (n:L) ON (n.p)`, nil)
	if err != nil {
		t.Fatalf("CREATE INDEX on empty graph: %v", err)
	}
	_ = res.Close()

	// Seed an INTEGER-valued property AFTER the index exists.
	res, err = eng.RunInTxAny(ctx, `CREATE (n:L {p: 5})`, nil)
	if err != nil {
		t.Fatalf("CREATE (n:L {p: 5}): %v", err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if cerr := res.Err(); cerr != nil {
		t.Fatalf("CREATE node result error: %v", cerr)
	}
	_ = res.Close()

	// MATCH with an INTEGER parameter must return the node — the empty-at-creation
	// index must not pin the parameter type to String.
	res, err = eng.Run(ctx, `MATCH (n:L {p:$id}) RETURN n.p AS p`,
		map[string]expr.Value{"id": expr.IntegerValue(5)})
	if err != nil {
		t.Fatalf("MATCH (n:L {p:$id}) with integer param: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("want 1 row for p=5, got %d (%v)", len(rows), rows)
	}
	iv, ok := rows[0]["p"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("p: expected IntegerValue, got %T (%v)", rows[0]["p"], rows[0]["p"])
	}
	if int64(iv) != 5 {
		t.Errorf("p = %d, want 5", int64(iv))
	}
}

// TestCreateIndexEmptyGraph_IntegerParamMatches_WhereForm covers the same defect
// via the WHERE-equality form, which is the shape the parameter type-inference
// pass matches directly.
func TestCreateIndexEmptyGraph_IntegerParamMatches_WhereForm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	res, err := eng.Run(ctx, `CREATE INDEX l_p2 FOR (n:L) ON (n.p)`, nil)
	if err != nil {
		t.Fatalf("CREATE INDEX on empty graph: %v", err)
	}
	_ = res.Close()

	res, err = eng.RunInTxAny(ctx, `CREATE (n:L {p: 7})`, nil)
	if err != nil {
		t.Fatalf("CREATE (n:L {p: 7}): %v", err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if cerr := res.Err(); cerr != nil {
		t.Fatalf("CREATE node result error: %v", cerr)
	}
	_ = res.Close()

	res, err = eng.Run(ctx, `MATCH (n:L) WHERE n.p = $id RETURN n.p AS p`,
		map[string]expr.Value{"id": expr.IntegerValue(7)})
	if err != nil {
		t.Fatalf("MATCH ... WHERE n.p = $id with integer param: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("want 1 row for p=7, got %d (%v)", len(rows), rows)
	}
}
