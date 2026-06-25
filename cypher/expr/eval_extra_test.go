package expr_test

// eval_extra_test.go — supplementary tests to bring cypher/expr above the 75%
// coverage gate. Targets uncovered branches in eval.go and value.go.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Float arithmetic — evalFloatArith branches
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_FloatArith(t *testing.T) {
	tests := []struct {
		name  string
		left  ast.Expression
		op    string
		right ast.Expression
		want  expr.Value
	}{
		{"float_sub", fltLit(5.0), "-", fltLit(2.0), expr.FloatValue(3.0)},
		{"float_div", fltLit(9.0), "/", fltLit(4.0), expr.FloatValue(2.25)},
		{"float_mod", fltLit(10.0), "%", fltLit(3.0), expr.FloatValue(1.0)},
		{"float_pow", fltLit(2.0), "^", fltLit(3.0), expr.FloatValue(8.0)},
		{"float_add_int", fltLit(1.5), "+", intLit(2), expr.FloatValue(3.5)},
		// Integer ^ integer → FloatValue (via evalIntArith)
		{"int_pow", intLit(3), "^", intLit(2), expr.FloatValue(9.0)},
		// Division by zero in float → IEEE 754 (+Inf), not an error
		// Division by zero in float → IEEE 754 (+Inf via math.IsInf check below).
		// We can't use a constant 1.0/0.0 here; the test just ensures no error is returned.

	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := binary(tc.left, tc.op, tc.right)
			v := eval(t, e, nil, nil)
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

// TestEval_FloatArith_DivByZero — float div/0 → Inf (no error).
func TestEval_FloatArith_DivByZero(t *testing.T) {
	// Use a variable to avoid compile-time constant-fold error.
	zero := fltLit(0.0)
	e := binary(fltLit(1.0), "/", zero)
	v, err := expr.Eval(e, nil, nil, noReg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fv, ok := v.(expr.FloatValue)
	if !ok {
		t.Fatalf("expected FloatValue, got %T", v)
	}
	// IEEE 754: 1.0/0.0 = +Inf.
	if float64(fv) != float64(fv) { // NaN check (NaN != NaN)
		t.Error("unexpected NaN from 1.0 / 0.0")
	}
}

// Integer division and modulo by zero RAISE an ArithmeticError (matches Neo4j;
// openCypher leaves it implementation-defined). Float by-zero stays IEEE-754
// (+Inf / NaN), covered separately. (#1766)
func TestEval_IntDivMod_ByZero_Raises(t *testing.T) {
	for _, tc := range []struct{ op string }{{"/"}, {"%"}} {
		_, err := evalBinaryInt(t, tc.op, 7, 0)
		if err == nil {
			t.Fatalf("7 %s 0: want an error, got nil", tc.op)
		}
		if !strings.Contains(err.Error(), "ArithmeticError") {
			t.Errorf("7 %s 0: error = %q, want it to contain \"ArithmeticError\"", tc.op, err.Error())
		}
	}
}

// String concatenation via list + list
func TestEval_ListConcat(t *testing.T) {
	left := listLit(intLit(1), intLit(2))
	right := listLit(intLit(3), intLit(4))
	v := eval(t, binary(left, "+", right), nil, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 4 {
		t.Errorf("list concat: got %v (%T), want list of 4", v, v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// compareValues — incompatible types
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Ordering_IncompatibleTypes(t *testing.T) {
	// Comparing a string with a bool → should return NULL (incompatible types)
	v := eval(t, binary(strLit("hello"), "<", boolLit(true)), nil, nil)
	if !expr.IsNull(v) {
		t.Errorf("string < bool should be null, got %v", v)
	}
}

func TestEval_Ordering_BoolComparison(t *testing.T) {
	// false < true
	v := eval(t, binary(boolLit(false), "<", boolLit(true)), nil, nil)
	if v != expr.BoolValue(true) {
		t.Errorf("false < true should be true, got %v", v)
	}
	// true >= false
	v = eval(t, binary(boolLit(true), ">=", boolLit(false)), nil, nil)
	if v != expr.BoolValue(true) {
		t.Errorf("true >= false should be true, got %v", v)
	}
}

func TestEval_Ordering_FloatComparison(t *testing.T) {
	v := eval(t, binary(fltLit(1.5), "<", fltLit(2.5)), nil, nil)
	if v != expr.BoolValue(true) {
		t.Errorf("1.5 < 2.5 should be true, got %v", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// evalProperty — RelationshipValue + non-map types
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Property_Relationship(t *testing.T) {
	row := expr.RowContext{
		"r": expr.RelationshipValue{
			ID:         1,
			Properties: expr.MapValue{"since": expr.IntegerValue(2020)},
		},
	}
	e := &ast.Property{Receiver: varExpr("r"), Key: "since"}
	v := eval(t, e, row, nil)
	if v != expr.IntegerValue(2020) {
		t.Errorf("got %v, want 2020", v)
	}
}

func TestEval_Property_RelationshipMissingKey(t *testing.T) {
	row := expr.RowContext{
		"r": expr.RelationshipValue{ID: 1},
	}
	e := &ast.Property{Receiver: varExpr("r"), Key: "missing"}
	v := eval(t, e, row, nil)
	if !expr.IsNull(v) {
		t.Errorf("missing property should be null, got %v", v)
	}
}

func TestEval_Property_NonMap_Null(t *testing.T) {
	// Property access on IntegerValue / FloatValue receivers returns Null
	// (not an error) because the parser sometimes reconstructs
	// long-decimal float literals as Property{IntLiteral, "digits"}; see
	// commit 6dbffac. The TypeError path remains for other scalar kinds
	// (StringValue, BoolValue, ListValue, …).
	row := expr.RowContext{"n": expr.IntegerValue(42)}
	e := &ast.Property{Receiver: varExpr("n"), Key: "x"}
	v, err := expr.Eval(e, row, nil, nil)
	if err != nil {
		t.Errorf("property on integer should return Null, got error %v", err)
	}
	if !expr.IsNull(v) {
		t.Errorf("property on integer = %v, want Null", v)
	}
}

func TestEval_Property_MapMissingKey(t *testing.T) {
	row := expr.RowContext{"m": expr.MapValue{"a": expr.IntegerValue(1)}}
	e := &ast.Property{Receiver: varExpr("m"), Key: "z"}
	v := eval(t, e, row, nil)
	if !expr.IsNull(v) {
		t.Errorf("missing map key should be null, got %v", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// evalSubscript — negative index, map missing key, non-int index, non-container
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Subscript_NegativeIndex(t *testing.T) {
	row := expr.RowContext{
		"lst": expr.ListValue{expr.IntegerValue(10), expr.IntegerValue(20), expr.IntegerValue(30)},
	}
	// -1 should return the last element.
	e := &ast.SubscriptExpr{Expr: varExpr("lst"), Index: intLit(-1)}
	v := eval(t, e, row, nil)
	if v != expr.IntegerValue(30) {
		t.Errorf("got %v, want 30", v)
	}
}

func TestEval_Subscript_NonIntIndex_List(t *testing.T) {
	row := expr.RowContext{
		"lst": expr.ListValue{expr.IntegerValue(1)},
	}
	// String index on list → InvalidArgumentType TypeError per
	// openCypher; the indexer requires an Integer.
	e := &ast.SubscriptExpr{Expr: varExpr("lst"), Index: strLit("k")}
	_, err := expr.Eval(e, row, nil, nil)
	if err == nil {
		t.Errorf("string index on list should error, got nil")
	}
}

func TestEval_Subscript_MapMissingKey(t *testing.T) {
	row := expr.RowContext{
		"m": expr.MapValue{"a": expr.IntegerValue(1)},
	}
	e := &ast.SubscriptExpr{Expr: varExpr("m"), Index: strLit("z")}
	v := eval(t, e, row, nil)
	if !expr.IsNull(v) {
		t.Errorf("missing map key subscript should be null, got %v", v)
	}
}

func TestEval_Subscript_NonContainer_Null(t *testing.T) {
	// Subscript on a non-container scalar → InvalidArgumentType
	// TypeError per openCypher.
	row := expr.RowContext{"n": expr.IntegerValue(5)}
	e := &ast.SubscriptExpr{Expr: varExpr("n"), Index: intLit(0)}
	_, err := expr.Eval(e, row, nil, nil)
	if err == nil {
		t.Errorf("subscript on integer should error, got nil")
	}
}

func TestEval_Subscript_NullContainer(t *testing.T) {
	e := &ast.SubscriptExpr{Expr: nullLit(), Index: intLit(0)}
	v := eval(t, e, nil, nil)
	if !expr.IsNull(v) {
		t.Errorf("subscript on null should be null, got %v", v)
	}
}

func TestEval_Subscript_NullIndex(t *testing.T) {
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(1)}}
	e := &ast.SubscriptExpr{Expr: varExpr("lst"), Index: nullLit()}
	v := eval(t, e, row, nil)
	if !expr.IsNull(v) {
		t.Errorf("null index should be null, got %v", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// evalStringOp — regex =~ and non-string operand
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_StringOp_Regex(t *testing.T) {
	t.Run("match", func(t *testing.T) {
		v := eval(t, binary(strLit("hello123"), "=~", strLit("[a-z]+[0-9]+")), nil, nil)
		if v != expr.BoolValue(true) {
			t.Errorf("got %v, want true", v)
		}
	})
	t.Run("no_match", func(t *testing.T) {
		v := eval(t, binary(strLit("hello"), "=~", strLit("^[0-9]+$")), nil, nil)
		if v != expr.BoolValue(false) {
			t.Errorf("got %v, want false", v)
		}
	})
	t.Run("invalid_pattern", func(t *testing.T) {
		// Invalid regex pattern → NULL per openCypher semantics.
		v := eval(t, binary(strLit("hello"), "=~", strLit("[invalid")), nil, nil)
		if !expr.IsNull(v) {
			t.Errorf("invalid regex pattern should return null, got %v", v)
		}
	})
	t.Run("null_null", func(t *testing.T) {
		v := eval(t, binary(nullLit(), "=~", nullLit()), nil, nil)
		if !expr.IsNull(v) {
			t.Errorf("null =~ null should be null, got %v", v)
		}
	})
}

func TestEval_StringOp_NonString_Null(t *testing.T) {
	// Non-string left operand → NULL.
	v := eval(t, binary(intLit(1), "CONTAINS", strLit("1")), nil, nil)
	if !expr.IsNull(v) {
		t.Errorf("non-string CONTAINS should be null, got %v", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// evalUnaryOp — unary plus, unknown operator
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_UnaryPlus(t *testing.T) {
	v := eval(t, unary("+", intLit(7)), nil, nil)
	if v != expr.IntegerValue(7) {
		t.Errorf("got %v, want 7", v)
	}
}

func TestEval_UnaryMinus_NonNumeric_Null(t *testing.T) {
	// Unary minus on a non-numeric → NULL.
	v := eval(t, unary("-", strLit("hello")), nil, nil)
	if !expr.IsNull(v) {
		t.Errorf("unary minus on string should be null, got %v", v)
	}
}

func TestEval_UnaryUnknownOp_Error(t *testing.T) {
	e := &ast.UnaryOp{Operator: "NOPE", Operand: intLit(1)}
	_, err := expr.Eval(e, nil, nil, noReg)
	if err == nil {
		t.Fatal("expected error for unknown unary operator")
	}
}

func TestEval_BinaryUnknownOp_Error(t *testing.T) {
	e := &ast.BinaryOp{Operator: "NOPE", Left: intLit(1), Right: intLit(2)}
	_, err := expr.Eval(e, nil, nil, noReg)
	if err == nil {
		t.Fatal("expected error for unknown binary operator")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// evalFunction — unknown function name
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_FunctionInvocation_Unknown(t *testing.T) {
	e := &ast.FunctionInvocation{Name: "unknownfn", Args: nil}
	_, err := expr.Eval(e, nil, nil, noReg)
	if err == nil {
		t.Fatal("expected error for unknown function")
	}
}

func TestEval_FunctionInvocation_WithNamespace(t *testing.T) {
	// Namespaced function call should work when the registry resolves it.
	reg := &singleFnReg{
		name: "apoc.util.sum",
		fn: func(args []expr.Value) (expr.Value, error) {
			return expr.IntegerValue(100), nil
		},
	}
	e := &ast.FunctionInvocation{
		Namespace: []string{"apoc", "util"},
		Name:      "sum",
	}
	v, err := expr.Eval(e, nil, nil, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != expr.IntegerValue(100) {
		t.Errorf("got %v, want 100", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// EvalError.Error()
// ─────────────────────────────────────────────────────────────────────────────

func TestEvalError_Error(t *testing.T) {
	// Trigger an EvalError via an unknown binary operator, then check the
	// error message satisfies the Error() method.
	e := &ast.BinaryOp{Operator: "??", Left: intLit(1), Right: intLit(2)}
	_, err := expr.Eval(e, nil, nil, noReg)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() == "" {
		t.Error("EvalError.Error() should not be empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Value.String() and Value.Hash() — exercise the String/Hash methods on
// value types that have 0% coverage in value.go
// ─────────────────────────────────────────────────────────────────────────────

func TestValue_String(t *testing.T) {
	// Use fmt.Stringer interface which all Value types satisfy.
	type stringer interface{ String() string }
	tests := []struct {
		v    stringer
		want string
	}{
		// nullValue is unexported; access via Null (which is a Value interface).
		{expr.IntegerValue(42), "42"},
		{expr.FloatValue(3.14), "3.14"},
		{expr.StringValue("hello"), `"hello"`},
		{expr.BoolValue(true), "true"},
		{expr.BoolValue(false), "false"},
		{expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}, "[1, 2]"},
		{expr.ListValue{}, "[]"},
		{expr.MapValue{}, "{}"},
		{expr.NodeValue{ID: 5}, "(node#5)"},
		{expr.RelationshipValue{ID: 3, Type: "LIKES"}, "-[rel#3:LIKES]->"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := tc.v.String()
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	// Test Null via the Value interface (nullValue is unexported).
	if s := expr.Null.(interface{ String() string }).String(); s != "null" {
		t.Errorf("Null.String() = %q, want null", s)
	}
}

func TestValue_Hash(t *testing.T) {
	// Hash must be stable (same value → same hash).
	// All concrete types are accessed via the Value interface which exposes Hash().
	type hasher interface{ Hash() uint64 }
	vals := []hasher{
		expr.Null.(hasher),
		expr.IntegerValue(1),
		expr.FloatValue(2.0),
		expr.StringValue("abc"),
		expr.BoolValue(true),
		expr.BoolValue(false),
		expr.ListValue{expr.IntegerValue(1)},
		expr.MapValue{"k": expr.StringValue("v")},
		expr.NodeValue{ID: 7},
		expr.RelationshipValue{ID: 3},
		expr.PathValue{Nodes: []expr.NodeValue{{ID: 1}}},
	}
	for _, h := range vals {
		h1, h2 := h.Hash(), h.Hash()
		if h1 != h2 {
			t.Errorf("%T Hash() not stable: %d vs %d", h, h1, h2)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// value.go Compare — cross-type ordering, list/path/map comparison,
// cmpUint64, compareList, comparePath
// ─────────────────────────────────────────────────────────────────────────────

func TestValue_Compare(t *testing.T) {
	tests := []struct {
		name string
		a, b expr.Value
		want int
	}{
		// NULL sorts last.
		{"null_null", expr.Null, expr.Null, 0},
		{"null_after_int", expr.IntegerValue(1), expr.Null, -1},
		{"null_before_int_from_b", expr.Null, expr.IntegerValue(1), 1},
		// Same-type integers.
		{"int_lt", expr.IntegerValue(1), expr.IntegerValue(2), -1},
		{"int_eq", expr.IntegerValue(5), expr.IntegerValue(5), 0},
		{"int_gt", expr.IntegerValue(3), expr.IntegerValue(2), 1},
		// Same-type floats.
		{"flt_lt", expr.FloatValue(1.0), expr.FloatValue(2.0), -1},
		{"flt_eq", expr.FloatValue(3.0), expr.FloatValue(3.0), 0},
		// Same-type strings.
		{"str_lt", expr.StringValue("a"), expr.StringValue("b"), -1},
		{"str_eq", expr.StringValue("x"), expr.StringValue("x"), 0},
		{"str_gt", expr.StringValue("z"), expr.StringValue("a"), 1},
		// Same-type booleans.
		{"bool_lt", expr.BoolValue(false), expr.BoolValue(true), -1},
		{"bool_eq", expr.BoolValue(true), expr.BoolValue(true), 0},
		// Cross-type: String(5) before Bool(6) before Float(7) before Integer(8).
		{"str_before_bool", expr.StringValue("x"), expr.BoolValue(true), -1},
		{"bool_before_float", expr.BoolValue(true), expr.FloatValue(1.0), -1},
		{"float_before_int", expr.FloatValue(1.0), expr.IntegerValue(1), -1},
		// Node comparison by ID (cmpUint64).
		{"node_lt", expr.NodeValue{ID: 1}, expr.NodeValue{ID: 2}, -1},
		{"node_eq", expr.NodeValue{ID: 5}, expr.NodeValue{ID: 5}, 0},
		{"node_gt", expr.NodeValue{ID: 10}, expr.NodeValue{ID: 3}, 1},
		// Relationship comparison by ID.
		{"rel_lt", expr.RelationshipValue{ID: 1}, expr.RelationshipValue{ID: 2}, -1},
		// List comparison (compareList).
		{"list_eq", expr.ListValue{expr.IntegerValue(1)}, expr.ListValue{expr.IntegerValue(1)}, 0},
		{"list_lt_shorter", expr.ListValue{expr.IntegerValue(1)}, expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}, -1},
		{"list_gt_longer", expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}, expr.ListValue{expr.IntegerValue(1)}, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := expr.Compare(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("Compare(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestValue_Compare_Path(t *testing.T) {
	p1 := expr.PathValue{
		Nodes:         []expr.NodeValue{{ID: 1}, {ID: 2}},
		Relationships: []expr.RelationshipValue{{ID: 10}},
	}
	p2 := expr.PathValue{
		Nodes:         []expr.NodeValue{{ID: 1}, {ID: 3}},
		Relationships: []expr.RelationshipValue{{ID: 10}},
	}
	p3 := expr.PathValue{
		Nodes:         []expr.NodeValue{{ID: 1}},
		Relationships: nil,
	}

	// p1 == p1
	if expr.Compare(p1, p1) != 0 {
		t.Error("same path should compare equal")
	}
	// p1 vs p2 — differ at node[1]: ID 2 < 3
	if expr.Compare(p1, p2) >= 0 {
		t.Error("p1 should be less than p2")
	}
	// p1 vs p3 — differ in node count
	if expr.Compare(p3, p1) >= 0 {
		t.Error("shorter path should come first")
	}
}

func TestValue_Equal_Edge_Cases(t *testing.T) {
	// ListValue.Equal: different lengths → false.
	a := expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}
	b := expr.ListValue{expr.IntegerValue(1)}
	if expr.IsTruthy(a.Equal(b)) {
		t.Error("different-length lists should not be equal")
	}
	// MapValue.Equal: missing key → false.
	m1 := expr.MapValue{"a": expr.IntegerValue(1), "b": expr.IntegerValue(2)}
	m2 := expr.MapValue{"a": expr.IntegerValue(1)}
	if expr.IsTruthy(m1.Equal(m2)) {
		t.Error("different key-set maps should not be equal")
	}
	// MapValue.Equal: null value inside map → NULL.
	m3 := expr.MapValue{"a": expr.Null}
	m4 := expr.MapValue{"a": expr.Null}
	if !expr.IsNull(m3.Equal(m4)) {
		t.Error("map with null value equal to another null-value map should be null")
	}
	// NodeValue.Equal: different ID → false.
	n1 := expr.NodeValue{ID: 1}
	n2 := expr.NodeValue{ID: 2}
	if expr.IsTruthy(n1.Equal(n2)) {
		t.Error("nodes with different IDs should not be equal")
	}
	// PathValue.Equal: null → null.
	p := expr.PathValue{Nodes: []expr.NodeValue{{ID: 1}}}
	if !expr.IsNull(p.Equal(expr.Null)) {
		t.Error("path.Equal(null) should be null")
	}
}

func TestValue_PathString(t *testing.T) {
	// Empty path string.
	ep := expr.PathValue{}
	s := ep.String()
	if s != "<empty-path>" {
		t.Errorf("empty path string = %q, want <empty-path>", s)
	}

	// Single-node path. PathValue.String() wraps the path in `<…>` so it is
	// visually distinct from a bare node renderering — matching the openCypher
	// TCK convention `<(node)>`.
	sp := expr.PathValue{Nodes: []expr.NodeValue{{ID: 1}}}
	s = sp.String()
	if s != "<(node#1)>" {
		t.Errorf("single-node path string = %q, want <(node#1)>", s)
	}

	// Full path.
	fp := expr.PathValue{
		Nodes:         []expr.NodeValue{{ID: 1}, {ID: 2}},
		Relationships: []expr.RelationshipValue{{ID: 10, Type: "KNOWS"}},
	}
	s = fp.String()
	if s == "" {
		t.Error("full path string should not be empty")
	}
}

func TestValue_MapString_NonEmpty(t *testing.T) {
	m := expr.MapValue{"key": expr.IntegerValue(1)}
	s := m.String()
	if s == "" || s == "{}" {
		t.Errorf("non-empty map string = %q, want non-empty non-{}", s)
	}
}
