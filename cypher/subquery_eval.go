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

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	lpg "github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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
	g      *lpg.ReadView[string, float64]

	// compiled caches the per-AST compiled subquery so the inner plan is
	// translated and physically built at most once per outer query. The key is
	// the unique AST pointer; the value carries the seedable Argument and the
	// schema layout used to materialise the per-row Row.
	//
	// nil until the first subquery is compiled — see [subqueryEvaluator.init].
	compiled map[ast.Expression]*compiledSubquery

	// degree caches the per-AST degree-rewrite verdict (rmp #2232). A present
	// non-nil value is a recognised degree-answerable pattern that answers from
	// the adjacency instead of driving an inner plan; a present nil value is a
	// pattern already examined and rejected, so the recogniser runs at most once
	// per subquery occurrence rather than once per outer row.
	//
	// nil until the first verdict is recorded — see [subqueryEvaluator.init].
	degree map[ast.Expression]*degreeShape

	// labelledHop caches the per-AST verdict for the labelled single-hop count
	// (rmp #2235). It is a SEPARATE map from degree because it is a separate
	// recogniser: a pattern rejected by one may be accepted by the other, and
	// sharing a cache would conflate the two verdicts.
	//
	// nil until the first verdict is recorded — see [subqueryEvaluator.init].
	labelledHop map[ast.Expression]*labelledHopShape

	// adjacencyCountsDisabled forbids BOTH adjacency-answered rewrites above, so
	// every EXISTS/COUNT subquery drives its compiled inner plan. It is set from
	// [EngineOptions.DisableAdjacencyCountRewrites]; see that field for what the
	// knob is for and why one flag covers two rewrites.
	//
	// The polarity is NEGATIVE so the zero value keeps both rewrites live: an
	// evaluator constructed without an Engine behaves as it did before the knob
	// existed. A nested subquery inherits the setting for free, because
	// [buildOpts.forSubquery] carries THIS evaluator into the child scope.
	adjacencyCountsDisabled bool

	// params is the enclosing query's fully-resolved parameter map, threaded
	// into every inner build (rmp #2507).
	//
	// It is NOT optional and it is not only about `$name`. [parser.StripLiterals]
	// hoists a string literal written inside a MATCH pattern or a WHERE predicate
	// onto an auto-parameter, so `(:Person {name: 'B'})` reaches the planner as
	// `(:Person {name: $«auto_1»})` — including when it is written inside a
	// subquery. Building the inner plan without the map left every such reference
	// unbound, the comparison evaluated to NULL, and the inner Filter dropped rows
	// that should have matched. That is why the defect looked like "only FILTERED
	// subqueries are wrong": a numeric literal is never hoisted, so a numeric
	// filter had always worked.
	params map[string]expr.Value

	// outer is the enclosing query's build options, the source from which each
	// inner plan's SCOPED CHILD is derived by [buildOpts.forSubquery]. It is read,
	// never mutated.
	//
	// Today it is always set: [buildReadPhysical] is the sole construction site and
	// it calls [subqueryEvaluator.bind] immediately. [buildOpts.forSubquery]
	// nonetheless tolerates a nil receiver, so a future construction site that
	// forgets to bind degrades to the pre-#2507 behaviour — an unresolvable
	// subquery predicate — rather than panicking mid-query.
	outer *buildOpts
}

// bind attaches the enclosing query's parameter map and build options to the
// evaluator. It is called once, after the outer [buildOpts] exists, because that
// struct holds a back-pointer to this evaluator (bopts.subEval) and the two
// therefore cannot be constructed in one step.
//
// Passing the outer buildOpts is what gives a NESTED subquery an evaluator: the
// child that [buildOpts.forSubquery] derives carries this same evaluator in its
// subEval field, so nesting recurses to any depth with a fresh scope each time.
func (e *subqueryEvaluator) bind(params map[string]expr.Value, outer *buildOpts) {
	e.params = params
	e.outer = outer
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
func newSubqueryEvaluator(walker nodeWalkerIface, labels labelResolverIface, reg expr.FunctionRegistry, g *lpg.ReadView[string, float64]) *subqueryEvaluator {
	e := &subqueryEvaluator{}
	e.init(walker, labels, reg, g)
	return e
}

// init sets up an evaluator IN PLACE. It exists so a caller that already owns
// storage for one — [readBuildScaffold], which holds the whole per-execution
// build scaffolding in a single heap object — can initialise it without a second
// allocation. [newSubqueryEvaluator] is this plus the allocation.
//
// The three memo maps are NOT created here. They are created on first write, by
// the three sites that write them, because a query with no EXISTS/COUNT
// subquery — which is most queries — never touches any of them, and three empty
// maps cost three heap allocations on every single execution (rmp #2693: 3 of
// the 33 allocations of a read whose plan is "Project ← LabelCountScan", which
// has no subquery at all). Reading a nil map is legal Go and returns the zero
// value with ok == false, which is exactly the "not memoised yet" answer every
// reader already handles; only the writes need the guard. [patternEvaluator]'s
// own labelledHop map has been built this way since rmp #2235, so this is the
// established shape in this package rather than a new one.
func (e *subqueryEvaluator) init(walker nodeWalkerIface, labels labelResolverIface, reg expr.FunctionRegistry, g *lpg.ReadView[string, float64]) {
	e.walker = walker
	e.labels = labels
	e.reg = reg
	e.g = g
}

// EvalExists implements [expr.SubqueryEvaluator]. It drives the compiled
// inner plan against the seeded outer row and reports whether any row was
// produced. The inner plan is closed early once a row is observed.
func (e *subqueryEvaluator) EvalExists(ctx context.Context, sub *ast.ExistsSubquery, row expr.RowContext, _ map[string]expr.Value) (expr.Value, error) {
	// Degree rewrite (#2232): EXISTS over a degree-answerable pattern is
	// "degree > 0", which the adjacency answers without an inner plan. Capped at
	// 1 — the existence question never needs to know the true degree.
	//
	// Only the subquery node is passed: since rmp #2648 the recogniser's view of
	// the body is derived inside the memo by [subqueryRecogniserBody], so BOTH
	// spellings — pattern form and single-MATCH block form — reach the same
	// recogniser. Reading sub.Pattern here is what made the block form
	// structurally unreachable.
	if sh := e.degreeShapeFor(sub, row); sh != nil {
		if n, ok := sh.count(e.g, row, 1); ok {
			degreeRewriteCount.Add(1)
			return expr.BoolValue(n > 0), nil
		}
	}
	// Labelled single hop (#2235): a label on the far node makes the pattern
	// ineligible for the degree rewrite above — in Neo4j too — but it is still
	// one adjacency walk rather than an inner plan. Capped at 1 for the same
	// reason.
	if n, ok := e.countLabelledHop(sub, row, 1); ok {
		return expr.BoolValue(n > 0), nil
	}
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
	// Degree rewrite (#2232). Uncapped: this entry point owes the caller the
	// true count. A comparison against a literal reaches the bounded form
	// through EvalCountBounded instead, which is where short-circuiting lives.
	//
	// The body's WHERE is still threaded — it is now derived by
	// [subqueryRecogniserBody] inside the memo rather than read from sub here —
	// because an inline WHERE is a Selection neither recogniser can evaluate, and
	// both must refuse the pattern rather than answer it without the predicate
	// (rmp #2242). That holds identically for a block form's `MATCH … WHERE …`
	// since rmp #2648.
	if sh := e.degreeShapeFor(sub, row); sh != nil {
		if n, ok := sh.count(e.g, row, -1); ok {
			degreeRewriteCount.Add(1)
			return expr.IntegerValue(n), nil
		}
	}
	// Labelled single hop (#2235), likewise uncapped here.
	if n, ok := e.countLabelledHop(sub, row, -1); ok {
		return expr.IntegerValue(n), nil
	}
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
	if e.compiled == nil {
		e.compiled = make(map[ast.Expression]*compiledSubquery)
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
	if e.compiled == nil {
		e.compiled = make(map[ast.Expression]*compiledSubquery)
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

	// The inner build receives the enclosing query's PARAMETERS and a SCOPED CHILD
	// of its build options (rmp #2507). Both were nil before, and both had to
	// change together: the parameter map is what makes a hoisted or user-supplied
	// literal resolve, and the child buildOpts is what makes an inner relationship
	// variable hydrate and a nested subquery or pattern predicate find its
	// evaluator. See [buildOpts.forSubquery] for what the child carries and, more
	// importantly, for what it must not.
	op, err := buildOperator(innerPlan, e.walker, e.labels, e.reg, e.params, schema, nil, nil, argByTag, e.outer.forSubquery())
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
// deterministic order.
//
// It used to filter out a reserved sentinel key, because [expr.EvalWith]
// smuggled its per-evaluation state through the RowContext and every row this
// function saw carried it. That state is now an explicit parameter inside the
// evaluator (#2653), so a RowContext holds nothing but real variable bindings
// and the filter — along with the duplicated copy of the sentinel constant that
// this file had to keep in step with cypher/expr — is gone.
func outerVarsFromRow(row expr.RowContext) []string {
	out := make([]string, 0, len(row))
	for k := range row {
		out = append(out, k)
	}
	// Sort for deterministic seed-row layout. The compiled schema map mirrors
	// this order via the index assigned in compileSubAST.
	sortStrings(out)
	return out
}

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
	// Where is load-bearing. Without it the inline predicate of a pattern-form
	// EXISTS was DISCARDED on the expression-evaluated path, so
	// `EXISTS { (a)-[:K]->(b) WHERE b.id = 999 }` returned true whenever the bare
	// pattern matched — a predicate that can never hold, answered true. The
	// WHERE-POSITION spelling was unaffected because the planner lowers it to a
	// SemiApply with its own Selection; only this expression path built the inner
	// MATCH by hand and forgot the clause (rmp #2242).
	return &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: sub.Pattern, Where: sub.Where},
		},
	}
}

// countToSingleQuery is the COUNT counterpart of [existsToSingleQuery], and
// threads Where for the same load-bearing reason (rmp #2242).
func countToSingleQuery(sub *ast.CountSubquery) *ast.SingleQuery {
	if sub.Query != nil {
		return sub.Query
	}
	return &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: sub.Pattern, Where: sub.Where},
		},
	}
}

// subqueryRecogniserBody returns the (pattern, where) pair the two
// adjacency-answered recognisers should see for one subquery occurrence,
// whichever of the two spellings the user wrote (rmp #2648).
//
// It is the INVERSE of [existsToSingleQuery] / [countToSingleQuery] above, and
// deliberately lives beside them: those two turn a pattern form into the
// synthetic `MATCH <pattern> [WHERE …]` body the translator compiles, and this
// turns exactly that body back into the pair the recognisers take. Keeping the
// two adjacent is what makes the round trip checkable — see
// TestPatternFormOf_IsInverseOfDesugaring — rather than a claim.
//
// It exists because [ast.CountSubquery] and [ast.ExistsSubquery] carry the same
// question in two shapes, and every fast path takes an [ast.Pattern]. Neo4j has
// no equivalent function because it has no equivalent problem: its parser emits
// one shape (see [ir.PatternFormOf] for the source citations and for the
// boundary this adopts).
//
// A nil pattern is the "not recognisable" answer, which is already what both
// recognisers do with one, so no caller needs a second code path.
//
// It is called from inside the per-occurrence memos in [degreeShapeFor] and
// [labelledHopShapeFor], NOT from the per-row dispatch sites, so the block-form
// walk is paid once per subquery occurrence rather than once per outer row.
func subqueryRecogniserBody(sub ast.Expression) (*ast.Pattern, *ast.Where) {
	var (
		pat   *ast.Pattern
		where *ast.Where
		body  *ast.SingleQuery
	)
	switch s := sub.(type) {
	case *ast.ExistsSubquery:
		pat, where, body = s.Pattern, s.Where, s.Query
	case *ast.CountSubquery:
		pat, where, body = s.Pattern, s.Where, s.Query
	default:
		return nil, nil
	}
	// The pattern form needs no normalisation: it IS the canonical shape.
	if pat != nil {
		return pat, where
	}
	if p, w, ok := ir.PatternFormOf(body); ok {
		return p, w
	}
	return nil, nil
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
