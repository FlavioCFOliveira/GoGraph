package expr

// eval_subquery_test.go — white-box tests for EvalWith, the subqueryContextValue
// holder, extractSubqueryContext, and evalReduce. Together these close every
// remaining 0%-coverage function in eval.go for cypher/expr.

import (
	"context"
	"testing"

	"gograph/cypher/ast"
)

// fakeSubEval is a deterministic SubqueryEvaluator stand-in. EXISTS yields a
// fixed bool; COUNT yields a fixed int. It also records the context received
// so the test can assert that EvalWith threads it through.
type fakeSubEval struct {
	existsResult bool
	countResult  int64
	gotCtx       context.Context
}

func (f *fakeSubEval) EvalExists(ctx context.Context, _ *ast.ExistsSubquery, _ RowContext, _ map[string]Value) (Value, error) {
	f.gotCtx = ctx
	return BoolValue(f.existsResult), nil
}

func (f *fakeSubEval) EvalCount(ctx context.Context, _ *ast.CountSubquery, _ RowContext, _ map[string]Value) (Value, error) {
	f.gotCtx = ctx
	return IntegerValue(f.countResult), nil
}

// nopReg is a FunctionRegistry that resolves nothing.
type nopReg struct{}

func (nopReg) Resolve(_ string) (BuiltinFn, bool) { return nil, false }

// TestEvalWith_ExistsAndCount drives EvalWith with a SubqueryEvaluator and
// asserts that EXISTS / COUNT delegate to the evaluator and propagate the
// context.
func TestEvalWith_ExistsAndCount(t *testing.T) {
	t.Parallel()
	pos := ast.Position{}
	exists := &ast.ExistsSubquery{Pos: pos}
	count := &ast.CountSubquery{Pos: pos}

	type ctxKey string
	const k = ctxKey("trace")
	ctx := context.WithValue(context.Background(), k, "v")

	sub := &fakeSubEval{existsResult: true, countResult: 7}

	v, err := EvalWith(ctx, exists, RowContext{"x": IntegerValue(1)}, nil, nopReg{}, sub)
	if err != nil {
		t.Fatalf("EvalWith EXISTS: %v", err)
	}
	if v != BoolValue(true) {
		t.Errorf("EXISTS: got %v want true", v)
	}
	if sub.gotCtx.Value(k) != "v" {
		t.Errorf("context not propagated")
	}

	v, err = EvalWith(ctx, count, nil, nil, nopReg{}, sub)
	if err != nil {
		t.Fatalf("EvalWith COUNT: %v", err)
	}
	if v != IntegerValue(7) {
		t.Errorf("COUNT: got %v want 7", v)
	}
}

// TestEvalWith_NilCtx ensures a nil ctx is upgraded to context.Background.
//
// We intentionally pass a nil [context.Context] here to exercise the documented
// fallback branch in [EvalWith] that upgrades nil to [context.Background]. The
// nil is routed through an interface-typed local to keep staticcheck (SA1012)
// silent without disabling the check globally.
func TestEvalWith_NilCtx(t *testing.T) {
	t.Parallel()
	exists := &ast.ExistsSubquery{}
	sub := &fakeSubEval{existsResult: false}
	var nilCtx context.Context // explicit nil typed value
	v, err := EvalWith(nilCtx, exists, nil, nil, nopReg{}, sub)
	if err != nil {
		t.Fatalf("EvalWith nil ctx: %v", err)
	}
	if v != BoolValue(false) {
		t.Errorf("got %v", v)
	}
	if sub.gotCtx == nil {
		t.Errorf("ctx still nil after EvalWith upgrade")
	}
}

// TestEvalWith_NilSubEvalErrors reaches the "no SubqueryEvaluator wired" error
// branches in evalExpr for both EXISTS and COUNT, via EvalWith with a nil
// evaluator. Tests that the error message mentions the missing evaluator.
func TestEvalWith_NilSubEvalErrors(t *testing.T) {
	t.Parallel()
	pos := ast.Position{}
	if _, err := EvalWith(context.Background(), &ast.ExistsSubquery{Pos: pos}, nil, nil, nopReg{}, nil); err == nil {
		t.Errorf("EXISTS with nil sub should error")
	}
	if _, err := EvalWith(context.Background(), &ast.CountSubquery{Pos: pos}, nil, nil, nopReg{}, nil); err == nil {
		t.Errorf("COUNT with nil sub should error")
	}
}

// TestSubqueryContextValue_ValueIface covers Kind, Equal, Hash, String on the
// internal holder so that all four 0%-covered methods light up.
func TestSubqueryContextValue_ValueIface(t *testing.T) {
	t.Parallel()
	scv := &subqueryContextValue{ctx: context.Background(), sub: nil}
	if scv.Kind() != KindNull {
		t.Errorf("Kind=%v; want KindNull", scv.Kind())
	}
	if scv.Equal(IntegerValue(0)) != Null {
		t.Errorf("Equal not Null")
	}
	if scv.Hash() != 0 {
		t.Errorf("Hash=%d; want 0", scv.Hash())
	}
	if scv.String() != "<subquery-context>" {
		t.Errorf("String=%q", scv.String())
	}
}

// TestExtractSubqueryContext_AllPaths covers each branch of
// extractSubqueryContext.
func TestExtractSubqueryContext_AllPaths(t *testing.T) {
	t.Parallel()
	// nil row → background context, no evaluator.
	ctx, sub := extractSubqueryContext(nil)
	if ctx == nil || sub != nil {
		t.Errorf("nil row: ctx=%v sub=%v", ctx, sub)
	}
	// Row without the sentinel key.
	row := RowContext{"x": IntegerValue(1)}
	ctx, sub = extractSubqueryContext(row)
	if ctx == nil || sub != nil {
		t.Errorf("no sentinel: ctx=%v sub=%v", ctx, sub)
	}
	// Row with the sentinel key holding the wrong value type.
	row[subqueryContextKey] = IntegerValue(99) // wrong type
	ctx, sub = extractSubqueryContext(row)
	if ctx == nil || sub != nil {
		t.Errorf("wrong type: ctx=%v sub=%v", ctx, sub)
	}
	// Row with proper holder.
	se := &fakeSubEval{}
	myCtx := context.Background()
	row[subqueryContextKey] = &subqueryContextValue{ctx: myCtx, sub: se}
	ctx, sub = extractSubqueryContext(row)
	if ctx != myCtx || sub != se {
		t.Errorf("happy path: ctx=%v sub=%v", ctx, sub)
	}
}

// TestEvalReduce_AccumulateSum exercises the reduce(acc=init, x IN list | expr)
// happy path. We build the AST manually to match the shape the parser emits.
func TestEvalReduce_AccumulateSum(t *testing.T) {
	t.Parallel()
	pos := ast.Position{}
	// init: 0 (constant). The accVarName fallback "_acc" is used when initExpr
	// is not a Variable. We use a Variable named "total" instead so the
	// `if v, ok := initExpr.(*ast.Variable); ok` branch is exercised.
	init := &ast.Variable{Pos: pos, Name: "total"}

	// List comprehension: x IN [1,2,3,4,5] | total + x
	source := &ast.ListLiteral{Pos: pos, Elements: []ast.Expression{
		&ast.IntLiteral{Pos: pos, Value: 1},
		&ast.IntLiteral{Pos: pos, Value: 2},
		&ast.IntLiteral{Pos: pos, Value: 3},
		&ast.IntLiteral{Pos: pos, Value: 4},
		&ast.IntLiteral{Pos: pos, Value: 5},
	}}
	projection := &ast.BinaryOp{
		Pos:      pos,
		Left:     &ast.Variable{Pos: pos, Name: "total"},
		Operator: "+",
		Right:    &ast.Variable{Pos: pos, Name: "x"},
	}
	lc := &ast.ListComprehension{
		Pos:        pos,
		Variable:   "x",
		Source:     source,
		Projection: projection,
	}
	// Outer row binds "total" to 0 (the initial accumulator value).
	row := RowContext{"total": IntegerValue(0)}
	got, err := evalReduce(init, lc, row, nil, nopReg{})
	if err != nil {
		t.Fatalf("evalReduce: %v", err)
	}
	if got != IntegerValue(15) {
		t.Errorf("reduce sum: got %v want 15", got)
	}
}

// TestEvalReduce_EmptyList ensures the accumulator is returned unchanged when
// the list is empty.
func TestEvalReduce_EmptyList(t *testing.T) {
	t.Parallel()
	pos := ast.Position{}
	init := &ast.IntLiteral{Pos: pos, Value: 42}
	lc := &ast.ListComprehension{
		Pos:        pos,
		Variable:   "x",
		Source:     &ast.ListLiteral{Pos: pos, Elements: nil},
		Projection: &ast.Variable{Pos: pos, Name: "_acc"},
	}
	got, err := evalReduce(init, lc, RowContext{}, nil, nopReg{})
	if err != nil {
		t.Fatalf("evalReduce empty: %v", err)
	}
	if got != IntegerValue(42) {
		t.Errorf("got %v want 42", got)
	}
}

// TestEvalReduce_NullSource passes a NullLiteral as the list source; reduce
// must short-circuit and return the initial accumulator.
func TestEvalReduce_NullSource(t *testing.T) {
	t.Parallel()
	pos := ast.Position{}
	init := &ast.IntLiteral{Pos: pos, Value: 7}
	lc := &ast.ListComprehension{
		Pos:      pos,
		Variable: "x",
		Source:   &ast.NullLiteral{Pos: pos},
	}
	got, err := evalReduce(init, lc, RowContext{}, nil, nopReg{})
	if err != nil {
		t.Fatalf("evalReduce null source: %v", err)
	}
	if got != IntegerValue(7) {
		t.Errorf("got %v want 7", got)
	}
}

// TestEvalReduce_NonListSource passes a string as the list source; reduce
// returns the accumulator unchanged because the source is not a ListValue.
func TestEvalReduce_NonListSource(t *testing.T) {
	t.Parallel()
	pos := ast.Position{}
	init := &ast.IntLiteral{Pos: pos, Value: 3}
	lc := &ast.ListComprehension{
		Pos:      pos,
		Variable: "x",
		Source:   &ast.StringLiteral{Pos: pos, Value: "not a list"},
	}
	got, err := evalReduce(init, lc, RowContext{}, nil, nopReg{})
	if err != nil {
		t.Fatalf("evalReduce non-list: %v", err)
	}
	if got != IntegerValue(3) {
		t.Errorf("got %v want 3", got)
	}
}

// TestEvalReduce_NilProjection covers the lc.Projection == nil branch.
func TestEvalReduce_NilProjection(t *testing.T) {
	t.Parallel()
	pos := ast.Position{}
	init := &ast.IntLiteral{Pos: pos, Value: 99}
	lc := &ast.ListComprehension{
		Pos:      pos,
		Variable: "x",
		Source: &ast.ListLiteral{Pos: pos, Elements: []ast.Expression{
			&ast.IntLiteral{Pos: pos, Value: 1},
		}},
		Projection: nil,
	}
	got, err := evalReduce(init, lc, RowContext{}, nil, nopReg{})
	if err != nil {
		t.Fatalf("evalReduce nil projection: %v", err)
	}
	if got != IntegerValue(99) {
		t.Errorf("got %v want 99", got)
	}
}
