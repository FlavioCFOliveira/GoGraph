package cypher

// subquery_eval.go — runtime implementation of [expr.SubqueryEvaluator] that
// bridges expression-level EXISTS { … } / COUNT { … } references to the
// engine's IR translator and physical builder (task-396).
//
// # Overview
//
// EXISTS { … } and COUNT { … } subqueries occur as expressions inside WHERE
// predicates, CASE branches, RETURN projection items, and arbitrarily nested
// composite expressions. They cannot in general be lifted to a top-level
// SemiApply / RollUpApply rewrite because their result is consumed by a
// surrounding scalar or boolean operator. The expression evaluator therefore
// dispatches every subquery occurrence to a [expr.SubqueryEvaluator]
// implementation that:
//
//  1. Builds the subquery's inner plan once per outer query (lazy compilation
//     keyed by the AST pointer);
//  2. For each evaluation, projects the current outer [expr.RowContext] onto
//     the schema layout the inner plan expects, then drives the inner plan
//     end-to-end per openCypher semantics:
//       - EXISTS yields [expr.BoolValue](true) iff the inner plan produces
//         at least one row, BoolValue(false) when zero rows;
//       - COUNT yields [expr.IntegerValue] equal to the exact row count
//         produced by the inner plan (0 on empty).
//
// # Correlation
//
// Outer-scope variables visible to the subquery are projected from the outer
// RowContext into a synthetic Row laid out per the subquery's compiled
// schema. The inner plan's leading [exec.Argument] receives this Row on every
// evaluation (one re-init per outer row), preserving correlation semantics
// without leaking inner-scope variables back to the outer plan.
//
// # Concurrency
//
// subqueryEvaluator is NOT safe for concurrent use. Each query execution
// constructs its own instance; the engine's Run/RunInTx entry points are
// the natural ownership boundaries.

import (
	"context"
	"fmt"

	"gograph/cypher/ast"
	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/cypher/ir"
	"gograph/graph"
	lpg "gograph/graph/lpg"
)

// subqueryEvaluator implements [expr.SubqueryEvaluator] by compiling each
// subquery's AST to a physical operator on first use and reusing the compiled
// pipeline across outer rows. The compiled pipeline's leaf [exec.Argument]
// is seeded with the current outer row before every drive call.
//
// subqueryEvaluator is NOT safe for concurrent use; each engine Run/RunInTx
// invocation owns its own instance.
type subqueryEvaluator struct {
	walker nodeWalkerIface
	labels labelResolverIface
	reg    expr.FunctionRegistry
	g      *lpg.Graph[string, float64]

	// compiled caches the per-AST compiled subquery so the inner plan is
	// translated and physically built at most once per outer query. The key is
	// the unique AST pointer; the value carries the seedable Argument and the
	// schema layout used to materialise the per-row Row.
	compiled map[ast.Expression]*compiledSubquery

	// closed tracks AST roots whose compiled pipeline has been closed by an
	// error path so subsequent drives can re-init cleanly.
	_ struct{}
}

// compiledSubquery bundles the runtime state of a single compiled subquery:
// the entry operator, the leaf Argument that receives the per-row seed, and
// the variable→column schema layout used to materialise that seed.
type compiledSubquery struct {
	op  exec.Operator
	arg *exec.Argument
	// outerVars is the ordered list of outer-scope variables that the inner
	// plan correlates against. Position i in outerVars corresponds to column
	// i in the seed Row that the inner Argument re-emits.
	outerVars []string
}

// newSubqueryEvaluator constructs the evaluator for one query run. The caller
// supplies every dependency the subquery's compiled pipeline may need.
func newSubqueryEvaluator(walker nodeWalkerIface, labels labelResolverIface, reg expr.FunctionRegistry, g *lpg.Graph[string, float64]) *subqueryEvaluator {
	return &subqueryEvaluator{
		walker:   walker,
		labels:   labels,
		reg:      reg,
		g:        g,
		compiled: make(map[ast.Expression]*compiledSubquery),
	}
}

// EvalExists implements [expr.SubqueryEvaluator]. It drives the compiled
// inner plan against the seeded outer row and reports whether any row was
// produced. The inner plan is closed early once a row is observed.
func (e *subqueryEvaluator) EvalExists(ctx context.Context, sub *ast.ExistsSubquery, row expr.RowContext, _ map[string]expr.Value) (expr.Value, error) {
	cs, err := e.compileExists(sub, row)
	if err != nil {
		return nil, err
	}
	hasRow, err := e.driveOne(ctx, cs, row)
	if err != nil {
		return nil, err
	}
	return expr.BoolValue(hasRow), nil
}

// EvalCount implements [expr.SubqueryEvaluator]. It drives the compiled inner
// plan to completion and counts the rows it emitted. The count is reported as
// an [expr.IntegerValue]; zero rows yield IntegerValue(0).
func (e *subqueryEvaluator) EvalCount(ctx context.Context, sub *ast.CountSubquery, row expr.RowContext, _ map[string]expr.Value) (expr.Value, error) {
	cs, err := e.compileCount(sub, row)
	if err != nil {
		return nil, err
	}
	count, err := e.driveAll(ctx, cs, row)
	if err != nil {
		return nil, err
	}
	return expr.IntegerValue(count), nil
}

// compileExists returns the compiled pipeline for sub, building it on first
// use. The current outer RowContext is used solely to enumerate the
// correlation variable set the inner plan will see; subsequent evaluations
// reuse the same compiled pipeline regardless of the outer row's contents.
func (e *subqueryEvaluator) compileExists(sub *ast.ExistsSubquery, row expr.RowContext) (*compiledSubquery, error) {
	if cs, ok := e.compiled[sub]; ok {
		return cs, nil
	}
	innerAST := existsToSingleQuery(sub)
	cs, err := e.compileSubAST(innerAST, row)
	if err != nil {
		return nil, fmt.Errorf("compile EXISTS subquery: %w", err)
	}
	e.compiled[sub] = cs
	return cs, nil
}

// compileCount returns the compiled pipeline for sub, building it on first
// use. See [compileExists] for the schema convention.
func (e *subqueryEvaluator) compileCount(sub *ast.CountSubquery, row expr.RowContext) (*compiledSubquery, error) {
	if cs, ok := e.compiled[sub]; ok {
		return cs, nil
	}
	innerAST := countToSingleQuery(sub)
	cs, err := e.compileSubAST(innerAST, row)
	if err != nil {
		return nil, fmt.Errorf("compile COUNT subquery: %w", err)
	}
	e.compiled[sub] = cs
	return cs, nil
}

// compileSubAST is the common compile pipeline for EXISTS and COUNT. It
// translates innerAST to an [ir.LogicalPlan] rooted at a synthetic Argument
// leaf carrying the outer-scope correlation variables, then physically builds
// the plan into an [exec.Operator].
func (e *subqueryEvaluator) compileSubAST(innerAST *ast.SingleQuery, row expr.RowContext) (*compiledSubquery, error) {
	// Collect the outer-scope variables in deterministic order. Stable order
	// matters: column i in the seed Row must map to the same variable on every
	// drive call.
	outerVars := outerVarsFromRow(row)

	// Build the inner plan with an Argument leaf carrying the correlation
	// vars. The Argument's Tag is shared with the seed exec.Argument we
	// register under argByTag below so the physical builder routes the seed
	// instance to the IR leaf.
	tag := ir.NextArgTag()
	innerPlan, err := ir.TranslateSubquery(innerAST, outerVars, tag)
	if err != nil {
		return nil, fmt.Errorf("translate inner: %w", err)
	}

	// Build the physical pipeline. Pre-register the seed Argument so the
	// IR Argument leaf carrying tag resolves to the same exec.Argument
	// instance.
	seed := exec.NewArgument()
	argByTag := map[uint32]*exec.Argument{tag: seed}
	schema := make(map[string]int, len(outerVars))
	for i, v := range outerVars {
		schema[v] = i
	}

	op, err := buildOperator(innerPlan, e.walker, e.labels, e.reg, nil, schema, nil, nil, argByTag, nil)
	if err != nil {
		return nil, fmt.Errorf("build inner operator: %w", err)
	}
	return &compiledSubquery{
		op:        op,
		arg:       seed,
		outerVars: outerVars,
	}, nil
}

// driveOne seeds the inner argument and pulls at most one row, returning
// (true, nil) when one was produced and (false, nil) when the inner plan was
// empty. The inner plan is closed (or short-circuit-finalised) before
// returning so resources do not leak across outer rows.
func (e *subqueryEvaluator) driveOne(ctx context.Context, cs *compiledSubquery, row expr.RowContext) (bool, error) {
	if err := e.prepareDrive(ctx, cs, row); err != nil {
		return false, err
	}
	var dummy exec.Row
	ok, err := cs.op.Next(&dummy)
	if err != nil {
		_ = cs.op.Close()
		return false, fmt.Errorf("EXISTS inner drive: %w", err)
	}
	if closeErr := cs.op.Close(); closeErr != nil {
		return false, fmt.Errorf("EXISTS inner close: %w", closeErr)
	}
	return ok, nil
}

// driveAll drives the inner plan to completion and counts the rows it
// emitted. The inner plan is closed before returning.
func (e *subqueryEvaluator) driveAll(ctx context.Context, cs *compiledSubquery, row expr.RowContext) (int64, error) {
	if err := e.prepareDrive(ctx, cs, row); err != nil {
		return 0, err
	}
	var count int64
	for {
		var r exec.Row
		ok, err := cs.op.Next(&r)
		if err != nil {
			_ = cs.op.Close()
			return 0, fmt.Errorf("COUNT inner drive: %w", err)
		}
		if !ok {
			break
		}
		count++
	}
	if err := cs.op.Close(); err != nil {
		return 0, fmt.Errorf("COUNT inner close: %w", err)
	}
	return count, nil
}

// prepareDrive projects the outer RowContext onto the seed Row, re-seeds the
// inner Argument, and (re)initialises the inner operator pipeline.
func (e *subqueryEvaluator) prepareDrive(ctx context.Context, cs *compiledSubquery, row expr.RowContext) error {
	seedRow := make(exec.Row, len(cs.outerVars))
	for i, v := range cs.outerVars {
		// Downgrade NodeValue / RelValue back to IntegerValue(ID) so the inner
		// plan's scan/expand operators see the same NodeID layout they expect.
		seedRow[i] = downgradeForRow(row[v])
	}
	cs.arg.SetOuterRow(seedRow)
	if err := cs.op.Init(ctx); err != nil {
		return fmt.Errorf("subquery init: %w", err)
	}
	return nil
}

// outerVarsFromRow returns the variable names present in row, in
// deterministic order, excluding the smuggled subquery-context sentinel.
func outerVarsFromRow(row expr.RowContext) []string {
	out := make([]string, 0, len(row))
	for k := range row {
		if k == subqueryContextRowKey {
			continue
		}
		out = append(out, k)
	}
	// Sort for deterministic seed-row layout. The compiled schema map mirrors
	// this order via the index assigned in compileSubAST.
	sortStrings(out)
	return out
}

// subqueryContextRowKey mirrors expr.subqueryContextKey for filtering during
// outer-variable enumeration. The constant is duplicated here because the
// expr-side key is unexported; both definitions use the same NUL-bracketed
// string and any drift would surface as the subquery seeing a synthetic
// variable named "subquery-context".
const subqueryContextRowKey = "\x00subquery-context\x00"

// downgradeForRow converts a NodeValue or RelValue back to the IntegerValue
// representation expected by inner scans and expands. Other values pass
// through unchanged.
func downgradeForRow(v expr.Value) expr.Value {
	if v == nil {
		return expr.Null
	}
	switch t := v.(type) {
	case expr.NodeValue:
		return expr.IntegerValue(int64(t.ID))
	case expr.RelationshipValue:
		return expr.IntegerValue(int64(t.ID))
	default:
		return v
	}
}

// existsToSingleQuery normalises sub to a *ast.SingleQuery suitable for the
// translator. The pattern form is wrapped in a synthetic MATCH so the same
// translation path handles both forms uniformly.
func existsToSingleQuery(sub *ast.ExistsSubquery) *ast.SingleQuery {
	if sub.Query != nil {
		return sub.Query
	}
	return &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: sub.Pattern},
		},
	}
}

// countToSingleQuery is the COUNT counterpart of [existsToSingleQuery].
func countToSingleQuery(sub *ast.CountSubquery) *ast.SingleQuery {
	if sub.Query != nil {
		return sub.Query
	}
	return &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: sub.Pattern},
		},
	}
}

// sortStrings is a tiny in-place ascending sort used by outerVarsFromRow.
// Stable, single-pass insertion sort is sufficient for the small lists we
// see in practice (typically 1–8 outer vars) and avoids the import of the
// sort package — keeping this file's dependency surface minimal.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// graph package alias to avoid unused-import error when the type assertions
// inside downgradeForRow are removed by a future refactor.
var _ = graph.NodeID(0)

// Suppress unused-parameter lint for params in the public API surface; the
// expression evaluator passes params to subqueries even when they happen to
// reference none, and the prepareDrive path may grow to forward them when
// parameter-driven subqueries are supported in a future task.
var _ = func(_ map[string]expr.Value) {}
