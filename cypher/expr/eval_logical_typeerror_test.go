package expr_test

// eval_logical_typeerror_test.go — regression tests for #2059.
//
// The 3VL logical operators AND/OR/XOR and unary NOT must raise an
// InvalidArgumentType evaluation error when an operand that is actually
// evaluated is a non-null, non-Boolean runtime value (parameter, property,
// variable), instead of silently coercing it to a boolean or to NULL. The
// compile-time literal guard in cypher/sema only catches syntactically-known
// literals; runtime values (parameters, properties) can only be caught here.
//
// The tests also pin the invariants the fix must NOT regress: NULL is a legal
// three-valued-logic operand (Kleene tables preserved) and genuinely-boolean
// short-circuit operands (false AND _, true OR _) skip evaluation of the other
// operand entirely, so a non-boolean short-circuited operand is never reached.

import (
	"errors"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// prop builds a property-access AST node (receiver.key).
func prop(recv ast.Expression, key string) *ast.Property {
	return &ast.Property{Pos: pos, Receiver: recv, Key: key}
}

// TestEval_Logical_NonBooleanRuntimeOperand_TypeError verifies that a non-null,
// non-Boolean operand supplied at runtime (a parameter or a property) raises an
// InvalidArgumentType evaluation error for every logical operator, rather than
// being silently coerced (#2059).
func TestEval_Logical_NonBooleanRuntimeOperand_TypeError(t *testing.T) {
	intParams := map[string]expr.Value{"x": expr.IntegerValue(5)}
	strParams := map[string]expr.Value{"x": expr.StringValue("hello")}
	// mapRow models `WHERE m.active <op> ...` where m.active was stored as a
	// non-boolean under schema drift.
	mapRow := expr.RowContext{"m": expr.MapValue{"active": expr.IntegerValue(1)}}

	cases := []struct {
		name   string
		e      ast.Expression
		row    expr.RowContext
		params map[string]expr.Value
	}{
		// Parameter $x = 5 (Integer) as a logical operand, left and right.
		{"and_param_int_left", binary(paramExpr("x"), "AND", boolLit(true)), nil, intParams},
		{"and_param_int_right", binary(boolLit(true), "AND", paramExpr("x")), nil, intParams},
		{"or_param_int_left", binary(paramExpr("x"), "OR", boolLit(false)), nil, intParams},
		{"or_param_int_right", binary(boolLit(false), "OR", paramExpr("x")), nil, intParams},
		{"xor_param_int_left", binary(paramExpr("x"), "XOR", boolLit(true)), nil, intParams},
		{"xor_param_int_right", binary(boolLit(true), "XOR", paramExpr("x")), nil, intParams},
		{"not_param_int", unary("NOT", paramExpr("x")), nil, intParams},
		// Parameter $x = "hello" (String).
		{"and_param_str", binary(paramExpr("x"), "AND", boolLit(true)), nil, strParams},
		{"or_param_str", binary(boolLit(false), "OR", paramExpr("x")), nil, strParams},
		{"xor_param_str", binary(paramExpr("x"), "XOR", boolLit(true)), nil, strParams},
		{"not_param_str", unary("NOT", paramExpr("x")), nil, strParams},
		// Property operand (int) as in `WHERE m.active AND true`.
		{"and_prop_int", binary(prop(varExpr("m"), "active"), "AND", boolLit(true)), mapRow, nil},
		{"or_prop_int", binary(prop(varExpr("m"), "active"), "OR", boolLit(false)), mapRow, nil},
		{"xor_prop_int", binary(prop(varExpr("m"), "active"), "XOR", boolLit(true)), mapRow, nil},
		{"not_prop_int", unary("NOT", prop(varExpr("m"), "active")), mapRow, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := expr.Eval(tc.e, tc.row, tc.params, noReg)
			if err == nil {
				t.Fatalf("expected InvalidArgumentType error, got value %v (%T)", v, v)
			}
			var ee *expr.EvalError
			if !errors.As(err, &ee) {
				t.Fatalf("expected *expr.EvalError, got %T: %v", err, err)
			}
			if !strings.HasPrefix(ee.Msg, "InvalidArgumentType:") {
				t.Fatalf("error message %q must start with %q so it maps to the InvalidArgumentType taxonomy",
					ee.Msg, "InvalidArgumentType:")
			}
		})
	}
}

// TestEval_Logical_NullAndShortCircuit_Preserved pins the invariants the #2059
// fix must not regress: the Kleene NULL truth tables and boolean short-circuit
// (which must not evaluate — nor type-check — the other operand).
func TestEval_Logical_NullAndShortCircuit_Preserved(t *testing.T) {
	// $x = 5 is a non-boolean; the short-circuit cases below must NOT touch it.
	intParams := map[string]expr.Value{"x": expr.IntegerValue(5)}

	cases := []struct {
		name string
		e    ast.Expression
		want expr.Value
	}{
		// NULL is a legal 3VL operand.
		{"null_and_true", binary(nullLit(), "AND", boolLit(true)), expr.Null},
		{"true_and_null", binary(boolLit(true), "AND", nullLit()), expr.Null},
		{"null_or_false", binary(nullLit(), "OR", boolLit(false)), expr.Null},
		{"false_or_null", binary(boolLit(false), "OR", nullLit()), expr.Null},
		{"null_xor_true", binary(nullLit(), "XOR", boolLit(true)), expr.Null},
		{"true_xor_null", binary(boolLit(true), "XOR", nullLit()), expr.Null},
		{"not_null", unary("NOT", nullLit()), expr.Null},
		// Kleene basics still hold.
		{"true_and_true", binary(boolLit(true), "AND", boolLit(true)), expr.BoolValue(true)},
		{"true_and_false", binary(boolLit(true), "AND", boolLit(false)), expr.BoolValue(false)},
		{"false_and_true", binary(boolLit(false), "AND", boolLit(true)), expr.BoolValue(false)},
		{"true_or_false", binary(boolLit(true), "OR", boolLit(false)), expr.BoolValue(true)},
		{"false_or_false", binary(boolLit(false), "OR", boolLit(false)), expr.BoolValue(false)},
		{"true_xor_false", binary(boolLit(true), "XOR", boolLit(false)), expr.BoolValue(true)},
		{"true_xor_true", binary(boolLit(true), "XOR", boolLit(true)), expr.BoolValue(false)},
		{"not_true", unary("NOT", boolLit(true)), expr.BoolValue(false)},
		{"not_false", unary("NOT", boolLit(false)), expr.BoolValue(true)},
		// Short-circuit: the non-boolean operand ($x=5) must NOT be evaluated,
		// so no type error is raised (matches Neo4j, which type-checks only the
		// evaluated operand).
		{"false_and_nonbool", binary(boolLit(false), "AND", paramExpr("x")), expr.BoolValue(false)},
		{"true_or_nonbool", binary(boolLit(true), "OR", paramExpr("x")), expr.BoolValue(true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := expr.Eval(tc.e, nil, intParams, noReg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v != tc.want {
				t.Fatalf("got %v (%T), want %v (%T)", v, v, tc.want, tc.want)
			}
		})
	}
}
