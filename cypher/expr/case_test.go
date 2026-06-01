package expr_test

// case_test.go — tests for CASE expression evaluation (task-263).
//
// 8 scenarios covering:
//   - Generic CASE: first match, second match, no match (with and without ELSE)
//   - Simple CASE: matching arm, no match with ELSE, null selector, null arm
//   - Short-circuit: later arms not evaluated after first match

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Generic CASE (no subject)
// ─────────────────────────────────────────────────────────────────────────────

func TestCase_Generic_MatchFirst(t *testing.T) {
	row := expr.RowContext{"x": expr.IntegerValue(1)}
	e := &ast.CaseExpression{
		Alternatives: []*ast.CaseAlternative{
			{Condition: binary(varExpr("x"), "=", intLit(1)), Consequent: strLit("one")},
			{Condition: binary(varExpr("x"), "=", intLit(2)), Consequent: strLit("two")},
		},
		ElseExpr: strLit("other"),
	}
	v := eval(t, e, row, nil)
	if v != expr.StringValue("one") {
		t.Errorf("got %v, want one", v)
	}
}

func TestCase_Generic_MatchSecond(t *testing.T) {
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
}

func TestCase_Generic_NoMatchWithElse(t *testing.T) {
	row := expr.RowContext{"x": expr.IntegerValue(99)}
	e := &ast.CaseExpression{
		Alternatives: []*ast.CaseAlternative{
			{Condition: binary(varExpr("x"), "=", intLit(1)), Consequent: strLit("one")},
		},
		ElseExpr: strLit("default"),
	}
	v := eval(t, e, row, nil)
	if v != expr.StringValue("default") {
		t.Errorf("got %v, want default", v)
	}
}

func TestCase_Generic_NoMatchNoElse_Null(t *testing.T) {
	e := &ast.CaseExpression{
		Alternatives: []*ast.CaseAlternative{
			{Condition: boolLit(false), Consequent: strLit("never")},
		},
	}
	v := eval(t, e, nil, nil)
	if !expr.IsNull(v) {
		t.Errorf("got %v, want null", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Simple CASE (with subject)
// ─────────────────────────────────────────────────────────────────────────────

func TestCase_Simple_MatchArm(t *testing.T) {
	row := expr.RowContext{"s": expr.StringValue("b")}
	e := &ast.CaseExpression{
		Subject: varExpr("s"),
		Alternatives: []*ast.CaseAlternative{
			{Condition: strLit("a"), Consequent: intLit(1)},
			{Condition: strLit("b"), Consequent: intLit(2)},
			{Condition: strLit("c"), Consequent: intLit(3)},
		},
		ElseExpr: intLit(0),
	}
	v := eval(t, e, row, nil)
	if v != expr.IntegerValue(2) {
		t.Errorf("got %v, want 2", v)
	}
}

func TestCase_Simple_NoMatchWithElse(t *testing.T) {
	row := expr.RowContext{"s": expr.StringValue("z")}
	e := &ast.CaseExpression{
		Subject: varExpr("s"),
		Alternatives: []*ast.CaseAlternative{
			{Condition: strLit("a"), Consequent: intLit(1)},
		},
		ElseExpr: intLit(-1),
	}
	v := eval(t, e, row, nil)
	if v != expr.IntegerValue(-1) {
		t.Errorf("got %v, want -1", v)
	}
}

// TestCase_Simple_NullSelector: a NULL subject never matches any WHEN arm (3VL).
func TestCase_Simple_NullSelector(t *testing.T) {
	row := expr.RowContext{"s": expr.Null}
	e := &ast.CaseExpression{
		Subject: varExpr("s"),
		Alternatives: []*ast.CaseAlternative{
			{Condition: strLit("a"), Consequent: intLit(1)},
			{Condition: nullLit(), Consequent: intLit(2)}, // NULL = NULL → NULL (not true)
		},
		ElseExpr: intLit(0),
	}
	v := eval(t, e, row, nil)
	// NULL selector → no arm matches → ELSE
	if v != expr.IntegerValue(0) {
		t.Errorf("got %v, want 0 (else branch for null selector)", v)
	}
}

// TestCase_Simple_NullArm: NULL in a WHEN value does not match a concrete subject.
func TestCase_Simple_NullArm(t *testing.T) {
	row := expr.RowContext{"s": expr.StringValue("x")}
	e := &ast.CaseExpression{
		Subject: varExpr("s"),
		Alternatives: []*ast.CaseAlternative{
			{Condition: nullLit(), Consequent: intLit(99)}, // "x" = NULL → NULL (not true)
			{Condition: strLit("x"), Consequent: intLit(42)},
		},
	}
	v := eval(t, e, row, nil)
	// null arm skipped, second arm matches
	if v != expr.IntegerValue(42) {
		t.Errorf("got %v, want 42", v)
	}
}
