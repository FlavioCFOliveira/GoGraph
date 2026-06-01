package expr_test

// eval_test.go — tests for the Cypher expression evaluator (task-247).
//
// Coverage requirements: 3VL truth tables, NULL propagation, arithmetic,
// comparisons, CASE, functions, property access, IS NULL / IS NOT NULL.
// 30+ scenarios as per the acceptance criteria.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// nullReg is a no-op FunctionRegistry that always returns not-found.
type nullReg struct{}

func (nullReg) Resolve(_ string) (expr.BuiltinFn, bool) { return nil, false }

var noReg expr.FunctionRegistry = nullReg{}

// pos is a zero Position for AST literals.
var pos = ast.Position{}

func intLit(v int64) *ast.IntLiteral       { return &ast.IntLiteral{Pos: pos, Value: v} }
func fltLit(v float64) *ast.FloatLiteral   { return &ast.FloatLiteral{Pos: pos, Value: v} }
func strLit(v string) *ast.StringLiteral   { return &ast.StringLiteral{Pos: pos, Value: v} }
func boolLit(v bool) *ast.BoolLiteral      { return &ast.BoolLiteral{Pos: pos, Value: v} }
func nullLit() *ast.NullLiteral            { return &ast.NullLiteral{Pos: pos} }
func varExpr(name string) *ast.Variable    { return &ast.Variable{Pos: pos, Name: name} }
func paramExpr(name string) *ast.Parameter { return &ast.Parameter{Pos: pos, Name: name} }
func unary(op string, operand ast.Expression) *ast.UnaryOp {
	return &ast.UnaryOp{Pos: pos, Operator: op, Operand: operand}
}
func binary(left ast.Expression, op string, right ast.Expression) *ast.BinaryOp {
	return &ast.BinaryOp{Pos: pos, Left: left, Operator: op, Right: right}
}
func listLit(elems ...ast.Expression) *ast.ListLiteral {
	return &ast.ListLiteral{Pos: pos, Elements: elems}
}

func eval(t *testing.T, e ast.Expression, row expr.RowContext, params map[string]expr.Value) expr.Value {
	t.Helper()
	v, err := expr.Eval(e, row, params, noReg)
	if err != nil {
		t.Fatalf("Eval error: %v", err)
	}
	return v
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Literals
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Literals(t *testing.T) {
	t.Run("null", func(t *testing.T) {
		v := eval(t, nullLit(), nil, nil)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null", v)
		}
	})
	t.Run("bool_true", func(t *testing.T) {
		v := eval(t, boolLit(true), nil, nil)
		if !expr.IsTruthy(v) {
			t.Errorf("got %v, want true", v)
		}
	})
	t.Run("bool_false", func(t *testing.T) {
		v := eval(t, boolLit(false), nil, nil)
		if expr.IsTruthy(v) || expr.IsNull(v) {
			t.Errorf("got %v, want false", v)
		}
	})
	t.Run("int", func(t *testing.T) {
		v := eval(t, intLit(42), nil, nil)
		if v != expr.IntegerValue(42) {
			t.Errorf("got %v, want 42", v)
		}
	})
	t.Run("float", func(t *testing.T) {
		v := eval(t, fltLit(3.14), nil, nil)
		if v != expr.FloatValue(3.14) {
			t.Errorf("got %v, want 3.14", v)
		}
	})
	t.Run("string", func(t *testing.T) {
		v := eval(t, strLit("hello"), nil, nil)
		if v != expr.StringValue("hello") {
			t.Errorf("got %v, want hello", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Variable and parameter lookup
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Variable(t *testing.T) {
	row := expr.RowContext{"x": expr.IntegerValue(7)}
	t.Run("bound", func(t *testing.T) {
		v := eval(t, varExpr("x"), row, nil)
		if v != expr.IntegerValue(7) {
			t.Errorf("got %v, want 7", v)
		}
	})
	t.Run("unbound_returns_null", func(t *testing.T) {
		v := eval(t, varExpr("y"), row, nil)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null for unbound variable", v)
		}
	})
}

func TestEval_Parameter(t *testing.T) {
	params := map[string]expr.Value{"p": expr.StringValue("world")}
	t.Run("set", func(t *testing.T) {
		v := eval(t, paramExpr("p"), nil, params)
		if v != expr.StringValue("world") {
			t.Errorf("got %v, want world", v)
		}
	})
	t.Run("unset_returns_null", func(t *testing.T) {
		v := eval(t, paramExpr("q"), nil, params)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null for unset param", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Equality — 3VL
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Equality_3VL(t *testing.T) {
	tests := []struct {
		name  string
		left  ast.Expression
		right ast.Expression
		want  expr.Value
	}{
		// int = int
		{"int_eq", intLit(1), intLit(1), expr.BoolValue(true)},
		{"int_neq", intLit(1), intLit(2), expr.BoolValue(false)},
		// null = null → null
		{"null_eq_null", nullLit(), nullLit(), expr.Null},
		// null = value → null
		{"null_eq_int", nullLit(), intLit(1), expr.Null},
		{"int_eq_null", intLit(1), nullLit(), expr.Null},
		// string equality
		{"str_eq", strLit("a"), strLit("a"), expr.BoolValue(true)},
		{"str_neq", strLit("a"), strLit("b"), expr.BoolValue(false)},
		// bool equality
		{"bool_eq", boolLit(true), boolLit(true), expr.BoolValue(true)},
		{"bool_neq", boolLit(true), boolLit(false), expr.BoolValue(false)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := binary(tc.left, "=", tc.right)
			v := eval(t, e, nil, nil)
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Inequality
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Inequality(t *testing.T) {
	tests := []struct {
		name  string
		left  ast.Expression
		right ast.Expression
		want  expr.Value
	}{
		{"int_neq_true", intLit(1), intLit(2), expr.BoolValue(true)},
		{"int_neq_false", intLit(1), intLit(1), expr.BoolValue(false)},
		{"null_neq_null", nullLit(), nullLit(), expr.Null},
		{"null_neq_int", nullLit(), intLit(1), expr.Null},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := binary(tc.left, "<>", tc.right)
			v := eval(t, e, nil, nil)
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Ordering comparisons with 3VL
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Ordering(t *testing.T) {
	tests := []struct {
		name  string
		left  ast.Expression
		op    string
		right ast.Expression
		want  expr.Value
	}{
		{"int_lt_true", intLit(1), "<", intLit(2), expr.BoolValue(true)},
		{"int_lt_false", intLit(2), "<", intLit(1), expr.BoolValue(false)},
		{"int_le_equal", intLit(1), "<=", intLit(1), expr.BoolValue(true)},
		{"int_gt", intLit(3), ">", intLit(2), expr.BoolValue(true)},
		{"int_ge", intLit(2), ">=", intLit(2), expr.BoolValue(true)},
		{"null_lt", nullLit(), "<", intLit(1), expr.Null},
		{"int_lt_null", intLit(1), "<", nullLit(), expr.Null},
		{"str_lt", strLit("a"), "<", strLit("b"), expr.BoolValue(true)},
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

// ─────────────────────────────────────────────────────────────────────────────
// 6. Arithmetic
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Arithmetic(t *testing.T) {
	tests := []struct {
		name  string
		left  ast.Expression
		op    string
		right ast.Expression
		want  expr.Value
	}{
		{"int_add", intLit(3), "+", intLit(4), expr.IntegerValue(7)},
		{"int_sub", intLit(10), "-", intLit(3), expr.IntegerValue(7)},
		{"int_mul", intLit(3), "*", intLit(4), expr.IntegerValue(12)},
		{"int_div", intLit(10), "/", intLit(3), expr.IntegerValue(3)},
		{"int_mod", intLit(10), "%", intLit(3), expr.IntegerValue(1)},
		// Float arithmetic
		{"float_add", fltLit(1.5), "+", fltLit(2.5), expr.FloatValue(4.0)},
		{"float_mul", fltLit(2.0), "*", fltLit(3.0), expr.FloatValue(6.0)},
		// Mixed Int+Float → Float
		{"int_plus_float", intLit(1), "+", fltLit(2.5), expr.FloatValue(3.5)},
		{"float_plus_int", fltLit(1.5), "+", intLit(2), expr.FloatValue(3.5)},
		// String concat
		{"str_concat", strLit("hello"), "+", strLit(" world"), expr.StringValue("hello world")},
		// NULL propagation
		{"null_add_int", nullLit(), "+", intLit(1), expr.Null},
		{"int_add_null", intLit(1), "+", nullLit(), expr.Null},
		// Division by zero → NULL
		{"div_zero", intLit(5), "/", intLit(0), expr.Null},
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

// ─────────────────────────────────────────────────────────────────────────────
// 7. 3VL AND truth table
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_AND_3VL(t *testing.T) {
	tests := []struct {
		left  ast.Expression
		right ast.Expression
		want  expr.Value
	}{
		{boolLit(true), boolLit(true), expr.BoolValue(true)},
		{boolLit(true), boolLit(false), expr.BoolValue(false)},
		{boolLit(false), boolLit(true), expr.BoolValue(false)},
		{boolLit(false), boolLit(false), expr.BoolValue(false)},
		{boolLit(false), nullLit(), expr.BoolValue(false)}, // false AND null = false
		{nullLit(), boolLit(false), expr.BoolValue(false)}, // null AND false = false
		{boolLit(true), nullLit(), expr.Null},              // true AND null = null
		{nullLit(), boolLit(true), expr.Null},              // null AND true = null
		{nullLit(), nullLit(), expr.Null},                  // null AND null = null
	}
	for _, tc := range tests {
		t.Run(tc.left.String()+"_AND_"+tc.right.String(), func(t *testing.T) {
			e := binary(tc.left, "AND", tc.right)
			v := eval(t, e, nil, nil)
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. 3VL OR truth table
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_OR_3VL(t *testing.T) {
	tests := []struct {
		left  ast.Expression
		right ast.Expression
		want  expr.Value
	}{
		{boolLit(true), boolLit(true), expr.BoolValue(true)},
		{boolLit(true), boolLit(false), expr.BoolValue(true)},
		{boolLit(false), boolLit(true), expr.BoolValue(true)},
		{boolLit(false), boolLit(false), expr.BoolValue(false)},
		{boolLit(true), nullLit(), expr.BoolValue(true)}, // true OR null = true
		{nullLit(), boolLit(true), expr.BoolValue(true)}, // null OR true = true
		{boolLit(false), nullLit(), expr.Null},           // false OR null = null
		{nullLit(), boolLit(false), expr.Null},           // null OR false = null
		{nullLit(), nullLit(), expr.Null},                // null OR null = null
	}
	for _, tc := range tests {
		t.Run(tc.left.String()+"_OR_"+tc.right.String(), func(t *testing.T) {
			e := binary(tc.left, "OR", tc.right)
			v := eval(t, e, nil, nil)
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. NOT with 3VL
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_NOT_3VL(t *testing.T) {
	tests := []struct {
		operand ast.Expression
		want    expr.Value
	}{
		{boolLit(true), expr.BoolValue(false)},
		{boolLit(false), expr.BoolValue(true)},
		{nullLit(), expr.Null}, // NOT null = null
	}
	for _, tc := range tests {
		t.Run(tc.operand.String(), func(t *testing.T) {
			e := unary("NOT", tc.operand)
			v := eval(t, e, nil, nil)
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 10. IS NULL / IS NOT NULL
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_IsNull(t *testing.T) {
	t.Run("null_is_null", func(t *testing.T) {
		e := unary("IS NULL", nullLit())
		v := eval(t, e, nil, nil)
		if v != expr.BoolValue(true) {
			t.Errorf("got %v, want true", v)
		}
	})
	t.Run("int_is_null_false", func(t *testing.T) {
		e := unary("IS NULL", intLit(1))
		v := eval(t, e, nil, nil)
		if v != expr.BoolValue(false) {
			t.Errorf("got %v, want false", v)
		}
	})
	t.Run("null_is_not_null", func(t *testing.T) {
		e := unary("IS NOT NULL", nullLit())
		v := eval(t, e, nil, nil)
		if v != expr.BoolValue(false) {
			t.Errorf("got %v, want false", v)
		}
	})
	t.Run("int_is_not_null_true", func(t *testing.T) {
		e := unary("IS NOT NULL", intLit(1))
		v := eval(t, e, nil, nil)
		if v != expr.BoolValue(true) {
			t.Errorf("got %v, want true", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 11. Unary minus
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_UnaryMinus(t *testing.T) {
	t.Run("int", func(t *testing.T) {
		v := eval(t, unary("-", intLit(5)), nil, nil)
		if v != expr.IntegerValue(-5) {
			t.Errorf("got %v, want -5", v)
		}
	})
	t.Run("float", func(t *testing.T) {
		v := eval(t, unary("-", fltLit(3.14)), nil, nil)
		if v != expr.FloatValue(-3.14) {
			t.Errorf("got %v, want -3.14", v)
		}
	})
	t.Run("null", func(t *testing.T) {
		v := eval(t, unary("-", nullLit()), nil, nil)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 12. CASE expression — generic and value forms
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Case(t *testing.T) {
	t.Run("generic_match_first", func(t *testing.T) {
		row := expr.RowContext{"x": expr.IntegerValue(2)}
		e := &ast.CaseExpression{
			Alternatives: []*ast.CaseAlternative{
				{Condition: binary(varExpr("x"), "=", intLit(1)), Consequent: strLit("one")},
				{Condition: binary(varExpr("x"), "=", intLit(2)), Consequent: strLit("two")},
			},
			ElseExpr: strLit("other"),
		}
		v := eval(t, e, row, nil)
		if v != expr.StringValue("two") {
			t.Errorf("got %v, want two", v)
		}
	})

	t.Run("generic_else", func(t *testing.T) {
		row := expr.RowContext{"x": expr.IntegerValue(99)}
		e := &ast.CaseExpression{
			Alternatives: []*ast.CaseAlternative{
				{Condition: binary(varExpr("x"), "=", intLit(1)), Consequent: strLit("one")},
			},
			ElseExpr: strLit("other"),
		}
		v := eval(t, e, row, nil)
		if v != expr.StringValue("other") {
			t.Errorf("got %v, want other", v)
		}
	})

	t.Run("no_match_no_else_null", func(t *testing.T) {
		e := &ast.CaseExpression{
			Alternatives: []*ast.CaseAlternative{
				{Condition: boolLit(false), Consequent: strLit("never")},
			},
		}
		v := eval(t, e, nil, nil)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null", v)
		}
	})

	t.Run("value_form", func(t *testing.T) {
		row := expr.RowContext{"s": expr.StringValue("b")}
		e := &ast.CaseExpression{
			Subject: varExpr("s"),
			Alternatives: []*ast.CaseAlternative{
				{Condition: strLit("a"), Consequent: intLit(1)},
				{Condition: strLit("b"), Consequent: intLit(2)},
			},
			ElseExpr: intLit(0),
		}
		v := eval(t, e, row, nil)
		if v != expr.IntegerValue(2) {
			t.Errorf("got %v, want 2", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 13. List literal
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_ListLiteral(t *testing.T) {
	e := listLit(intLit(1), intLit(2), intLit(3))
	v := eval(t, e, nil, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("got %v (%T), want list of 3", v, v)
	}
	for i, want := range []expr.Value{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)} {
		if lv[i] != want {
			t.Errorf("[%d] got %v, want %v", i, lv[i], want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 14. Map literal
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_MapLiteral(t *testing.T) {
	e := &ast.MapLiteral{
		Keys:   []string{"a", "b"},
		Values: []ast.Expression{intLit(1), strLit("x")},
	}
	v := eval(t, e, nil, nil)
	mv, ok := v.(expr.MapValue)
	if !ok {
		t.Fatalf("got %T, want MapValue", v)
	}
	if mv["a"] != expr.IntegerValue(1) {
		t.Errorf("a = %v, want 1", mv["a"])
	}
	if mv["b"] != expr.StringValue("x") {
		t.Errorf("b = %v, want x", mv["b"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 15. Property access
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Property(t *testing.T) {
	t.Run("node_property", func(t *testing.T) {
		row := expr.RowContext{
			"n": expr.NodeValue{
				ID:         1,
				Properties: expr.MapValue{"name": expr.StringValue("Alice")},
			},
		}
		e := &ast.Property{Receiver: varExpr("n"), Key: "name"}
		v := eval(t, e, row, nil)
		if v != expr.StringValue("Alice") {
			t.Errorf("got %v, want Alice", v)
		}
	})

	t.Run("map_property", func(t *testing.T) {
		row := expr.RowContext{
			"m": expr.MapValue{"x": expr.IntegerValue(42)},
		}
		e := &ast.Property{Receiver: varExpr("m"), Key: "x"}
		v := eval(t, e, row, nil)
		if v != expr.IntegerValue(42) {
			t.Errorf("got %v, want 42", v)
		}
	})

	t.Run("missing_key_null", func(t *testing.T) {
		row := expr.RowContext{
			"n": expr.NodeValue{ID: 1},
		}
		e := &ast.Property{Receiver: varExpr("n"), Key: "missing"}
		v := eval(t, e, row, nil)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null for missing property", v)
		}
	})

	t.Run("null_receiver", func(t *testing.T) {
		e := &ast.Property{Receiver: nullLit(), Key: "x"}
		v := eval(t, e, nil, nil)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 16. IN operator
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_IN(t *testing.T) {
	tests := []struct {
		name string
		val  ast.Expression
		list ast.Expression
		want expr.Value
	}{
		{
			"present",
			intLit(2),
			listLit(intLit(1), intLit(2), intLit(3)),
			expr.BoolValue(true),
		},
		{
			"absent",
			intLit(5),
			listLit(intLit(1), intLit(2), intLit(3)),
			expr.BoolValue(false),
		},
		{
			"null_val",
			nullLit(),
			listLit(intLit(1)),
			expr.Null,
		},
		{
			"null_list",
			intLit(1),
			nullLit(),
			expr.Null,
		},
		{
			"list_with_null_absent",
			intLit(5),
			listLit(intLit(1), nullLit(), intLit(3)),
			expr.Null, // 5 not found, but null in list → null
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := binary(tc.val, "IN", tc.list)
			v := eval(t, e, nil, nil)
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 17. String operators
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_StringOps(t *testing.T) {
	tests := []struct {
		name  string
		left  ast.Expression
		op    string
		right ast.Expression
		want  expr.Value
	}{
		{"contains_yes", strLit("hello world"), "CONTAINS", strLit("world"), expr.BoolValue(true)},
		{"contains_no", strLit("hello"), "CONTAINS", strLit("xyz"), expr.BoolValue(false)},
		{"starts_with_yes", strLit("hello"), "STARTS WITH", strLit("hel"), expr.BoolValue(true)},
		{"starts_with_no", strLit("hello"), "STARTS WITH", strLit("llo"), expr.BoolValue(false)},
		{"ends_with_yes", strLit("hello"), "ENDS WITH", strLit("llo"), expr.BoolValue(true)},
		{"ends_with_no", strLit("hello"), "ENDS WITH", strLit("hel"), expr.BoolValue(false)},
		{"null_contains", nullLit(), "CONTAINS", strLit("x"), expr.Null},
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

// ─────────────────────────────────────────────────────────────────────────────
// 18. ListComprehension — basic evaluation
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_ListComprehension_Basic(t *testing.T) {
	// [x IN [1,2,3] | x * 2] → [2,4,6]
	row := expr.RowContext{}
	e := &ast.ListComprehension{
		Variable:   "x",
		Source:     listLit(intLit(1), intLit(2), intLit(3)),
		Projection: binary(varExpr("x"), "*", intLit(2)),
	}
	v := eval(t, e, row, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("got %v (%T), want list of 3", v, v)
	}
	for i, want := range []expr.Value{expr.IntegerValue(2), expr.IntegerValue(4), expr.IntegerValue(6)} {
		if lv[i] != want {
			t.Errorf("[%d] got %v, want %v", i, lv[i], want)
		}
	}
}

// TestEval_UnsupportedNode_Error tests the default branch with a node type that
// is never supported (PatternComprehension — executor-level, not expression-level).
func TestEval_UnsupportedNode_Error(t *testing.T) {
	e := &ast.PatternComprehension{
		Projection: intLit(1),
	}
	_, err := expr.Eval(e, nil, nil, noReg)
	if err == nil {
		t.Fatal("expected error for unsupported expression type, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 19. Function invocation
// ─────────────────────────────────────────────────────────────────────────────

type singleFnReg struct {
	name string
	fn   expr.BuiltinFn
}

func (r *singleFnReg) Resolve(name string) (expr.BuiltinFn, bool) {
	if name == r.name {
		return r.fn, true
	}
	return nil, false
}

func TestEval_FunctionInvocation(t *testing.T) {
	reg := &singleFnReg{
		name: "double",
		fn: func(args []expr.Value) (expr.Value, error) {
			n := args[0].(expr.IntegerValue) //nolint:forcetypeassert // test only
			return expr.IntegerValue(int64(n) * 2), nil
		},
	}

	e := &ast.FunctionInvocation{
		Name: "double",
		Args: []ast.Expression{intLit(21)},
	}
	v, err := expr.Eval(e, nil, nil, reg)
	if err != nil {
		t.Fatalf("Eval error: %v", err)
	}
	if v != expr.IntegerValue(42) {
		t.Errorf("got %v, want 42", v)
	}
}

func TestEval_FunctionInvocation_NoRegistry(t *testing.T) {
	e := &ast.FunctionInvocation{
		Name: "something",
		Args: []ast.Expression{intLit(1)},
	}
	_, err := expr.Eval(e, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error when no registry provided")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 20. Subscript access
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Subscript(t *testing.T) {
	t.Run("list_index", func(t *testing.T) {
		row := expr.RowContext{
			"lst": expr.ListValue{expr.IntegerValue(10), expr.IntegerValue(20), expr.IntegerValue(30)},
		}
		e := &ast.SubscriptExpr{Expr: varExpr("lst"), Index: intLit(1)}
		v := eval(t, e, row, nil)
		if v != expr.IntegerValue(20) {
			t.Errorf("got %v, want 20", v)
		}
	})
	t.Run("list_out_of_bounds", func(t *testing.T) {
		row := expr.RowContext{
			"lst": expr.ListValue{expr.IntegerValue(10)},
		}
		e := &ast.SubscriptExpr{Expr: varExpr("lst"), Index: intLit(5)}
		v := eval(t, e, row, nil)
		if !expr.IsNull(v) {
			t.Errorf("got %v, want null for out-of-bounds", v)
		}
	})
	t.Run("map_key", func(t *testing.T) {
		row := expr.RowContext{
			"m": expr.MapValue{"k": expr.StringValue("val")},
		}
		e := &ast.SubscriptExpr{Expr: varExpr("m"), Index: strLit("k")}
		v := eval(t, e, row, nil)
		if v != expr.StringValue("val") {
			t.Errorf("got %v, want val", v)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 21. XOR truth table
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_XOR(t *testing.T) {
	tests := []struct {
		left  ast.Expression
		right ast.Expression
		want  expr.Value
	}{
		{boolLit(true), boolLit(true), expr.BoolValue(false)},
		{boolLit(true), boolLit(false), expr.BoolValue(true)},
		{boolLit(false), boolLit(true), expr.BoolValue(true)},
		{boolLit(false), boolLit(false), expr.BoolValue(false)},
		{nullLit(), boolLit(true), expr.Null},
		{boolLit(true), nullLit(), expr.Null},
	}
	for _, tc := range tests {
		t.Run(tc.left.String()+"_XOR_"+tc.right.String(), func(t *testing.T) {
			e := binary(tc.left, "XOR", tc.right)
			v := eval(t, e, nil, nil)
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 22. Exponentiation (^)
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_Exponentiation(t *testing.T) {
	v := eval(t, binary(intLit(2), "^", intLit(10)), nil, nil)
	if v != expr.FloatValue(1024.0) {
		t.Errorf("got %v, want 1024.0", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 23. NULL = NULL is NULL (not TRUE)
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_NullEqualNull_IsNull(t *testing.T) {
	v := eval(t, binary(nullLit(), "=", nullLit()), nil, nil)
	if !expr.IsNull(v) {
		t.Errorf("NULL = NULL should be NULL, got %v", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 24. IS NULL on NULL is TRUE
// ─────────────────────────────────────────────────────────────────────────────

func TestEval_IsNull_OnNull_IsTrue(t *testing.T) {
	v := eval(t, unary("IS NULL", nullLit()), nil, nil)
	if v != expr.BoolValue(true) {
		t.Errorf("IS NULL on NULL should be TRUE, got %v", v)
	}
}
