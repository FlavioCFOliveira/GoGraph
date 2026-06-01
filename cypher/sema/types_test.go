package sema_test

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// inferOK calls InferType and fails immediately if an error is returned.
func inferOK(t *testing.T, expr ast.Expression) sema.CypherType {
	t.Helper()
	got, err := sema.InferType(expr)
	if err != nil {
		t.Fatalf("unexpected TypeError: %v", err)
	}
	return got
}

// inferErr calls InferType and fails if no error is returned.
func inferErr(t *testing.T, expr ast.Expression) *sema.TypeError {
	t.Helper()
	_, err := sema.InferType(expr)
	if err == nil {
		t.Fatal("expected TypeError, got nil")
	}
	var te *sema.TypeError
	if !errors.As(err, &te) {
		t.Fatalf("expected *sema.TypeError, got %T: %v", err, err)
	}
	return te
}

// assertType asserts a specific CypherType result with no error.
func assertType(t *testing.T, expr ast.Expression, want sema.CypherType) {
	t.Helper()
	got := inferOK(t, expr)
	if got != want {
		t.Errorf("InferType(%T) = %v, want %v", expr, got, want)
	}
}

// intLit builds an integer literal.
func intLit(v int64) *ast.IntLiteral { return &ast.IntLiteral{Value: v} }

// floatLit builds a float literal.
func floatLit(v float64) *ast.FloatLiteral { return &ast.FloatLiteral{Value: v} }

// strLit builds a string literal.
func strLit(s string) *ast.StringLiteral { return &ast.StringLiteral{Value: s} }

// boolLit builds a bool literal.
func boolLit(v bool) *ast.BoolLiteral { return &ast.BoolLiteral{Value: v} }

// nullLit builds a null literal.
func nullLit() *ast.NullLiteral { return &ast.NullLiteral{} }

// binop constructs a BinaryOp expression.
func binop(left ast.Expression, op string, right ast.Expression) *ast.BinaryOp {
	return &ast.BinaryOp{Left: left, Operator: op, Right: right}
}

// unop constructs a UnaryOp expression.
func unop(op string, operand ast.Expression) *ast.UnaryOp {
	return &ast.UnaryOp{Operator: op, Operand: operand}
}

// fn constructs a FunctionInvocation with no namespace.
func fn(name string, args ...ast.Expression) *ast.FunctionInvocation {
	return &ast.FunctionInvocation{Name: name, Args: args}
}

// listLit constructs a ListLiteral.
func listLit(elems ...ast.Expression) *ast.ListLiteral {
	return &ast.ListLiteral{Elements: elems}
}

// mapLit constructs a MapLiteral with a single key.
func mapLit() *ast.MapLiteral {
	return &ast.MapLiteral{Keys: []string{"k"}, Values: []ast.Expression{intLit(1)}}
}

// param constructs a Parameter.
func param(name string) *ast.Parameter { return &ast.Parameter{Name: name} }

// variable constructs a Variable.
func variable(name string) *ast.Variable { return &ast.Variable{Name: name} }

// ─────────────────────────────────────────────────────────────────────────────
// 1. Literal types
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_NullLiteral(t *testing.T) {
	assertType(t, nullLit(), sema.TypeNull)
}

func TestInfer_BoolLiteralTrue(t *testing.T) {
	assertType(t, boolLit(true), sema.TypeBoolean)
}

func TestInfer_BoolLiteralFalse(t *testing.T) {
	assertType(t, boolLit(false), sema.TypeBoolean)
}

func TestInfer_IntLiteral(t *testing.T) {
	assertType(t, intLit(42), sema.TypeInteger)
}

func TestInfer_FloatLiteral(t *testing.T) {
	assertType(t, floatLit(3.14), sema.TypeFloat)
}

func TestInfer_StringLiteral(t *testing.T) {
	assertType(t, strLit("hello"), sema.TypeString)
}

func TestInfer_ListLiteral(t *testing.T) {
	assertType(t, listLit(intLit(1), intLit(2)), sema.TypeList)
}

func TestInfer_MapLiteral(t *testing.T) {
	assertType(t, mapLit(), sema.TypeMap)
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Variable and parameter
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_Variable(t *testing.T) {
	assertType(t, variable("n"), sema.TypeAny)
}

func TestInfer_Parameter(t *testing.T) {
	assertType(t, param("userId"), sema.TypeAny)
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Binary + operator — arithmetic coercion
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_BinPlus_IntInt(t *testing.T) {
	assertType(t, binop(intLit(1), "+", intLit(2)), sema.TypeInteger)
}

func TestInfer_BinPlus_FloatFloat(t *testing.T) {
	assertType(t, binop(floatLit(1.5), "+", floatLit(2.5)), sema.TypeFloat)
}

func TestInfer_BinPlus_IntFloat_CoercesToFloat(t *testing.T) {
	// Int + Float → Float (oC9 coercion)
	assertType(t, binop(intLit(1), "+", floatLit(2.0)), sema.TypeFloat)
}

func TestInfer_BinPlus_FloatInt_CoercesToFloat(t *testing.T) {
	// Float + Int → Float
	assertType(t, binop(floatLit(2.0), "+", intLit(1)), sema.TypeFloat)
}

func TestInfer_BinPlus_StringString(t *testing.T) {
	assertType(t, binop(strLit("a"), "+", strLit("b")), sema.TypeString)
}

func TestInfer_BinPlus_ListList(t *testing.T) {
	assertType(t, binop(listLit(intLit(1)), "+", listLit(intLit(2))), sema.TypeList)
}

// oC9: String + Number is a TypeError, NOT String concatenation.
func TestInfer_BinPlus_StringInt_TypeError(t *testing.T) {
	te := inferErr(t, binop(strLit("x"), "+", intLit(1)))
	if te.Op != "+" {
		t.Errorf("want op +, got %q", te.Op)
	}
	if te.Left != sema.TypeString {
		t.Errorf("want left String, got %v", te.Left)
	}
	if te.Right != sema.TypeInteger {
		t.Errorf("want right Integer, got %v", te.Right)
	}
}

func TestInfer_BinPlus_IntString_TypeError(t *testing.T) {
	_ = inferErr(t, binop(intLit(1), "+", strLit("x")))
}

func TestInfer_BinPlus_MapList_TypeError(t *testing.T) {
	_ = inferErr(t, binop(mapLit(), "+", listLit(intLit(1))))
}

func TestInfer_BinPlus_ListMap_TypeError(t *testing.T) {
	_ = inferErr(t, binop(listLit(intLit(1)), "+", mapLit()))
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Arithmetic: -, *, /, %
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_BinMinus_IntInt(t *testing.T) {
	assertType(t, binop(intLit(5), "-", intLit(3)), sema.TypeInteger)
}

func TestInfer_BinMinus_FloatInt_CoercesToFloat(t *testing.T) {
	assertType(t, binop(floatLit(5.0), "-", intLit(3)), sema.TypeFloat)
}

func TestInfer_BinMul_IntFloat(t *testing.T) {
	assertType(t, binop(intLit(2), "*", floatLit(3.14)), sema.TypeFloat)
}

func TestInfer_BinDiv_IntInt(t *testing.T) {
	// In oC9, integer / integer → integer (truncating division)
	assertType(t, binop(intLit(6), "/", intLit(3)), sema.TypeInteger)
}

func TestInfer_BinMod_IntInt(t *testing.T) {
	assertType(t, binop(intLit(7), "%", intLit(3)), sema.TypeInteger)
}

func TestInfer_BinMinus_StringInt_TypeError(t *testing.T) {
	_ = inferErr(t, binop(strLit("a"), "-", intLit(1)))
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Power operator (^)
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_BinPow_IntInt_YieldsFloat(t *testing.T) {
	// oC9: ^ always yields Float
	assertType(t, binop(intLit(2), "^", intLit(10)), sema.TypeFloat)
}

func TestInfer_BinPow_FloatInt_YieldsFloat(t *testing.T) {
	assertType(t, binop(floatLit(2.0), "^", intLit(3)), sema.TypeFloat)
}

func TestInfer_BinPow_StringInt_TypeError(t *testing.T) {
	_ = inferErr(t, binop(strLit("x"), "^", intLit(2)))
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Comparison operators
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_Eq_IntInt(t *testing.T) {
	assertType(t, binop(intLit(1), "=", intLit(1)), sema.TypeBoolean)
}

func TestInfer_Neq_StringString(t *testing.T) {
	assertType(t, binop(strLit("a"), "<>", strLit("b")), sema.TypeBoolean)
}

func TestInfer_Lt_FloatFloat(t *testing.T) {
	assertType(t, binop(floatLit(1.0), "<", floatLit(2.0)), sema.TypeBoolean)
}

func TestInfer_Gte_IntFloat(t *testing.T) {
	assertType(t, binop(intLit(1), ">=", floatLit(1.0)), sema.TypeBoolean)
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Logical operators
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_And_BoolBool(t *testing.T) {
	assertType(t, binop(boolLit(true), "AND", boolLit(false)), sema.TypeBoolean)
}

func TestInfer_Or_BoolBool(t *testing.T) {
	assertType(t, binop(boolLit(true), "OR", boolLit(false)), sema.TypeBoolean)
}

func TestInfer_Xor_BoolBool(t *testing.T) {
	assertType(t, binop(boolLit(true), "XOR", boolLit(false)), sema.TypeBoolean)
}

func TestInfer_And_IntBool_TypeError(t *testing.T) {
	_ = inferErr(t, binop(intLit(1), "AND", boolLit(true)))
}

func TestInfer_Or_StringBool_TypeError(t *testing.T) {
	_ = inferErr(t, binop(strLit("x"), "OR", boolLit(true)))
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. IN operator
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_In_IntList(t *testing.T) {
	assertType(t, binop(intLit(1), "IN", listLit(intLit(1), intLit(2))), sema.TypeBoolean)
}

func TestInfer_In_StringList(t *testing.T) {
	assertType(t, binop(strLit("a"), "IN", listLit(strLit("a"), strLit("b"))), sema.TypeBoolean)
}

func TestInfer_In_IntMap_TypeError(t *testing.T) {
	_ = inferErr(t, binop(intLit(1), "IN", mapLit()))
}

func TestInfer_In_IntString_TypeError(t *testing.T) {
	_ = inferErr(t, binop(intLit(1), "IN", strLit("abc")))
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. String predicates
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_StartsWith_StringString(t *testing.T) {
	assertType(t, binop(strLit("hello"), "STARTS WITH", strLit("hel")), sema.TypeBoolean)
}

func TestInfer_EndsWith_StringString(t *testing.T) {
	assertType(t, binop(strLit("hello"), "ENDS WITH", strLit("lo")), sema.TypeBoolean)
}

func TestInfer_Contains_StringString(t *testing.T) {
	assertType(t, binop(strLit("hello"), "CONTAINS", strLit("ell")), sema.TypeBoolean)
}

func TestInfer_StartsWith_IntString_TypeError(t *testing.T) {
	_ = inferErr(t, binop(intLit(1), "STARTS WITH", strLit("1")))
}

// ─────────────────────────────────────────────────────────────────────────────
// 10. Regex
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_Regex_StringString(t *testing.T) {
	assertType(t, binop(strLit("hello"), "=~", strLit("hel.*")), sema.TypeBoolean)
}

func TestInfer_Regex_IntString_TypeError(t *testing.T) {
	_ = inferErr(t, binop(intLit(1), "=~", strLit(".*")))
}

// ─────────────────────────────────────────────────────────────────────────────
// 11. Unary operators
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_UnaryNot_Bool(t *testing.T) {
	assertType(t, unop("NOT", boolLit(true)), sema.TypeBoolean)
}

func TestInfer_UnaryNot_Int_TypeError(t *testing.T) {
	_ = inferErr(t, unop("NOT", intLit(1)))
}

func TestInfer_UnaryNot_String_TypeError(t *testing.T) {
	_ = inferErr(t, unop("NOT", strLit("x")))
}

func TestInfer_UnaryMinus_Integer(t *testing.T) {
	assertType(t, unop("-", intLit(5)), sema.TypeInteger)
}

func TestInfer_UnaryMinus_Float(t *testing.T) {
	assertType(t, unop("-", floatLit(3.14)), sema.TypeFloat)
}

func TestInfer_UnaryMinus_String_TypeError(t *testing.T) {
	_ = inferErr(t, unop("-", strLit("x")))
}

func TestInfer_IsNull_AnyType(t *testing.T) {
	assertType(t, unop("IS NULL", variable("n")), sema.TypeBoolean)
}

func TestInfer_IsNotNull_String(t *testing.T) {
	assertType(t, unop("IS NOT NULL", strLit("x")), sema.TypeBoolean)
}

// ─────────────────────────────────────────────────────────────────────────────
// 12. Null propagation
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_Plus_NullInt(t *testing.T) {
	// null + anything → null (null propagation)
	assertType(t, binop(nullLit(), "+", intLit(1)), sema.TypeNull)
}

func TestInfer_Plus_IntNull(t *testing.T) {
	assertType(t, binop(intLit(1), "+", nullLit()), sema.TypeNull)
}

// ─────────────────────────────────────────────────────────────────────────────
// 13. TypeAny short-circuit — no error when operand is Any
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_BinPlus_AnyInt_NoError(t *testing.T) {
	// variable + literal → Any (no type error because variable is Any)
	got := inferOK(t, binop(variable("n"), "+", intLit(1)))
	if got == sema.TypeBoolean {
		t.Errorf("unexpected Boolean for Any+Int; expected non-error result")
	}
}

func TestInfer_And_AnyBool_NoError(t *testing.T) {
	// AND with Any operand should not error
	inferOK(t, binop(variable("flag"), "AND", boolLit(true)))
}

func TestInfer_In_IntAny_NoError(t *testing.T) {
	// x IN $list → Any (parameter is Any; no error)
	got := inferOK(t, binop(intLit(1), "IN", param("list")))
	_ = got // result can be Any or Boolean depending on impl
}

// ─────────────────────────────────────────────────────────────────────────────
// 14. Function invocation
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_Func_count(t *testing.T) {
	assertType(t, fn("count", variable("n")), sema.TypeInteger)
}

func TestInfer_Func_size(t *testing.T) {
	assertType(t, fn("size", listLit(intLit(1))), sema.TypeInteger)
}

func TestInfer_Func_id(t *testing.T) {
	assertType(t, fn("id", variable("n")), sema.TypeInteger)
}

func TestInfer_Func_type(t *testing.T) {
	assertType(t, fn("type", variable("r")), sema.TypeString)
}

func TestInfer_Func_labels(t *testing.T) {
	assertType(t, fn("labels", variable("n")), sema.TypeList)
}

func TestInfer_Func_keys(t *testing.T) {
	assertType(t, fn("keys", variable("n")), sema.TypeList)
}

func TestInfer_Func_toString(t *testing.T) {
	assertType(t, fn("toString", intLit(1)), sema.TypeString)
}

func TestInfer_Func_toInteger(t *testing.T) {
	assertType(t, fn("toInteger", strLit("42")), sema.TypeInteger)
}

func TestInfer_Func_toFloat(t *testing.T) {
	assertType(t, fn("toFloat", strLit("3.14")), sema.TypeFloat)
}

func TestInfer_Func_coalesce(t *testing.T) {
	assertType(t, fn("coalesce", variable("x"), intLit(0)), sema.TypeAny)
}

func TestInfer_Func_sqrt(t *testing.T) {
	assertType(t, fn("sqrt", intLit(4)), sema.TypeFloat)
}

func TestInfer_Func_unknown(t *testing.T) {
	// Unknown function falls back to TypeAny (no error).
	assertType(t, fn("apoc.something.unknown"), sema.TypeAny)
}

// ─────────────────────────────────────────────────────────────────────────────
// 15. Compound expressions
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_Compound_IntPlusFloat_LtFloat(t *testing.T) {
	// (1 + 2.0) < 5 → Boolean
	inner := binop(intLit(1), "+", floatLit(2.0)) // → Float
	outer := binop(inner, "<", intLit(5))
	assertType(t, outer, sema.TypeBoolean)
}

func TestInfer_Compound_BoolAndNotBool(t *testing.T) {
	// true AND NOT false → Boolean
	notFalse := unop("NOT", boolLit(false))
	assertType(t, binop(boolLit(true), "AND", notFalse), sema.TypeBoolean)
}

func TestInfer_Compound_PropagatesError(t *testing.T) {
	// (1 + "x") + 2 → TypeError propagates from inner expression.
	inner := binop(intLit(1), "+", strLit("x")) // TypeError
	outer := binop(inner, "+", intLit(2))
	_ = inferErr(t, outer)
}

// ─────────────────────────────────────────────────────────────────────────────
// 16. Special expression types
// ─────────────────────────────────────────────────────────────────────────────

func TestInfer_Property(t *testing.T) {
	assertType(t, &ast.Property{Receiver: variable("n"), Key: "name"}, sema.TypeAny)
}

func TestInfer_SubscriptExpr(t *testing.T) {
	assertType(t, &ast.SubscriptExpr{Expr: variable("xs"), Index: intLit(0)}, sema.TypeAny)
}

func TestInfer_SliceExpr(t *testing.T) {
	assertType(t, &ast.SliceExpr{Expr: variable("xs"), From: intLit(0), To: intLit(3)}, sema.TypeList)
}

func TestInfer_ListComprehension(t *testing.T) {
	assertType(t, &ast.ListComprehension{
		Variable:   "x",
		Source:     listLit(intLit(1)),
		Projection: variable("x"),
	}, sema.TypeList)
}

func TestInfer_PatternComprehension(t *testing.T) {
	assertType(t, &ast.PatternComprehension{
		Pattern:    &ast.PathPattern{},
		Projection: variable("m"),
	}, sema.TypeList)
}

func TestInfer_MapProjection(t *testing.T) {
	assertType(t, &ast.MapProjection{
		Subject: variable("n"),
		Items:   []*ast.MapProjectionItem{{Key: "name"}},
	}, sema.TypeMap)
}

func TestInfer_ExistsSubquery(t *testing.T) {
	assertType(t, &ast.ExistsSubquery{Pattern: &ast.Pattern{}}, sema.TypeBoolean)
}

func TestInfer_CountSubquery(t *testing.T) {
	assertType(t, &ast.CountSubquery{Pattern: &ast.Pattern{}}, sema.TypeInteger)
}

func TestInfer_PathPattern(t *testing.T) {
	assertType(t, &ast.PathPattern{}, sema.TypePath)
}

func TestInfer_CaseExpression(t *testing.T) {
	assertType(t, &ast.CaseExpression{
		Alternatives: []*ast.CaseAlternative{
			{Condition: boolLit(true), Consequent: intLit(1)},
		},
	}, sema.TypeAny)
}

func TestInfer_NilExpression(t *testing.T) {
	// nil expression is treated as null.
	assertType(t, nil, sema.TypeNull)
}

// ─────────────────────────────────────────────────────────────────────────────
// 17. TypeError — error interface and field integrity
// ─────────────────────────────────────────────────────────────────────────────

func TestTypeError_Message_BinaryOp(t *testing.T) {
	te := inferErr(t, binop(strLit("x"), "+", intLit(1)))
	msg := te.Error()
	if msg == "" {
		t.Fatal("TypeError.Error() returned empty string")
	}
	// Must mention the operator.
	if !contains(msg, "+") {
		t.Errorf("expected operator '+' in error message, got: %q", msg)
	}
}

func TestTypeError_Message_UnaryOp(t *testing.T) {
	te := inferErr(t, unop("NOT", intLit(42)))
	msg := te.Error()
	if msg == "" {
		t.Fatal("TypeError.Error() returned empty string")
	}
	if !contains(msg, "NOT") {
		t.Errorf("expected operator 'NOT' in error message, got: %q", msg)
	}
}

func TestTypeError_Fields_BinaryOp(t *testing.T) {
	te := inferErr(t, binop(strLit("a"), "AND", boolLit(true)))
	if te.Op != "AND" {
		t.Errorf("want Op=AND, got %q", te.Op)
	}
	if te.Left != sema.TypeString {
		t.Errorf("want Left=String, got %v", te.Left)
	}
	if te.Right != sema.TypeBoolean {
		t.Errorf("want Right=Boolean, got %v", te.Right)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 18. CypherType.String() coverage
// ─────────────────────────────────────────────────────────────────────────────

func TestCypherType_String(t *testing.T) {
	cases := []struct {
		t    sema.CypherType
		want string
	}{
		{sema.TypeAny, "Any"},
		{sema.TypeNull, "Null"},
		{sema.TypeBoolean, "Boolean"},
		{sema.TypeInteger, "Integer"},
		{sema.TypeFloat, "Float"},
		{sema.TypeString, "String"},
		{sema.TypeNode, "Node"},
		{sema.TypeRelationship, "Relationship"},
		{sema.TypePath, "Path"},
		{sema.TypeList, "List"},
		{sema.TypeMap, "Map"},
	}
	for _, tc := range cases {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("CypherType(%d).String() = %q, want %q", tc.t, got, tc.want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || sub == "" ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
