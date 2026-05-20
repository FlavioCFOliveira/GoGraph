package expr_test

// list_test.go — tests for list slicing, comprehension, and quantifiers (task-264).
//
// 30+ scenarios:
//   - List indexing (negative, out-of-bounds)
//   - Slice: basic, nil bounds, negative bounds, out-of-range, from > to
//   - Comprehension: projection, filter, filter+projection, null source, outer row
//   - Quantifiers via AST: all, any, none, single (correct behaviour + NULL)

import (
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/expr"
	"gograph/cypher/funcs"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func sliceExpr(src, from, to ast.Expression) *ast.SliceExpr {
	return &ast.SliceExpr{Expr: src, From: from, To: to}
}

func listComp(variable string, source, predicate, projection ast.Expression) *ast.ListComprehension {
	return &ast.ListComprehension{
		Variable:   variable,
		Source:     source,
		Predicate:  predicate,
		Projection: projection,
	}
}

// quantifierFn wraps a ListComprehension in a FunctionInvocation for all/any/none/single.
func quantifierFn(name string, lc *ast.ListComprehension) *ast.FunctionInvocation {
	return &ast.FunctionInvocation{Name: name, Args: []ast.Expression{lc}}
}

func evalWithReg(t *testing.T, e ast.Expression, row expr.RowContext, params map[string]expr.Value) expr.Value {
	t.Helper()
	v, err := expr.Eval(e, row, params, funcs.DefaultRegistry)
	if err != nil {
		t.Fatalf("Eval error: %v", err)
	}
	return v
}

// ─────────────────────────────────────────────────────────────────────────────
// List indexing (re-exercised with edge cases)
// ─────────────────────────────────────────────────────────────────────────────

func TestList_Index_Positive(t *testing.T) {
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(10), expr.IntegerValue(20), expr.IntegerValue(30)}}
	v := eval(t, &ast.SubscriptExpr{Expr: varExpr("lst"), Index: intLit(0)}, row, nil)
	if v != expr.IntegerValue(10) {
		t.Errorf("got %v, want 10", v)
	}
}

func TestList_Index_Negative(t *testing.T) {
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(10), expr.IntegerValue(20), expr.IntegerValue(30)}}
	v := eval(t, &ast.SubscriptExpr{Expr: varExpr("lst"), Index: intLit(-1)}, row, nil)
	if v != expr.IntegerValue(30) {
		t.Errorf("got %v, want 30", v)
	}
}

func TestList_Index_OutOfBounds(t *testing.T) {
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(1)}}
	v := eval(t, &ast.SubscriptExpr{Expr: varExpr("lst"), Index: intLit(10)}, row, nil)
	if !expr.IsNull(v) {
		t.Errorf("out-of-bounds should be null, got %v", v)
	}
}

func TestList_Index_NegativePastStart(t *testing.T) {
	// -99 + 2 = -97 < 0 → null
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}}
	v := eval(t, &ast.SubscriptExpr{Expr: varExpr("lst"), Index: intLit(-99)}, row, nil)
	if !expr.IsNull(v) {
		t.Errorf("large negative index should be null, got %v", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// List slicing
// ─────────────────────────────────────────────────────────────────────────────

func TestList_Slice_Basic(t *testing.T) {
	// [1,2,3,4,5][1..3] → [2,3]
	row := expr.RowContext{"lst": expr.ListValue{
		expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3),
		expr.IntegerValue(4), expr.IntegerValue(5),
	}}
	v := eval(t, sliceExpr(varExpr("lst"), intLit(1), intLit(3)), row, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 2 {
		t.Fatalf("got %v (%T), want list of 2", v, v)
	}
	if lv[0] != expr.IntegerValue(2) || lv[1] != expr.IntegerValue(3) {
		t.Errorf("got %v, want [2,3]", lv)
	}
}

func TestList_Slice_FromNil(t *testing.T) {
	// [1,2,3][..2] → [1,2]
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}}
	v := eval(t, sliceExpr(varExpr("lst"), nil, intLit(2)), row, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 2 {
		t.Fatalf("got %v (%T), want list of 2", v, v)
	}
}

func TestList_Slice_ToNil(t *testing.T) {
	// [1,2,3][1..] → [2,3]
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}}
	v := eval(t, sliceExpr(varExpr("lst"), intLit(1), nil), row, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 2 {
		t.Fatalf("got %v (%T), want list of 2", v, v)
	}
	if lv[0] != expr.IntegerValue(2) || lv[1] != expr.IntegerValue(3) {
		t.Errorf("got %v, want [2,3]", lv)
	}
}

func TestList_Slice_BothNil(t *testing.T) {
	// [1,2,3][..] → [1,2,3]
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}}
	v := eval(t, sliceExpr(varExpr("lst"), nil, nil), row, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("got %v (%T), want list of 3", v, v)
	}
}

func TestList_Slice_NullSource(t *testing.T) {
	v := eval(t, sliceExpr(nullLit(), intLit(0), intLit(2)), nil, nil)
	if !expr.IsNull(v) {
		t.Errorf("null source should return null, got %v", v)
	}
}

func TestList_Slice_FromGreaterThanTo(t *testing.T) {
	// [1,2,3][3..1] → []
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}}
	v := eval(t, sliceExpr(varExpr("lst"), intLit(3), intLit(1)), row, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 0 {
		t.Errorf("from > to should yield empty list, got %v", v)
	}
}

func TestList_Slice_ToOutOfRange(t *testing.T) {
	// [1,2,3][0..100] → [1,2,3] (to clamped)
	row := expr.RowContext{"lst": expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)}}
	v := eval(t, sliceExpr(varExpr("lst"), intLit(0), intLit(100)), row, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Errorf("to out-of-range should clamp, got %v", v)
	}
}

func TestList_Slice_NegativeFrom(t *testing.T) {
	// [1,2,3,4][-2..4] → [3,4]
	row := expr.RowContext{"lst": expr.ListValue{
		expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3), expr.IntegerValue(4),
	}}
	v := eval(t, sliceExpr(varExpr("lst"), intLit(-2), intLit(4)), row, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 2 {
		t.Fatalf("got %v (%T), want list of 2", v, v)
	}
	if lv[0] != expr.IntegerValue(3) || lv[1] != expr.IntegerValue(4) {
		t.Errorf("got %v, want [3,4]", lv)
	}
}

func TestList_Slice_NonList(t *testing.T) {
	// slicing a non-list → null
	v := eval(t, sliceExpr(intLit(42), intLit(0), intLit(1)), nil, nil)
	if !expr.IsNull(v) {
		t.Errorf("slicing non-list should be null, got %v", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// List concatenation
// ─────────────────────────────────────────────────────────────────────────────

func TestList_Concat(t *testing.T) {
	v := eval(t,
		binary(listLit(intLit(1), intLit(2)), "+", listLit(intLit(3), intLit(4))),
		nil, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 4 {
		t.Fatalf("got %v (%T), want list of 4", v, v)
	}
	for i, want := range []int64{1, 2, 3, 4} {
		if lv[i] != expr.IntegerValue(want) {
			t.Errorf("[%d] got %v, want %d", i, lv[i], want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// List comprehension
// ─────────────────────────────────────────────────────────────────────────────

func TestList_Comprehension_ProjectionOnly(t *testing.T) {
	// [x IN [1,2,3] | x * 2] → [2,4,6]
	e := listComp("x", listLit(intLit(1), intLit(2), intLit(3)),
		nil,
		binary(varExpr("x"), "*", intLit(2)))
	v := eval(t, e, nil, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("got %v (%T), want list of 3", v, v)
	}
	for i, want := range []int64{2, 4, 6} {
		if lv[i] != expr.IntegerValue(want) {
			t.Errorf("[%d] got %v, want %d", i, lv[i], want)
		}
	}
}

func TestList_Comprehension_FilterOnly(t *testing.T) {
	// [x IN [1,2,3,4,5] WHERE x > 3] → [4,5]
	e := listComp("x", listLit(intLit(1), intLit(2), intLit(3), intLit(4), intLit(5)),
		binary(varExpr("x"), ">", intLit(3)),
		nil)
	v := eval(t, e, nil, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 2 {
		t.Fatalf("got %v (%T), want list of 2", v, v)
	}
	if lv[0] != expr.IntegerValue(4) || lv[1] != expr.IntegerValue(5) {
		t.Errorf("got %v, want [4,5]", lv)
	}
}

func TestList_Comprehension_FilterAndProjection(t *testing.T) {
	// [x IN [1,2,3,4,5] WHERE x % 2 = 1 | x * x] → [1, 9, 25]
	e := listComp("x", listLit(intLit(1), intLit(2), intLit(3), intLit(4), intLit(5)),
		binary(binary(varExpr("x"), "%", intLit(2)), "=", intLit(1)),
		binary(varExpr("x"), "*", varExpr("x")))
	v := eval(t, e, nil, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("got %v (%T), want list of 3", v, v)
	}
	for i, want := range []int64{1, 9, 25} {
		if lv[i] != expr.IntegerValue(want) {
			t.Errorf("[%d] got %v, want %d", i, lv[i], want)
		}
	}
}

func TestList_Comprehension_NullSource(t *testing.T) {
	// [x IN null | x] → []
	e := listComp("x", nullLit(), nil, varExpr("x"))
	v := eval(t, e, nil, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 0 {
		t.Errorf("null source should yield empty list, got %v", v)
	}
}

func TestList_Comprehension_OuterRowAccess(t *testing.T) {
	// WITH factor = 3: [x IN [1,2,3] | x * factor] → [3,6,9]
	row := expr.RowContext{"factor": expr.IntegerValue(3)}
	e := listComp("x", listLit(intLit(1), intLit(2), intLit(3)),
		nil,
		binary(varExpr("x"), "*", varExpr("factor")))
	v := eval(t, e, row, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("got %v (%T), want list of 3", v, v)
	}
	for i, want := range []int64{3, 6, 9} {
		if lv[i] != expr.IntegerValue(want) {
			t.Errorf("[%d] got %v, want %d", i, lv[i], want)
		}
	}
}

func TestList_Comprehension_EmptyList(t *testing.T) {
	// [x IN [] | x] → []
	e := listComp("x", listLit(), nil, varExpr("x"))
	v := eval(t, e, nil, nil)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 0 {
		t.Errorf("empty source should yield empty list, got %v", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Quantifier functions: all, any, none, single
// ─────────────────────────────────────────────────────────────────────────────

// Build a FunctionInvocation{Name: name, Args: [ListComprehension{...}]}
// as the parser would emit for all(x IN list WHERE pred).

func TestList_All_AllTrue(t *testing.T) {
	// all(x IN [2,4,6] WHERE x % 2 = 0) → true
	lc := listComp("x", listLit(intLit(2), intLit(4), intLit(6)),
		binary(binary(varExpr("x"), "%", intLit(2)), "=", intLit(0)), nil)
	v := evalWithReg(t, quantifierFn("all", lc), nil, nil)
	if v != expr.BoolValue(true) {
		t.Errorf("all even: got %v, want true", v)
	}
}

func TestList_All_OneFalse(t *testing.T) {
	// all(x IN [2,3,4] WHERE x % 2 = 0) → false (3 is odd)
	lc := listComp("x", listLit(intLit(2), intLit(3), intLit(4)),
		binary(binary(varExpr("x"), "%", intLit(2)), "=", intLit(0)), nil)
	v := evalWithReg(t, quantifierFn("all", lc), nil, nil)
	if v != expr.BoolValue(false) {
		t.Errorf("not-all-even: got %v, want false", v)
	}
}

func TestList_All_EmptyList(t *testing.T) {
	// all(x IN [] WHERE x > 0) → true (vacuously)
	lc := listComp("x", listLit(),
		binary(varExpr("x"), ">", intLit(0)), nil)
	v := evalWithReg(t, quantifierFn("all", lc), nil, nil)
	if v != expr.BoolValue(true) {
		t.Errorf("all(empty): got %v, want true", v)
	}
}

func TestList_Any_AnyTrue(t *testing.T) {
	// any(x IN [1,2,3] WHERE x > 2) → true
	lc := listComp("x", listLit(intLit(1), intLit(2), intLit(3)),
		binary(varExpr("x"), ">", intLit(2)), nil)
	v := evalWithReg(t, quantifierFn("any", lc), nil, nil)
	if v != expr.BoolValue(true) {
		t.Errorf("any > 2: got %v, want true", v)
	}
}

func TestList_Any_AllFalse(t *testing.T) {
	// any(x IN [1,2,3] WHERE x > 10) → false
	lc := listComp("x", listLit(intLit(1), intLit(2), intLit(3)),
		binary(varExpr("x"), ">", intLit(10)), nil)
	v := evalWithReg(t, quantifierFn("any", lc), nil, nil)
	if v != expr.BoolValue(false) {
		t.Errorf("any > 10: got %v, want false", v)
	}
}

func TestList_Any_EmptyList(t *testing.T) {
	// any(x IN [] WHERE x > 0) → false
	lc := listComp("x", listLit(),
		binary(varExpr("x"), ">", intLit(0)), nil)
	v := evalWithReg(t, quantifierFn("any", lc), nil, nil)
	if v != expr.BoolValue(false) {
		t.Errorf("any(empty): got %v, want false", v)
	}
}

func TestList_None_AllFalse(t *testing.T) {
	// none(x IN [1,2,3] WHERE x > 10) → true
	lc := listComp("x", listLit(intLit(1), intLit(2), intLit(3)),
		binary(varExpr("x"), ">", intLit(10)), nil)
	v := evalWithReg(t, quantifierFn("none", lc), nil, nil)
	if v != expr.BoolValue(true) {
		t.Errorf("none > 10: got %v, want true", v)
	}
}

func TestList_None_OneTrue(t *testing.T) {
	// none(x IN [1,2,3] WHERE x > 2) → false
	lc := listComp("x", listLit(intLit(1), intLit(2), intLit(3)),
		binary(varExpr("x"), ">", intLit(2)), nil)
	v := evalWithReg(t, quantifierFn("none", lc), nil, nil)
	if v != expr.BoolValue(false) {
		t.Errorf("none > 2: got %v, want false", v)
	}
}

func TestList_Single_ExactlyOne(t *testing.T) {
	// single(x IN [1,2,3] WHERE x = 2) → true
	lc := listComp("x", listLit(intLit(1), intLit(2), intLit(3)),
		binary(varExpr("x"), "=", intLit(2)), nil)
	v := evalWithReg(t, quantifierFn("single", lc), nil, nil)
	if v != expr.BoolValue(true) {
		t.Errorf("single = 2: got %v, want true", v)
	}
}

func TestList_Single_MultipleMatches(t *testing.T) {
	// single(x IN [2,2,3] WHERE x = 2) → false
	lc := listComp("x", listLit(intLit(2), intLit(2), intLit(3)),
		binary(varExpr("x"), "=", intLit(2)), nil)
	v := evalWithReg(t, quantifierFn("single", lc), nil, nil)
	if v != expr.BoolValue(false) {
		t.Errorf("single with 2 matches: got %v, want false", v)
	}
}

func TestList_Single_NoMatch(t *testing.T) {
	// single(x IN [1,2,3] WHERE x = 9) → false
	lc := listComp("x", listLit(intLit(1), intLit(2), intLit(3)),
		binary(varExpr("x"), "=", intLit(9)), nil)
	v := evalWithReg(t, quantifierFn("single", lc), nil, nil)
	if v != expr.BoolValue(false) {
		t.Errorf("single no match: got %v, want false", v)
	}
}

func TestList_Quantifier_NullSource(t *testing.T) {
	lc := listComp("x", nullLit(),
		binary(varExpr("x"), ">", intLit(0)), nil)
	for _, name := range []string{"all", "any", "none", "single"} {
		v := evalWithReg(t, quantifierFn(name, lc), nil, nil)
		if !expr.IsNull(v) {
			t.Errorf("%s(null): got %v, want null", name, v)
		}
	}
}
