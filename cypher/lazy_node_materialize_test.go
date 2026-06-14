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
