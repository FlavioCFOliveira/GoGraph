package expr_test

// map_test.go — tests for map projection evaluation (task-265).
//
// 5+ scenarios per operation:
//   - Property selector .key
//   - Star selector .*
//   - Computed key: expr
//   - Null subject
//   - Node subject, Relationship subject, Map subject

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

func mapProj(subject ast.Expression, items ...*ast.MapProjectionItem) *ast.MapProjection {
	return &ast.MapProjection{Subject: subject, Items: items}
}

func propSelector(key string) *ast.MapProjectionItem {
	return &ast.MapProjectionItem{Key: key} // Value == nil, IsAll == false
}

func allSelector() *ast.MapProjectionItem {
	return &ast.MapProjectionItem{IsAll: true}
}

func computedEntry(key string, value ast.Expression) *ast.MapProjectionItem {
	return &ast.MapProjectionItem{Key: key, Value: value}
}

func varEntry(name string) *ast.MapProjectionItem {
	return &ast.MapProjectionItem{Value: varExpr(name)} // key derived from variable name
}

// ─────────────────────────────────────────────────────────────────────────────
// Property selector .key
// ─────────────────────────────────────────────────────────────────────────────

func TestMapProj_PropSelector_Node(t *testing.T) {
	row := expr.RowContext{
		"n": expr.NodeValue{
			ID:         1,
			Properties: expr.MapValue{"name": expr.StringValue("Alice"), "age": expr.IntegerValue(30)},
		},
	}
	e := mapProj(varExpr("n"), propSelector("name"), propSelector("age"))
	v := eval(t, e, row, nil)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if mv["name"] != expr.StringValue("Alice") {
		t.Errorf("name = %v, want Alice", mv["name"])
	}
	if mv["age"] != expr.IntegerValue(30) {
		t.Errorf("age = %v, want 30", mv["age"])
	}
}

func TestMapProj_PropSelector_MissingKey(t *testing.T) {
	row := expr.RowContext{
		"n": expr.NodeValue{
			ID:         1,
			Properties: expr.MapValue{"name": expr.StringValue("Alice")},
		},
	}
	e := mapProj(varExpr("n"), propSelector("missing"))
	v := eval(t, e, row, nil)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if !expr.IsNull(mv["missing"]) {
		t.Errorf("missing key should be null, got %v", mv["missing"])
	}
}

func TestMapProj_PropSelector_MapSubject(t *testing.T) {
	row := expr.RowContext{
		"m": expr.MapValue{"x": expr.IntegerValue(10), "y": expr.IntegerValue(20)},
	}
	e := mapProj(varExpr("m"), propSelector("x"))
	v := eval(t, e, row, nil)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if mv["x"] != expr.IntegerValue(10) {
		t.Errorf("x = %v, want 10", mv["x"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Star selector .*
// ─────────────────────────────────────────────────────────────────────────────

func TestMapProj_StarSelector_ClonesAll(t *testing.T) {
	row := expr.RowContext{
		"n": expr.NodeValue{
			ID: 1,
			Properties: expr.MapValue{
				"a": expr.IntegerValue(1),
				"b": expr.IntegerValue(2),
			},
		},
	}
	e := mapProj(varExpr("n"), allSelector())
	v := eval(t, e, row, nil)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if len(mv) != 2 {
		t.Errorf("want 2 entries, got %d: %v", len(mv), mv)
	}
	if mv["a"] != expr.IntegerValue(1) || mv["b"] != expr.IntegerValue(2) {
		t.Errorf("got %v, want {a:1,b:2}", mv)
	}
}

func TestMapProj_StarSelector_RelSubject(t *testing.T) {
	row := expr.RowContext{
		"r": expr.RelationshipValue{
			ID:         1,
			Type:       "KNOWS",
			Properties: expr.MapValue{"since": expr.IntegerValue(2020)},
		},
	}
	e := mapProj(varExpr("r"), allSelector())
	v := eval(t, e, row, nil)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if mv["since"] != expr.IntegerValue(2020) {
		t.Errorf("since = %v, want 2020", mv["since"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Computed projection key: expr
// ─────────────────────────────────────────────────────────────────────────────

func TestMapProj_ComputedEntry(t *testing.T) {
	row := expr.RowContext{"n": expr.NodeValue{ID: 1, Properties: expr.MapValue{"name": expr.StringValue("Bob")}}}
	e := mapProj(varExpr("n"),
		propSelector("name"),
		computedEntry("extra", intLit(99)),
	)
	v := eval(t, e, row, nil)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if mv["name"] != expr.StringValue("Bob") {
		t.Errorf("name = %v, want Bob", mv["name"])
	}
	if mv["extra"] != expr.IntegerValue(99) {
		t.Errorf("extra = %v, want 99", mv["extra"])
	}
}

func TestMapProj_VarEntry(t *testing.T) {
	// n{age} where age is a bound variable → {age: <value>}
	row := expr.RowContext{
		"n":   expr.NodeValue{ID: 1},
		"age": expr.IntegerValue(42),
	}
	e := mapProj(varExpr("n"), varEntry("age"))
	v := eval(t, e, row, nil)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if mv["age"] != expr.IntegerValue(42) {
		t.Errorf("age = %v, want 42", mv["age"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NULL subject
// ─────────────────────────────────────────────────────────────────────────────

func TestMapProj_NullSubject(t *testing.T) {
	e := mapProj(nullLit(), propSelector("x"))
	v := eval(t, e, nil, nil)
	if !expr.IsNull(v) {
		t.Errorf("null subject should return null, got %v", v)
	}
}
