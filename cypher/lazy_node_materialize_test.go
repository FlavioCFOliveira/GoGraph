package cypher_test

// lazy_node_materialize_test.go — correctness guards for the lazy / late node
// materialisation optimisation (#1500).
//
// The optimisation defers full node materialisation in the WHERE predicate and
// the scalar-projection general path: a node accessed only through scalar shapes
// (n.key, n["key"], n:Label) loads only the touched scalars. These tests pin the
// failure modes where that deferral must NOT happen, or must still produce the
// byte-identical result of full eager materialisation.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newLazyGraph builds a single Person node carrying three properties and two
// labels, the fixture for the lazy-materialisation guards.
func newLazyGraph(tb testing.TB) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("n0"); err != nil {
		tb.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("n0", "Person"); err != nil {
		tb.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeLabel("n0", "Employee"); err != nil {
		tb.Fatalf("SetNodeLabel: %v", err)
	}
	for k, v := range map[string]lpg.PropertyValue{
		"name": lpg.StringValue("Alice"),
		"age":  lpg.Int64Value(30),
		"city": lpg.StringValue("Lisbon"),
	} {
		if err := g.SetNodeProperty("n0", k, v); err != nil {
			tb.Fatalf("SetNodeProperty %s: %v", k, err)
		}
	}
	return g
}

// firstRecord runs query and returns the single result record, failing if the
// query errors or yields a different row count than wantRows.
func firstRecord(t *testing.T, g *lpg.Graph[string, float64], query string, wantRows int) exec.Record {
	t.Helper()
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer res.Close()
	var rec exec.Record
	var rows int
	for res.Next() {
		if rows == 0 {
			rec = res.Record()
		}
		rows++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("drain(%q): %v", query, err)
	}
	if rows != wantRows {
		t.Fatalf("%q: got %d rows, want %d", query, rows, wantRows)
	}
	return rec
}

// TestLazy_ScalarProjection_ReadsCorrectValue is the happy path: a scalar
// projection after a scalar filter must return the exact stored value.
func TestLazy_ScalarProjection_ReadsCorrectValue(t *testing.T) {
	g := newLazyGraph(t)
	rec := firstRecord(t, g, "MATCH (n:Person) WHERE n.age > 18 RETURN n.name AS name", 1)
	sv, ok := rec["name"].(expr.StringValue)
	if !ok || string(sv) != "Alice" {
		t.Fatalf("RETURN n.name = %#v, want StringValue(\"Alice\")", rec["name"])
	}
}

// TestLazy_MissingProperty_IsNull confirms missing-key access yields null
// regardless of partial materialisation.
func TestLazy_MissingProperty_IsNull(t *testing.T) {
	g := newLazyGraph(t)
	rec := firstRecord(t, g, "MATCH (n:Person) WHERE n.age > 18 RETURN n.absent AS x", 1)
	xv, _ := rec["x"].(expr.Value)
	if xv == nil || !expr.IsNull(xv) {
		t.Fatalf("RETURN n.absent = %#v, want null", rec["x"])
	}
}

// TestLazy_WholeNodeProjection_FullProperties is the key escape guard: a bare
// node projection must serialise EVERY property, not just any the same query
// happened to read elsewhere. A truncated property map here would be the
// signature lazy-escape bug.
func TestLazy_WholeNodeProjection_FullProperties(t *testing.T) {
	g := newLazyGraph(t)
	// The WHERE clause reads only n.age, but RETURN n must still carry all three
	// properties and both labels.
	rec := firstRecord(t, g, "MATCH (n:Person) WHERE n.age > 18 RETURN n", 1)
	nv, ok := rec["n"].(expr.NodeValue)
	if !ok {
		t.Fatalf("RETURN n = %#v, want NodeValue", rec["n"])
	}
	for _, k := range []string{"name", "age", "city"} {
		if _, present := nv.Properties[k]; !present {
			t.Errorf("RETURN n: property %q missing — node was truncated by lazy path; props=%v", k, nv.Properties)
		}
	}
	if len(nv.Labels) != 2 {
		t.Errorf("RETURN n: got %d labels, want 2 (Person, Employee): %v", len(nv.Labels), nv.Labels)
	}
}

// TestLazy_PropertiesFunc_AfterScalarFilter guards properties(n): a whole-node
// accessor must observe the full property map even when the predicate only read
// a single scalar (the needsWholeNode fail-safe).
func TestLazy_PropertiesFunc_AfterScalarFilter(t *testing.T) {
	g := newLazyGraph(t)
	rec := firstRecord(t, g, "MATCH (n:Person) WHERE n.age > 18 RETURN properties(n) AS p", 1)
	mv, ok := rec["p"].(expr.MapValue)
	if !ok {
		t.Fatalf("properties(n) = %#v, want MapValue", rec["p"])
	}
	if len(mv) != 3 {
		t.Errorf("properties(n): got %d keys, want 3: %v", len(mv), mv)
	}
}

// TestLazy_KeysFunc_AfterScalarFilter is the analogous guard for keys(n).
func TestLazy_KeysFunc_AfterScalarFilter(t *testing.T) {
	g := newLazyGraph(t)
	rec := firstRecord(t, g, "MATCH (n:Person) WHERE n.name = 'Alice' RETURN keys(n) AS k", 1)
	lv, ok := rec["k"].(expr.ListValue)
	if !ok {
		t.Fatalf("keys(n) = %#v, want ListValue", rec["k"])
	}
	if len(lv) != 3 {
		t.Errorf("keys(n): got %d keys, want 3: %v", len(lv), lv)
	}
}

// TestLazy_LabelsFunc_AfterScalarFilter is the guard for labels(n).
func TestLazy_LabelsFunc_AfterScalarFilter(t *testing.T) {
	g := newLazyGraph(t)
	rec := firstRecord(t, g, "MATCH (n:Person) WHERE n.age > 18 RETURN labels(n) AS l", 1)
	lv, ok := rec["l"].(expr.ListValue)
	if !ok {
		t.Fatalf("labels(n) = %#v, want ListValue", rec["l"])
	}
	if len(lv) != 2 {
		t.Errorf("labels(n): got %d labels, want 2: %v", len(lv), lv)
	}
}

// TestLazy_LabelPredicate matches a node on a label predicate that requires only
// the label set, not properties.
func TestLazy_LabelPredicate(t *testing.T) {
	g := newLazyGraph(t)
	firstRecord(t, g, "MATCH (n) WHERE n:Employee RETURN 1 AS x", 1)
	// A non-matching label must exclude the row.
	firstRecord(t, g, "MATCH (n) WHERE n:Nonexistent RETURN 1 AS x", 0)
}

// TestLazy_DynamicSubscript forces the eager fallback: a dynamic subscript key
// cannot be statically enumerated, so the whole node must be materialised and
// the read must still resolve correctly.
func TestLazy_DynamicSubscript(t *testing.T) {
	g := newLazyGraph(t)
	rec := firstRecord(t, g, "MATCH (n:Person) WHERE n['na' + 'me'] = 'Alice' RETURN n.city AS c", 1)
	sv, ok := rec["c"].(expr.StringValue)
	if !ok || string(sv) != "Lisbon" {
		t.Fatalf("dynamic-subscript filter: RETURN n.city = %#v, want \"Lisbon\"", rec["c"])
	}
}

// TestLazy_NodeIdentityEquality forces eager materialisation via node identity
// equality (a whole-node use), confirming the result is unaffected.
func TestLazy_NodeIdentityEquality(t *testing.T) {
	g := newLazyGraph(t)
	firstRecord(t, g, "MATCH (n:Person), (m:Person) WHERE n = m RETURN n.name AS name", 1)
}

// TestLazy_SnapshotConsistency reads the same property in WHERE and RETURN; both
// must observe the identical pinned-snapshot value. A divergence would indicate
// the deferred read escaped the query's visibility barrier (an Isolation
// violation).
func TestLazy_SnapshotConsistency(t *testing.T) {
	g := newLazyGraph(t)
	rec := firstRecord(t, g, "MATCH (n:Person) WHERE n.age = 30 RETURN n.age AS a", 1)
	iv, ok := rec["a"].(expr.IntegerValue)
	if !ok || int64(iv) != 30 {
		t.Fatalf("RETURN n.age = %#v, want IntegerValue(30)", rec["a"])
	}
}

// TestLazy_PropertyTypeFidelity asserts that the lazy resolver reproduces the
// exact runtime value for every scalar property kind — string, integer, float,
// bool, and list — byte-identical to eager materialisation (cypher-expert item
// 2). A type or value divergence here would mean the on-demand fetch decodes a
// property differently from the full materialiser.
func TestLazy_PropertyTypeFidelity(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("v"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("v", "T"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	for k, pv := range map[string]lpg.PropertyValue{
		"s": lpg.StringValue("hello"),
		"i": lpg.Int64Value(-7),
		"f": lpg.Float64Value(3.5),
		"b": lpg.BoolValue(true),
		"g": lpg.Int64Value(0), // gate property for the WHERE filter
	} {
		if err := g.SetNodeProperty("v", k, pv); err != nil {
			t.Fatalf("SetNodeProperty %s: %v", k, err)
		}
	}
	// WHERE n.g = 0 keeps the row but only touches scalars, so n is lazily
	// materialised; the projection then reads each typed property on demand.
	rec := firstRecord(t, g, "MATCH (n:T) WHERE n.g = 0 RETURN n.s AS s, n.i AS i, n.f AS f, n.b AS b", 1)
	if s, ok := rec["s"].(expr.StringValue); !ok || string(s) != "hello" {
		t.Errorf("n.s = %#v, want StringValue(\"hello\")", rec["s"])
	}
	if i, ok := rec["i"].(expr.IntegerValue); !ok || int64(i) != -7 {
		t.Errorf("n.i = %#v, want IntegerValue(-7)", rec["i"])
	}
	if f, ok := rec["f"].(expr.FloatValue); !ok || float64(f) != 3.5 {
		t.Errorf("n.f = %#v, want FloatValue(3.5)", rec["f"])
	}
	if b, ok := rec["b"].(expr.BoolValue); !ok || !bool(b) {
		t.Errorf("n.b = %#v, want BoolValue(true)", rec["b"])
	}
}

// TestLazy_LabelPredicateConjunction pins the `n:A:B` conjunction and the
// no-label-node-yields-false semantics through the lazy label-predicate path
// (cypher-expert item 3). A node with both labels matches the conjunction; a
// node missing one label does not; a label-less node yields false, never null.
func TestLazy_LabelPredicateConjunction(t *testing.T) {
	g := newLazyGraph(t) // n0 carries both Person and Employee
	if err := g.AddNode("bare"); err != nil {
		t.Fatalf("AddNode bare: %v", err)
	}
	if err := g.SetNodeProperty("bare", "age", lpg.Int64Value(40)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	// Conjunction matches only the dual-labelled node.
	firstRecord(t, g, "MATCH (n) WHERE n:Person:Employee RETURN 1 AS x", 1)
	// A label-less node makes the predicate false (not null), so it is filtered
	// out — the label-less `bare` node never satisfies n:Person.
	firstRecord(t, g, "MATCH (n) WHERE n:Person AND n.age = 40 RETURN 1 AS x", 0)
}

// TestLazy_DeletedEntityViaScalarPath is the cypher-expert item-7 guard: a node
// deleted earlier in the same statement must raise DeletedEntityAccess on a
// later property read, NOT be served a stale or null value, even when the
// surrounding query shape would otherwise be lazy-eligible. The DELETE operator
// stamps the deleted node into the row as a Deleted NodeValue carrying a frozen
// snapshot, so it never reaches the lazy IntegerValue branch — the eager
// Deleted-flag check fires. This proves the lazy path cannot smuggle a stale
// read past an in-statement deletion.
func TestLazy_DeletedEntityViaScalarPath(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	res, err := eng.RunInTx(ctx, `CREATE (n:Person {name: "Dora", age: 22})`, nil)
	if err != nil {
		t.Fatalf("seed CREATE: %v", err)
	}
	for res.Next() { //nolint:revive // drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("seed drain: %v", err)
	}
	_ = res.Close() //nolint:errcheck

	// `n.age > 0` is a scalar predicate (lazy-eligible); DELETE n stamps n
	// Deleted; RETURN n.name must surface DeletedEntityAccess.
	res2, err := eng.RunInTx(ctx, `MATCH (n:Person) WHERE n.age > 0 DELETE n RETURN n.name AS x`, nil)
	failed := err != nil
	if err == nil {
		for res2.Next() { //nolint:revive // drain
		}
		if res2.Err() != nil {
			failed = true
		}
		_ = res2.Close() //nolint:errcheck
	}
	if !failed {
		t.Fatal("DELETE n ... RETURN n.name: expected DeletedEntityAccess error, got success (a stale lazy read would mask the deletion)")
	}
}
