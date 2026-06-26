package cypher_test

// mapprojection_test.go — end-to-end behavioural tests for map projection
// (openCypher CIP2014-12-12, #1775). Each test drives a query through the
// Engine and asserts the projected map value, covering: property selectors,
// the .* all-properties selector, literal entries, variable selectors, a map
// variable subject, and null handling for a missing property selector.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runOneMap runs a query expected to produce exactly one row and returns the
// MapValue projected under the given column.
func runOneMap(t *testing.T, eng *cypher.Engine, query, col string) expr.MapValue {
	t.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("Run(%q): got %d rows, want 1", query, len(rows))
	}
	m, ok := rows[0][col].(expr.MapValue)
	if !ok {
		t.Fatalf("Run(%q): column %q = %T (%v), want expr.MapValue", query, col, rows[0][col], rows[0][col])
	}
	return m
}

// newMapProjPersonEngine builds an engine over a single :Person node created via
// Cypher, carrying name (STRING) and age (INTEGER) properties.
func newMapProjPersonEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	res, err := eng.RunAny(context.Background(),
		"CREATE (:Person {name: 'Alice', age: 30})", nil)
	if err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("CREATE drain: %v", err)
	}
	res.Close()
	return eng
}

func TestMapProjection_PropertySelectors(t *testing.T) {
	eng := newMapProjPersonEngine(t)
	m := runOneMap(t, eng, "MATCH (n:Person) RETURN n{.name, .age} AS m", "m")

	if got := m["name"]; got != expr.StringValue("Alice") {
		t.Errorf("m.name = %v, want 'Alice'", got)
	}
	if got := m["age"]; got != expr.IntegerValue(30) {
		t.Errorf("m.age = %v, want 30", got)
	}
	if len(m) != 2 {
		t.Errorf("map has %d keys (%v), want exactly name+age", len(m), m)
	}
}

func TestMapProjection_AllProperties(t *testing.T) {
	eng := newMapProjPersonEngine(t)
	m := runOneMap(t, eng, "MATCH (n:Person) RETURN n{.*} AS m", "m")

	if got := m["name"]; got != expr.StringValue("Alice") {
		t.Errorf("m.name = %v, want 'Alice'", got)
	}
	if got := m["age"]; got != expr.IntegerValue(30) {
		t.Errorf("m.age = %v, want 30", got)
	}
	// .* copies all stored properties; the node has exactly name and age.
	if len(m) != 2 {
		t.Errorf("m{.*} has %d keys (%v), want all node properties (name, age)", len(m), m)
	}
}

func TestMapProjection_LiteralEntry(t *testing.T) {
	eng := newMapProjPersonEngine(t)
	m := runOneMap(t, eng, "MATCH (n:Person) RETURN n{.name, extra: 1} AS m", "m")

	if got := m["name"]; got != expr.StringValue("Alice") {
		t.Errorf("m.name = %v, want 'Alice'", got)
	}
	if got := m["extra"]; got != expr.IntegerValue(1) {
		t.Errorf("m.extra = %v, want 1", got)
	}
	if len(m) != 2 {
		t.Errorf("map has %d keys (%v), want name+extra", len(m), m)
	}
}

func TestMapProjection_VariableSelector(t *testing.T) {
	eng := newMapProjPersonEngine(t)
	// A variable selector `n.age` is bound first via WITH so that `age` is a
	// plain variable in scope; the selector projects it under its own name.
	m := runOneMap(t, eng,
		"MATCH (n:Person) WITH n, n.age AS age RETURN n{.name, age} AS m", "m")

	if got := m["name"]; got != expr.StringValue("Alice") {
		t.Errorf("m.name = %v, want 'Alice'", got)
	}
	if got := m["age"]; got != expr.IntegerValue(30) {
		t.Errorf("m.age (variable selector) = %v, want 30", got)
	}
}

func TestMapProjection_MapVariableSubject(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	m := runOneMap(t, eng,
		"WITH {a: 1, b: 2, c: 3} AS src RETURN src{.a, .b} AS m", "m")

	if got := m["a"]; got != expr.IntegerValue(1) {
		t.Errorf("m.a = %v, want 1", got)
	}
	if got := m["b"]; got != expr.IntegerValue(2) {
		t.Errorf("m.b = %v, want 2", got)
	}
	// Only the selected keys are projected; c is dropped.
	if _, present := m["c"]; present {
		t.Errorf("m unexpectedly contains key c: %v", m)
	}
	if len(m) != 2 {
		t.Errorf("map has %d keys (%v), want a+b", len(m), m)
	}
}

func TestMapProjection_MissingPropertyYieldsNull(t *testing.T) {
	eng := newMapProjPersonEngine(t)
	// `missing` is not a stored property; openCypher map projection yields null
	// for an absent property selector.
	m := runOneMap(t, eng, "MATCH (n:Person) RETURN n{.name, .missing} AS m", "m")

	if got := m["name"]; got != expr.StringValue("Alice") {
		t.Errorf("m.name = %v, want 'Alice'", got)
	}
	got, present := m["missing"]
	if !present {
		t.Fatalf("m has no key 'missing'; expected it present with a null value: %v", m)
	}
	if !expr.IsNull(got) {
		t.Errorf("m.missing = %v, want null", got)
	}
}
