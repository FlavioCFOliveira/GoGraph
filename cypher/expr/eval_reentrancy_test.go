package expr

// eval_reentrancy_test.go — regression tests for the nesting contract of
// [EvalWith], pinned when the per-evaluation state stopped travelling inside the
// RowContext under a reserved sentinel key and became an explicit parameter
// (#2653).
//
// The contract these tests pin is not new; it is what the sentinel form
// implemented with an explicit `prev, had := row[key]` save and restore around
// each call, and what the parameter form must reproduce rather than assume away.
// It has three parts:
//
//  1. A nested EvalWith gets its OWN context and evaluators. The outer call's
//     context must not be visible to it.
//  2. The nested call's context and evaluators must not be visible to the OUTER
//     call after it returns — the old failure mode, had the restore been
//     dropped, was precisely an inner binding left behind in the shared map.
//  3. The nested call gets a FRESH budget; what it materialises is not debited
//     from the outer evaluation's ceiling.
//
// Re-entrancy is reachable in production: a pattern comprehension re-enters
// [EvalWith] through [PatternEvaluator.EvalPatternComp] with the inner row, and
// an EXISTS { … } / COUNT { … } inside that comprehension re-enters again
// through [SubqueryEvaluator]. These tests drive that nesting directly.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// reentrantKey is a private context-key type so the values these tests plant
// cannot collide with any other package's keys.
type reentrantKey string

const reentrantMark = reentrantKey("eval-depth")

// reentrantSubEval re-enters [EvalWith] from inside EvalExists on its FIRST
// call, with a different context and a different evaluator, and records the
// context it was handed on every call. The second call is the observation: if
// the nested evaluation's context had leaked into the outer one, the second
// call would see the inner mark instead of the outer one.
type reentrantSubEval struct {
	calls    int
	gotMarks []any
	// inner is the nested evaluation's evaluator; it must never be the one the
	// outer call dispatches to.
	inner *reentrantSubEval
	// nested is the expression the first call evaluates re-entrantly.
	nested ast.Expression
	reg    FunctionRegistry
	// nestedErr records an error raised by the nested evaluation, so a failure
	// there is reported rather than swallowed into a passing assertion.
	nestedErr error
}

func (r *reentrantSubEval) EvalExists(ctx context.Context, _ *ast.ExistsSubquery, row RowContext, params map[string]Value) (Value, error) {
	r.calls++
	r.gotMarks = append(r.gotMarks, ctx.Value(reentrantMark))
	if r.calls == 1 && r.nested != nil {
		// Re-enter with a DIFFERENT context and a DIFFERENT evaluator, exactly
		// as a pattern comprehension does when it evaluates its projection.
		innerCtx := context.WithValue(context.Background(), reentrantMark, "inner")
		if _, err := EvalWith(innerCtx, r.nested, row, params, r.reg, r.inner, nil); err != nil {
			r.nestedErr = err
		}
	}
	return BoolValue(true), nil
}

func (r *reentrantSubEval) EvalCount(ctx context.Context, _ *ast.CountSubquery, _ RowContext, _ map[string]Value) (Value, error) {
	r.calls++
	r.gotMarks = append(r.gotMarks, ctx.Value(reentrantMark))
	return IntegerValue(1), nil
}

// TestEvalWith_NestedSubqueryDoesNotLeakContext drives a subquery inside a
// subquery and asserts the inner evaluation's context and evaluator are not
// observable by the outer one once the nested call returns.
func TestEvalWith_NestedSubqueryDoesNotLeakContext(t *testing.T) {
	t.Parallel()
	pos := ast.Position{}
	reg := nopReg{}

	// The inner evaluator, dispatched only by the nested EvalWith.
	inner := &reentrantSubEval{}
	// The nested expression: one EXISTS, evaluated under the inner context.
	nested := &ast.ExistsSubquery{Pos: pos}

	outer := &reentrantSubEval{inner: inner, nested: nested, reg: reg}

	// A list literal evaluates BOTH elements, so the outer evaluator is
	// dispatched twice: once before the re-entry (which it performs) and once
	// after it has returned. The second dispatch is what proves the inner
	// context did not survive into the outer evaluation.
	e := &ast.ListLiteral{Pos: pos, Elements: []ast.Expression{
		&ast.ExistsSubquery{Pos: pos},
		&ast.ExistsSubquery{Pos: pos},
	}}

	outerCtx := context.WithValue(context.Background(), reentrantMark, "outer")
	row := RowContext{"x": IntegerValue(1)}
	got, err := EvalWith(outerCtx, e, row, nil, reg, outer, nil)
	if err != nil {
		t.Fatalf("EvalWith: %v", err)
	}
	if outer.nestedErr != nil {
		t.Fatalf("nested EvalWith: %v", outer.nestedErr)
	}
	if lv, ok := got.(ListValue); !ok || len(lv) != 2 {
		t.Fatalf("result = %v; want a 2-element list", got)
	}

	// (1) The nested call reached the INNER evaluator, under the INNER context.
	if inner.calls != 1 {
		t.Fatalf("inner evaluator dispatched %d times; want exactly 1", inner.calls)
	}
	if len(inner.gotMarks) != 1 || inner.gotMarks[0] != "inner" {
		t.Errorf("nested evaluation saw context mark %v; want \"inner\"", inner.gotMarks)
	}

	// (2) The outer evaluator was dispatched twice and saw the OUTER context
	// BOTH times — including the dispatch that happened after the nested call
	// returned. A leak would show as "inner" in the second position.
	if outer.calls != 2 {
		t.Fatalf("outer evaluator dispatched %d times; want exactly 2", outer.calls)
	}
	for i, m := range outer.gotMarks {
		if m != "outer" {
			t.Errorf("outer dispatch %d saw context mark %v; want \"outer\" (the nested context leaked)", i, m)
		}
	}

	// (3) The caller's row is unchanged: nothing was written into it and left
	// behind. Before #2653 the state rode in this very map, and the restore that
	// kept this true was hand-written.
	if len(row) != 1 || row["x"] != IntegerValue(1) {
		t.Errorf("caller's row was mutated: %v; want exactly {x:1}", row)
	}
}

// budgetProbeSubEval re-enters [EvalWith] and makes the nested evaluation
// materialise a list of a known size, so the test can check that the charge
// landed on the nested call's budget and not on the outer one's.
type budgetProbeSubEval struct {
	reg       FunctionRegistry
	elems     int
	nestedErr error
}

func (b *budgetProbeSubEval) EvalExists(ctx context.Context, _ *ast.ExistsSubquery, row RowContext, params map[string]Value) (Value, error) {
	// A list literal of b.elems elements charges b.elems against whichever
	// budget the nested evaluation is using.
	pos := ast.Position{}
	list := &ast.ListLiteral{Pos: pos}
	for i := 0; i < b.elems; i++ {
		list.Elements = append(list.Elements, &ast.IntLiteral{Pos: pos, Value: int64(i)})
	}
	lc := &ast.ListComprehension{
		Pos:      pos,
		Variable: "z",
		Source:   list,
	}
	if _, err := EvalWith(ctx, lc, row, params, b.reg, nil, nil); err != nil {
		b.nestedErr = err
	}
	return BoolValue(true), nil
}

func (*budgetProbeSubEval) EvalCount(_ context.Context, _ *ast.CountSubquery, _ RowContext, _ map[string]Value) (Value, error) {
	return IntegerValue(0), nil
}

// TestEvalWith_NestedEvaluationGetsFreshBudget asserts a nested [EvalWith] is
// charged against its OWN budget, leaving the outer evaluation's ceiling
// untouched. This is white-box: it builds the outer state itself so it can read
// the remaining budget after the nested call has returned, which is the only way
// to observe the property without materialising DefaultMaxListElements values.
func TestEvalWith_NestedEvaluationGetsFreshBudget(t *testing.T) {
	t.Parallel()
	pos := ast.Position{}
	reg := nopReg{}
	const nested = 64

	sub := &budgetProbeSubEval{reg: reg, elems: nested}
	st := &evalCallState{
		ctx: context.Background(),
		sub: sub,
		budget: evalBudget{
			remaining:      DefaultMaxListElements,
			limit:          DefaultMaxListElements,
			bytesRemaining: DefaultMaxStringEvalBytes,
			bytesLimit:     DefaultMaxStringEvalBytes,
		},
	}

	if _, err := evalExpr(&ast.ExistsSubquery{Pos: pos}, RowContext{}, st, nil, reg); err != nil {
		t.Fatalf("evalExpr: %v", err)
	}
	if sub.nestedErr != nil {
		t.Fatalf("nested EvalWith: %v", sub.nestedErr)
	}
	if st.budget.remaining != DefaultMaxListElements {
		t.Errorf("outer budget remaining = %d; want %d — the nested evaluation debited the outer ceiling",
			st.budget.remaining, DefaultMaxListElements)
	}

	// The same evaluation charged against the OUTER budget must debit it, so the
	// assertion above is not passing merely because nothing charges at all.
	if err := chargeListGrowth(st, nested); err != nil {
		t.Fatalf("chargeListGrowth: %v", err)
	}
	if st.budget.remaining != DefaultMaxListElements-nested {
		t.Errorf("outer budget remaining = %d after charging %d; want %d — the budget is not being debited at all",
			st.budget.remaining, nested, DefaultMaxListElements-nested)
	}
}
