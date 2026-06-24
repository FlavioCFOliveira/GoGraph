package expr

// eval.go — Cypher expression evaluator with three-valued logic (task-247).
//
// Eval dispatches on the concrete AST node type and recursively evaluates
// sub-expressions. The implementation follows openCypher 9 semantics:
//
//   - Comparisons involving NULL return NULL (3VL).
//   - IS NULL / IS NOT NULL always return a Bool.
//   - AND/OR/NOT follow the Kleene three-valued truth tables.
//   - Arithmetic promotes Int+Float→Float; Int+Int→Int; Float+Float→Float.
//
// # Concurrency
//
// Eval is stateless and safe for concurrent use.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// RowContext maps variable names to their current runtime values.
// It is typically derived from the current operator row plus the schema
// mapping maintained by the executor.
type RowContext map[string]Value

// FunctionRegistry resolves built-in and user-defined functions by name.
// Implementations must be safe for concurrent use (read-only after init).
type FunctionRegistry interface {
	// Resolve returns the BuiltinFn for name, or (nil, false) when unknown.
	// name is lower-cased by the caller before lookup.
	Resolve(name string) (BuiltinFn, bool)
}

// BuiltinFn is the signature of a built-in Cypher function.
type BuiltinFn func(args []Value) (Value, error)

// SubqueryEvaluator drives [ast.ExistsSubquery] and [ast.CountSubquery]
// expressions at evaluation time. The expression evaluator dispatches every
// subquery occurrence to one of these methods, passing the current outer row
// so that correlated bindings are visible inside the subquery.
//
// Implementations must:
//   - return BoolValue(true) when the inner plan produces ≥1 row, BoolValue(false)
//     otherwise (EvalExists);
//   - return IntegerValue equal to the exact row count produced by the inner
//     plan, 0 when empty (EvalCount);
//   - honour the context (used for cancellation and deadlines);
//   - propagate any error from the inner plan unchanged.
//
// Implementations are expected to compile the subquery's AST once per outer
// query and reuse the compiled operator across outer rows; per-row state is
// reset by re-seeding the inner [Argument] leaf via the IR's ArgTag wiring.
type SubqueryEvaluator interface {
	// EvalExists evaluates an EXISTS { … } subquery against row and returns
	// BoolValue(true) iff the inner plan emits at least one row.
	EvalExists(ctx context.Context, sub *ast.ExistsSubquery, row RowContext, params map[string]Value) (Value, error)
	// EvalCount evaluates a COUNT { … } subquery against row and returns an
	// IntegerValue equal to the number of rows the inner plan emits.
	EvalCount(ctx context.Context, sub *ast.CountSubquery, row RowContext, params map[string]Value) (Value, error)
}

// PatternEvaluator evaluates [ast.PathPattern] expressions used as existential
// predicates inside WHERE clauses (e.g. WHERE (a)-[:T]->(b)). The evaluator
// receives the current row context so that bound variables are visible and can
// be used as anchors for the graph traversal.
//
// EvalPattern must return BoolValue(true) when at least one match for the
// pattern exists in the graph given the bindings in row, BoolValue(false) when
// no match exists, or Null when the result is undefined. It must honour the
// supplied context for cancellation and propagate errors unchanged.
//
// EvalPatternComp is the list-producing variant used for
// [ast.PatternComprehension] expressions that survive IR hoisting (e.g.
// when nested inside a [ast.ListComprehension]'s predicate or
// projection, where lifting the comprehension out of the per-iteration
// scope would lose the iteration variable binding). It enumerates every
// match of pc.Pattern given the bindings in row, evaluates the
// per-match projection (or returns the matched relationship for the
// projection-less form), and returns the collected ListValue. WHERE
// predicates declared inside the comprehension are honoured.
type PatternEvaluator interface {
	// EvalPattern evaluates pp as an existential predicate and returns a boolean
	// Value indicating whether the pattern matches at least one path in the graph.
	EvalPattern(ctx context.Context, pp *ast.PathPattern, row RowContext, params map[string]Value) (Value, error)
	// EvalPatternComp evaluates pc as a list-producing pattern
	// comprehension and returns the projected list. The runtime
	// implementation iterates every match of pc.Pattern with the
	// bindings in row as anchors.
	EvalPatternComp(ctx context.Context, pc *ast.PatternComprehension, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error)
}

// EvalError is returned when Eval encounters a type or semantic error that
// is not representable as a NULL (e.g. unknown operator, unsupported AST node).
type EvalError struct {
	Msg string
}

// Error implements the error interface.
func (e *EvalError) Error() string { return "eval: " + e.Msg }

// DefaultMaxListElements is the per-evaluation upper bound on the total number
// of list ELEMENTS a single expression may materialise across all of its
// iteration helpers (reduce(), list comprehensions and their nested
// combinations, and list concatenation). It guards against a single tiny query
// such as
//
//	reduce(acc=[0], i IN range(1,30) | acc + acc)
//
// (which doubles the accumulator to 2^30 elements) or deeply nested
// comprehensions (which multiply element counts as N^depth) exhausting host
// memory before any pipeline-breaker cap applies — those caps bound result
// ROWS, not intermediate lists built inside ONE evalExpr call for ONE row.
//
// The value mirrors [github.com/FlavioCFOliveira/GoGraph/cypher/funcs].DefaultMaxCollectItems
// (10,000,000) for consistency with the collect()/percentile aggregator budget.
// It is far above any openCypher TCK scenario (whose lists are at most a few
// thousand elements) and above any legitimate single-expression list, so no
// conforming query trips it; an expression that exceeds it returns a typed
// [EvalError] (fail-stop) rather than allocating without bound.
const DefaultMaxListElements = 10_000_000

// DefaultMaxStringEvalBytes is the per-evaluation upper bound on the total
// number of STRING BYTES a single expression may materialise across all of its
// string-producing helpers (string concatenation with "+", and string
// accumulation inside reduce()/comprehensions). The list-element budget
// ([DefaultMaxListElements]) is byte-blind, so it does not stop a string
// doubling such as
//
//	reduce(s='x', i IN range(1,33) | s + s)
//
// from growing one StringValue to gigabytes from O(1) query text (#1482).
// This byte budget complements the element budget: it charges the bytes a
// string concatenation is about to materialise BEFORE allocating, so an
// over-budget concat fails fast with a typed [EvalError] (fail-stop) rather than
// exhausting host memory.
//
// The ceiling is 1 GiB, set far above any legitimate single-expression string.
// The largest string the openCypher TCK constructs or asserts is the 10,000-char
// literal in Literals6 (~10 KiB) — roughly five orders of magnitude below this
// bound — so no conforming query trips it (verified with the cypher-expert; the
// openCypher specification imposes no maximum string length, leaving it
// implementation-defined). Operators on memory-constrained hosts can lower it.
const DefaultMaxStringEvalBytes = 1 << 30

// errListTooLarge builds the typed error returned when an expression would
// materialise more list elements than its per-evaluation budget allows. The
// message shape mirrors the range() over-cap error
// (github.com/FlavioCFOliveira/GoGraph/cypher/funcs.errRangeTooLarge) so callers
// map it to a query error, never a panic or an out-of-memory crash.
func errListTooLarge(limit int64) error {
	return &EvalError{Msg: fmt.Sprintf(
		"ArgumentError: NumberOutOfRange: expression would materialise more than %d list elements, exceeding the maximum of %d",
		limit, limit)}
}

// errStringTooLarge builds the typed error returned when an expression would
// materialise more string bytes than its per-evaluation byte budget allows. Its
// message shape mirrors [errListTooLarge] so callers map it to a query error,
// never a panic or an out-of-memory crash (#1482).
func errStringTooLarge(limit int64) error {
	return &EvalError{Msg: fmt.Sprintf(
		"ArgumentError: NumberOutOfRange: expression would materialise more than %d string bytes, exceeding the maximum of %d",
		limit, limit)}
}

// evalBudget is the per-evaluation cumulative list-element budget shared across
// every iteration helper reached from a single [EvalWith] call. It lets nested
// list comprehensions (whose individual lists may each be small, but whose
// product is enormous) charge against one ceiling. The bare [Eval] entry point
// installs no budget; those helpers fall back to a per-call intrinsic ceiling
// (see [chargeListGrowth]), which still bounds a single oversized list such as
// a doubling reduce accumulator.
//
// evalBudget is not safe for concurrent use; each Eval/EvalWith call owns its
// own instance on the call stack.
type evalBudget struct {
	remaining int64 // remaining list elements that may still be materialised
	limit     int64 // original element ceiling, retained for the error message

	// bytesRemaining and bytesLimit form the parallel per-evaluation STRING
	// BYTE budget (#1482), debited whenever a string is grown (concatenation).
	bytesRemaining int64
	bytesLimit     int64
}

// charge debits n elements from the budget and returns a typed [EvalError] when
// the budget is exhausted. n<=0 is a no-op.
func (b *evalBudget) charge(n int64) error {
	if b == nil || n <= 0 {
		return nil
	}
	b.remaining -= n
	if b.remaining < 0 {
		return errListTooLarge(b.limit)
	}
	return nil
}

// chargeBytes debits n string bytes from the byte budget and returns a typed
// [EvalError] when the budget is exhausted. n<=0 is a no-op (#1482).
func (b *evalBudget) chargeBytes(n int64) error {
	if b == nil || n <= 0 {
		return nil
	}
	b.bytesRemaining -= n
	if b.bytesRemaining < 0 {
		return errStringTooLarge(b.bytesLimit)
	}
	return nil
}

// extractBudget returns the per-evaluation [evalBudget] smuggled through row by
// [EvalWith], or nil when none is present (the bare [Eval] path).
func extractBudget(row RowContext) *evalBudget {
	if row == nil {
		return nil
	}
	scv, ok := row[subqueryContextKey].(*subqueryContextValue)
	if !ok {
		return nil
	}
	return &scv.budget
}

// chargeListGrowth charges n newly-materialised list elements against the
// per-evaluation budget carried by row. When row carries no budget (the bare
// [Eval] path), it instead enforces an intrinsic per-call ceiling of
// [DefaultMaxListElements] on n alone, which still rejects a single oversized
// list. It returns a typed [EvalError] on breach.
func chargeListGrowth(row RowContext, n int64) error {
	if b := extractBudget(row); b != nil {
		return b.charge(n)
	}
	if n > DefaultMaxListElements {
		return errListTooLarge(DefaultMaxListElements)
	}
	return nil
}

// chargeStringGrowth charges n newly-materialised string bytes against the
// per-evaluation byte budget carried by row. When row carries no budget (the
// bare [Eval] path), it instead enforces an intrinsic per-call ceiling of
// [DefaultMaxStringEvalBytes] on n alone, which still rejects a single oversized
// concatenation. It returns a typed [EvalError] on breach (#1482).
func chargeStringGrowth(row RowContext, n int64) error {
	if b := extractBudget(row); b != nil {
		return b.chargeBytes(n)
	}
	if n > DefaultMaxStringEvalBytes {
		return errStringTooLarge(DefaultMaxStringEvalBytes)
	}
	return nil
}

// ctxIterCheckStride is the iteration stride at which the list-iteration
// helpers poll the context for cancellation, mirroring the executor's
// every-4096-tuples convention (see cypher/exec/operator.go and eager.go).
const ctxIterCheckStride = 4096

// checkIterCtx polls the context smuggled through row for cancellation when
// iter is a multiple of [ctxIterCheckStride]. It returns the context error
// (context.Canceled / context.DeadlineExceeded) promptly so a long in-expression
// loop — reduce(), a comprehension, or a quantifier over a large list — can be
// aborted by a caller's deadline or cancellation. On the bare [Eval] path no
// context is smuggled; the extracted context is context.Background() and the
// check never fires.
func checkIterCtx(ctx context.Context, iter int) error {
	if iter%ctxIterCheckStride != 0 {
		return nil
	}
	return ctx.Err()
}

// Eval evaluates expr in the context of row and params. It dispatches on the
// concrete AST node type and returns the resulting Value. An EvalError is
// returned for unsupported constructs; all other errors propagate from the
// function registry.
//
// If reg is nil, function invocations return an EvalError.
//
// Eval does not support subquery expressions ([ast.ExistsSubquery],
// [ast.CountSubquery]); these return an [EvalError]. Use [EvalWith] with a
// non-nil [SubqueryEvaluator] to enable subquery evaluation.
func Eval(expr ast.Expression, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	return evalExpr(expr, row, params, reg)
}

// EvalWith evaluates expr just like [Eval], but threads a [context.Context]
// and optional evaluators through the evaluation. The context is used for
// cancellation and deadlines when subquery or pattern evaluation is involved.
// subEval handles [ast.ExistsSubquery] and [ast.CountSubquery] occurrences;
// patEval handles [ast.PathPattern] existential predicates in WHERE clauses.
//
// When subEval is nil, subquery expressions produce an [EvalError].
// When patEval is nil, pattern predicate expressions produce an [EvalError].
//
// EvalWith is safe for concurrent use: each call carries its own context and
// evaluators on the call stack; there is no shared mutable state.
func EvalWith(ctx context.Context, expr ast.Expression, row RowContext, params map[string]Value, reg FunctionRegistry, subEval SubqueryEvaluator, patEval PatternEvaluator) (Value, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// The subquery/pattern context is carried alongside the per-row evaluator
	// state via a small holder so it threads through every recursive call
	// automatically. We attach it to the RowContext under a sentinel reserved
	// key that cannot collide with any valid Cypher identifier (NUL bytes are
	// not legal in identifiers per the openCypher 9 grammar §A.1).
	//
	// The sentinel is inserted into the caller's map in place rather than into
	// a full clone: cloning copied every binding on every projected row, the
	// dominant per-row allocation on the WITH/RETURN general path (#1501). The
	// caller owns a freshly built, single-goroutine RowContext that is
	// discarded after this call, so an in-place toggle is sound. To remain
	// re-entrant — a nested EvalWith reached through a pattern comprehension
	// can receive a map that already carries the sentinel (it was copied down
	// by enumeratePatternMatches' cloneRow) — the prior binding is saved and
	// restored on return rather than blindly deleted.
	//
	// A nil RowContext (a legal input: an expression with no variable
	// bindings) has no slot to toggle, so it needs a one-entry map to carry
	// the sentinel holder — there is nothing to copy, so this is not the clone
	// the optimisation removes. Binding-free expressions (RETURN 1, constant
	// projections, parameter-only predicates) hit this on every evaluated row,
	// so the map is drawn from a pool and returned on exit instead of freshly
	// allocated each call (#1721). The map's lifetime is exactly this single,
	// single-goroutine call: the sentinel is the only key ever written to it
	// (every binding-introducing form — list/pattern comprehension — clones
	// into a fresh inner map, never our row), and it is deleted again below, so
	// the map is observably empty when released.
	pooledRow := false
	if row == nil {
		row = bindingFreeRowPool.Get().(RowContext)
		pooledRow = true
	}
	// The holder + its embedded budget were the dominant per-row allocation on
	// the common (no-subquery) evaluation path (#1589): every EvalWith call
	// heap-allocated a fresh subqueryContextValue and evalBudget even when no
	// subquery / pattern / list-budget work occurred. They are now drawn from a
	// sync.Pool and returned on exit. The lifetime is exactly this single,
	// single-goroutine EvalWith call: the holder lives only in row[sentinel],
	// is restored to its prior binding before return, and is never retained by
	// any evaluator (subquery/pattern evaluators read correlation bindings and
	// return fresh scalar/list values; they never keep the holder), so it is
	// sound to recycle. A nested EvalWith (reached through a pattern
	// comprehension) acquires its OWN holder and restores ours on return — each
	// call releases only the holder it acquired, so the LIFO save/restore and
	// the pool stay in lockstep.
	scv := acquireSubqueryContext()
	scv.ctx = ctx
	scv.sub = subEval
	scv.pat = patEval
	scv.budget = evalBudget{
		remaining:      DefaultMaxListElements,
		limit:          DefaultMaxListElements,
		bytesRemaining: DefaultMaxStringEvalBytes,
		bytesLimit:     DefaultMaxStringEvalBytes,
	}
	prev, had := row[subqueryContextKey]
	row[subqueryContextKey] = scv
	v, err := evalExpr(expr, row, params, reg)
	if had {
		row[subqueryContextKey] = prev
	} else {
		delete(row, subqueryContextKey)
	}
	releaseSubqueryContext(scv)
	// Recycle the pooled binding-free map only once it is observably empty (the
	// sentinel deleted above, no binding written in place). The guard keeps a
	// future in-place writer from poisoning the pool: a non-empty map is simply
	// dropped for the GC rather than reused.
	if pooledRow && len(row) == 0 {
		bindingFreeRowPool.Put(row)
	}
	return v, err
}

// bindingFreeRowPool recycles the single-entry RowContext that [EvalWith] needs
// when called with a nil row (a binding-free expression). The map exists only
// to carry the pooled subquery-context holder under subqueryContextKey and is
// emptied again before EvalWith returns, so it is reused across calls instead
// of freshly allocated each time. RowContext is a map — pointer-shaped — so
// boxing it into the pool's any allocates nothing.
var bindingFreeRowPool = sync.Pool{New: func() any { return make(RowContext, 1) }}

// subqueryContextPool recycles the per-evaluation holder allocated by
// [EvalWith]. The holder's lifetime is exactly one EvalWith call (it is
// installed into the call's RowContext, restored before return, and never
// retained beyond the call), so recycling it removes the per-row holder +
// budget allocation without any aliasing hazard. See [EvalWith].
var subqueryContextPool = sync.Pool{New: func() any { return new(subqueryContextValue) }}

// acquireSubqueryContext returns a holder from the pool. The caller fully
// overwrites every field before use, so a recycled holder needs no clearing.
func acquireSubqueryContext() *subqueryContextValue {
	scv, _ := subqueryContextPool.Get().(*subqueryContextValue)
	if scv == nil {
		scv = new(subqueryContextValue)
	}
	return scv
}

// releaseSubqueryContext returns a holder to the pool. The evaluator references
// (ctx/sub/pat) are nilled so a pooled holder cannot pin a context, evaluator,
// or — transitively — a graph snapshot between evaluations.
func releaseSubqueryContext(scv *subqueryContextValue) {
	scv.ctx = nil
	scv.sub = nil
	scv.pat = nil
	subqueryContextPool.Put(scv)
}

// subqueryContextKey is the sentinel RowContext key used by [EvalWith] to
// smuggle the [context.Context] and [SubqueryEvaluator] down through the
// recursive evaluator without touching every helper's signature. The key
// contains NUL bytes that are not legal in Cypher identifiers per the
// openCypher 9 grammar §A.1, so no user variable can ever collide with it.
const subqueryContextKey = "\x00subquery-context\x00"

// subqueryContextValue is the holder stored under [subqueryContextKey]. It
// implements [Value] so it can live inside a [RowContext] map alongside real
// runtime values. The smuggled fields are accessed via
// [extractSubqueryContext] and [extractPatternEvaluator]; user code never
// sees this value.
type subqueryContextValue struct {
	ctx    context.Context //nolint:containedctx // smuggled through RowContext, see EvalWith
	sub    SubqueryEvaluator
	pat    PatternEvaluator
	budget evalBudget // per-evaluation cumulative list-element budget (#1475), embedded by value so the holder is a single (pooled) allocation
}

// Kind implements [Value]. Returns [KindNull] because subqueryContextValue
// must never appear in arithmetic or comparison contexts; if it does, the
// 3-valued logic will propagate Null and surface the bug as a Null result.
func (*subqueryContextValue) Kind() Kind { return KindNull }

// Equal implements [Value]. Always returns Null — subqueryContextValue must
// never be compared for equality.
func (*subqueryContextValue) Equal(_ Value) Value { return Null }

// Hash implements [Value]. Returns a fixed sentinel so accidental map
// insertion is deterministic.
func (*subqueryContextValue) Hash() uint64 { return 0 }

// String implements [Value]. Returns a fixed sentinel string for debugging.
func (*subqueryContextValue) String() string { return "<subquery-context>" }

// extractSubqueryContext returns the smuggled context and SubqueryEvaluator
// from row, or (context.Background(), nil) when none is present.
func extractSubqueryContext(row RowContext) (context.Context, SubqueryEvaluator) {
	if row == nil {
		return context.Background(), nil
	}
	v, ok := row[subqueryContextKey]
	if !ok {
		return context.Background(), nil
	}
	scv, ok := v.(*subqueryContextValue)
	if !ok {
		return context.Background(), nil
	}
	return scv.ctx, scv.sub
}

// extractPatternEvaluator returns the smuggled context and PatternEvaluator
// from row, or (context.Background(), nil) when none is present.
func extractPatternEvaluator(row RowContext) (context.Context, PatternEvaluator) {
	if row == nil {
		return context.Background(), nil
	}
	v, ok := row[subqueryContextKey]
	if !ok {
		return context.Background(), nil
	}
	scv, ok := v.(*subqueryContextValue)
	if !ok {
		return context.Background(), nil
	}
	return scv.ctx, scv.pat
}

func evalExpr(e ast.Expression, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) { //nolint:gocyclo // Main dispatch switch; all branches are simple delegations and cannot be split without obscuring the type mapping.
	switch n := e.(type) {
	// ── Literals ──────────────────────────────────────────────────────────────
	case *ast.NullLiteral:
		return Null, nil
	case *ast.BoolLiteral:
		return BoolValue(n.Value), nil
	case *ast.IntLiteral:
		return IntegerValue(n.Value), nil
	case *ast.FloatLiteral:
		return FloatValue(n.Value), nil
	case *ast.StringLiteral:
		return StringValue(n.Value), nil

	// ── Composite literals ─────────────────────────────────────────────────────
	case *ast.ListLiteral:
		return evalListLiteral(n, row, params, reg)
	case *ast.MapLiteral:
		return evalMapLiteral(n, row, params, reg)

	// ── Variable and parameter ─────────────────────────────────────────────────
	case *ast.Variable:
		if v, ok := row[n.Name]; ok {
			return v, nil
		}
		return Null, nil // unbound variable → NULL per openCypher semantics

	case *ast.Parameter:
		if params != nil {
			if v, ok := params[n.Name]; ok {
				return v, nil
			}
		}
		return Null, nil // unset parameter → NULL

	// ── Property access ────────────────────────────────────────────────────────
	case *ast.Property:
		return evalProperty(n, row, params, reg)

	// ── Label predicate ────────────────────────────────────────────────────────
	case *ast.LabelPredicate:
		return evalLabelPredicate(n, row, params, reg)

	// ── Subscript access ───────────────────────────────────────────────────────
	case *ast.SubscriptExpr:
		return evalSubscript(n, row, params, reg)

	// ── Slice access ───────────────────────────────────────────────────────────
	case *ast.SliceExpr:
		return evalSlice(n, row, params, reg)

	// ── List comprehension ─────────────────────────────────────────────────────
	case *ast.ListComprehension:
		return evalListComprehension(n, row, params, reg)

	// ── Map projection ─────────────────────────────────────────────────────────
	case *ast.MapProjection:
		return evalMapProjection(n, row, params, reg)

	// ── Binary operator ────────────────────────────────────────────────────────
	case *ast.BinaryOp:
		return evalBinaryOp(n, row, params, reg)

	// ── Unary operator ─────────────────────────────────────────────────────────
	case *ast.UnaryOp:
		return evalUnaryOp(n, row, params, reg)

	// ── CASE expression ────────────────────────────────────────────────────────
	case *ast.CaseExpression:
		return evalCase(n, row, params, reg)

	// ── Function call ──────────────────────────────────────────────────────────
	case *ast.FunctionInvocation:
		return evalFunction(n, row, params, reg)

	// ── EXISTS { … } subquery ──────────────────────────────────────────────────
	case *ast.ExistsSubquery:
		ctx, subEval := extractSubqueryContext(row)
		if subEval == nil {
			return nil, &EvalError{Msg: "EXISTS { … } subquery is not supported in this evaluation context (no SubqueryEvaluator wired)"}
		}
		return subEval.EvalExists(ctx, n, row, params)

	// ── COUNT { … } subquery ───────────────────────────────────────────────────
	case *ast.CountSubquery:
		ctx, subEval := extractSubqueryContext(row)
		if subEval == nil {
			return nil, &EvalError{Msg: "COUNT { … } subquery is not supported in this evaluation context (no SubqueryEvaluator wired)"}
		}
		return subEval.EvalCount(ctx, n, row, params)

	// ── Pattern predicate (existential check) ─────────────────────────────────
	// WHERE (a)-[:T]->(b) is an existential check: true iff at least one path
	// matching the pattern exists in the graph given the bindings in row.
	case *ast.PathPattern:
		ctx, patEval := extractPatternEvaluator(row)
		if patEval == nil {
			return nil, &EvalError{Msg: "pattern predicate is not supported in this evaluation context (no PatternEvaluator wired)"}
		}
		return patEval.EvalPattern(ctx, n, row, params)

	// ── Pattern comprehension (list-producing) ────────────────────────────────
	// Survives IR hoisting in nested contexts (e.g. inside a list
	// comprehension's projection) where lifting the comprehension out of
	// the iteration scope would lose the iteration variable binding.
	// Closes Pattern2 [7].
	case *ast.PatternComprehension:
		ctx, patEval := extractPatternEvaluator(row)
		if patEval == nil {
			return nil, &EvalError{Msg: "pattern comprehension is not supported in this evaluation context (no PatternEvaluator wired)"}
		}
		return patEval.EvalPatternComp(ctx, n, row, params, reg)

	case *ast.ReduceExpr:
		return evalReduceExpr(n, row, params, reg)

	default:
		return nil, &EvalError{Msg: fmt.Sprintf("unsupported expression type %T", e)}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Composite literals
// ─────────────────────────────────────────────────────────────────────────────

func evalListLiteral(n *ast.ListLiteral, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	result := make(ListValue, len(n.Elements))
	for i, elem := range n.Elements {
		v, err := evalExpr(elem, row, params, reg)
		if err != nil {
			return nil, err
		}
		result[i] = v
	}
	return result, nil
}

func evalMapLiteral(n *ast.MapLiteral, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	result := make(MapValue, len(n.Keys))
	for i, k := range n.Keys {
		v, err := evalExpr(n.Values[i], row, params, reg)
		if err != nil {
			return nil, err
		}
		result[k] = v
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Label predicate
// ─────────────────────────────────────────────────────────────────────────────

// evalLabelPredicate evaluates `receiver:Label1:Label2`. The receiver
// may be a Node (conjunctive label test against the node's labels) or
// a Relationship (type-name match — a relationship has exactly one
// type and only one label may be specified after the colon, per
// openCypher 9). NULL receiver propagates to NULL; any other kind
// yields NULL (a runtime type mismatch).
func evalLabelPredicate(n *ast.LabelPredicate, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	recv, err := evalExpr(n.Receiver, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(recv) {
		return Null, nil
	}
	switch r := recv.(type) {
	case NodeValue:
		for _, want := range n.Labels {
			found := false
			for _, have := range r.Labels {
				if have == want {
					found = true
					break
				}
			}
			if !found {
				return BoolValue(false), nil
			}
		}
		return BoolValue(true), nil
	case *LazyNodeValue:
		// Lazy node fast path: test each required label membership on demand.
		// The conjunction `n:A:B` is true iff every label is present; a
		// node with no labels yields false (not null), matching the eager
		// NodeValue branch above.
		for _, want := range n.Labels {
			if !r.HasLabel(want) {
				return BoolValue(false), nil
			}
		}
		return BoolValue(true), nil
	case RelationshipValue:
		// A relationship has exactly one type; the openCypher spec
		// only allows a single label after the colon. We accept the
		// same conjunctive walk for forward-compat but the only legal
		// shape today is `r:Type`.
		for _, want := range n.Labels {
			if r.Type != want {
				return BoolValue(false), nil
			}
		}
		return BoolValue(true), nil
	}
	return Null, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Property access
// ─────────────────────────────────────────────────────────────────────────────

func evalProperty(n *ast.Property, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	recv, err := evalExpr(n.Receiver, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(recv) {
		return Null, nil
	}
	switch r := recv.(type) {
	case NodeValue:
		if r.Deleted {
			return nil, &EvalError{Msg: fmt.Sprintf("EntityNotFound: DeletedEntityAccess: cannot read property %q of deleted node", n.Key)}
		}
		if r.Properties != nil {
			if v, ok := r.Properties[n.Key]; ok {
				return v, nil
			}
		}
		return Null, nil
	case *LazyNodeValue:
		// Lazy node fast path: resolve only the touched property from
		// storage. A LazyNodeValue is constructed solely for nodes the
		// static analysis proved are accessed only through scalar
		// accessors, and never for an entity deleted in the same statement
		// (the DELETE operator stamps those as a Deleted NodeValue, handled
		// above), so the DeletedEntityAccess contract is preserved without a
		// flag here. A missing key reads as Null (Property does this).
		return r.Property(n.Key), nil
	case RelationshipValue:
		if r.Deleted {
			return nil, &EvalError{Msg: fmt.Sprintf("EntityNotFound: DeletedEntityAccess: cannot read property %q of deleted relationship", n.Key)}
		}
		if r.Properties != nil {
			if v, ok := r.Properties[n.Key]; ok {
				return v, nil
			}
		}
		return Null, nil
	case MapValue:
		if v, ok := r[n.Key]; ok {
			return v, nil
		}
		return Null, nil
	case DateValue, LocalDateTimeValue, DateTimeValue, LocalTimeValue, TimeValue, DurationValue:
		if v, ok := temporalAccessor(recv, n.Key); ok {
			return v, nil
		}
		return Null, nil
	case IntegerValue, FloatValue:
		// The parser reconstructs float literals like `1.0` from an
		// IntLiteral atom followed by a numeric Name accessor; very
		// long floats may slip through that reconstruction and reach
		// the evaluator as Property{Receiver: IntLiteral, Key: digits}.
		// Returning NULL here keeps those queries running instead of
		// surfacing a type error on a literal float that just happens
		// to lose its FloatLiteral reconstruction.
		return Null, nil
	}
	// Property access on a non-map/non-graph/non-temporal value is an
	// InvalidArgumentType TypeError per openCypher (e.g. `'string'.foo`,
	// `true.foo`, `[1, 2].foo`).
	return nil, &EvalError{Msg: fmt.Sprintf("InvalidArgumentType: property access requires Map, Node, or Relationship, got %s", recv.Kind())}
}

// ─────────────────────────────────────────────────────────────────────────────
// Subscript access
// ─────────────────────────────────────────────────────────────────────────────

func evalSubscript(n *ast.SubscriptExpr, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	container, err := evalExpr(n.Expr, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(container) {
		return Null, nil
	}
	idx, err := evalExpr(n.Index, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(idx) {
		return Null, nil
	}
	switch c := container.(type) {
	case ListValue:
		// Index into a list — must be an Integer; a non-integer index
		// (e.g. `list[1.5]`, `list["x"]`) is an InvalidArgumentType
		// TypeError at runtime per openCypher.
		if _, ok := idx.(IntegerValue); !ok {
			return nil, &EvalError{Msg: fmt.Sprintf("InvalidArgumentType: list index must be Integer, got %s", idx.Kind())}
		}
		return subscriptList(c, idx), nil
	case MapValue:
		if _, ok := idx.(StringValue); !ok {
			return nil, &EvalError{Msg: fmt.Sprintf("MapElementAccessByNonString: map key must be String, got %s", idx.Kind())}
		}
		return subscriptMap(c, idx), nil
	case NodeValue:
		if _, ok := idx.(StringValue); !ok {
			return nil, &EvalError{Msg: fmt.Sprintf("MapElementAccessByNonString: map key must be String, got %s", idx.Kind())}
		}
		return subscriptMap(c.Properties, idx), nil
	case *LazyNodeValue:
		// Lazy node subscript: only a String key is valid (same TypeError
		// surface as the eager NodeValue branch). The static analysis only
		// produces a LazyNodeValue when subscripts use literal-string keys,
		// but resolving any runtime String key on demand is equally sound.
		sk, ok := idx.(StringValue)
		if !ok {
			return nil, &EvalError{Msg: fmt.Sprintf("MapElementAccessByNonString: map key must be String, got %s", idx.Kind())}
		}
		return c.Property(string(sk)), nil
	case RelationshipValue:
		if _, ok := idx.(StringValue); !ok {
			return nil, &EvalError{Msg: fmt.Sprintf("MapElementAccessByNonString: map key must be String, got %s", idx.Kind())}
		}
		return subscriptMap(c.Properties, idx), nil
	default:
		// Subscripting a non-list / non-map / non-graph-element value is
		// an InvalidArgumentType TypeError per openCypher (e.g. `1[0]`,
		// `'foo'[0]`, `true[0]`).
		return nil, &EvalError{Msg: fmt.Sprintf("InvalidArgumentType: cannot index into %s", container.Kind())}
	}
}

// subscriptList returns list[idx] using openCypher list-indexing semantics:
// negative indices wrap from the end; out-of-range indices and non-integer
// keys yield NULL.
func subscriptList(list ListValue, idx Value) Value {
	i, ok := idx.(IntegerValue)
	if !ok {
		return Null
	}
	pos := int(i)
	if pos < 0 {
		pos = len(list) + pos
	}
	if pos < 0 || pos >= len(list) {
		return Null
	}
	return list[pos]
}

// subscriptMap returns m[idx] for any MapValue-shaped container (used for
// MapValue itself and for the Properties of NodeValue / RelationshipValue).
// Non-string keys and absent keys both yield NULL.
func subscriptMap(m MapValue, idx Value) Value {
	k, ok := idx.(StringValue)
	if !ok {
		return Null
	}
	if v, exists := m[string(k)]; exists {
		return v
	}
	return Null
}

// ─────────────────────────────────────────────────────────────────────────────
// Binary operator
// ─────────────────────────────────────────────────────────────────────────────

func evalBinaryOp(n *ast.BinaryOp, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) { //nolint:gocyclo // One case per binary operator; splitting would obscure the operator mapping without reducing real complexity.
	// AND and OR short-circuit under 3VL before evaluating right.
	switch n.Operator {
	case "AND":
		return eval3VLAND(n, row, params, reg)
	case "OR":
		return eval3VLOR(n, row, params, reg)
	}

	left, err := evalExpr(n.Left, row, params, reg)
	if err != nil {
		return nil, err
	}
	right, err := evalExpr(n.Right, row, params, reg)
	if err != nil {
		return nil, err
	}

	switch n.Operator {
	// ── Equality / inequality ─────────────────────────────────────────────────
	case "=":
		return left.Equal(right), nil
	case "<>":
		eq := left.Equal(right)
		if IsNull(eq) {
			return Null, nil
		}
		return BoolValue(!IsTruthy(eq)), nil

	// ── Ordering comparisons ──────────────────────────────────────────────────
	case "<", "<=", ">", ">=":
		return evalOrdering(n.Operator, left, right)

	// ── Arithmetic ────────────────────────────────────────────────────────────
	case "+":
		return evalArith(row, "+", left, right)
	case "-":
		return evalArith(row, "-", left, right)
	case "*":
		return evalArith(row, "*", left, right)
	case "/":
		return evalArith(row, "/", left, right)
	case "%":
		return evalArith(row, "%", left, right)
	case "^":
		return evalArith(row, "^", left, right)

	// ── String operators ──────────────────────────────────────────────────────
	case "CONTAINS":
		return evalStringOp("CONTAINS", left, right)
	case "STARTS WITH":
		return evalStringOp("STARTS WITH", left, right)
	case "ENDS WITH":
		return evalStringOp("ENDS WITH", left, right)
	case "=~":
		return evalStringOp("=~", left, right)

	// ── List / map membership ─────────────────────────────────────────────────
	case "IN":
		return evalIn(left, right)

	// ── XOR ──────────────────────────────────────────────────────────────────
	case "XOR":
		return eval3VLXOR(left, right)

	default:
		return nil, &EvalError{Msg: fmt.Sprintf("unsupported binary operator %q", n.Operator)}
	}
}

// eval3VLAND evaluates AND with Kleene 3VL short-circuit.
func eval3VLAND(n *ast.BinaryOp, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	left, err := evalExpr(n.Left, row, params, reg)
	if err != nil {
		return nil, err
	}
	// false AND _ = false (short-circuit, even over NULL)
	if b, ok := left.(BoolValue); ok && !bool(b) {
		return BoolValue(false), nil
	}
	right, err := evalExpr(n.Right, row, params, reg)
	if err != nil {
		return nil, err
	}
	// false AND _ = false
	if b, ok := right.(BoolValue); ok && !bool(b) {
		return BoolValue(false), nil
	}
	// NULL AND NULL = NULL; NULL AND true = NULL
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	return BoolValue(true), nil
}

// eval3VLOR evaluates OR with Kleene 3VL short-circuit.
func eval3VLOR(n *ast.BinaryOp, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	left, err := evalExpr(n.Left, row, params, reg)
	if err != nil {
		return nil, err
	}
	// true OR _ = true (short-circuit, even over NULL)
	if IsTruthy(left) {
		return BoolValue(true), nil
	}
	right, err := evalExpr(n.Right, row, params, reg)
	if err != nil {
		return nil, err
	}
	// true OR _ = true
	if IsTruthy(right) {
		return BoolValue(true), nil
	}
	// NULL OR false = NULL; NULL OR NULL = NULL
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	return BoolValue(false), nil
}

// eval3VLXOR evaluates XOR with 3VL: NULL XOR _ = NULL.
func eval3VLXOR(left, right Value) (Value, error) {
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	lb, lok := left.(BoolValue)
	rb, rok := right.(BoolValue)
	if !lok || !rok {
		return Null, nil
	}
	return BoolValue(bool(lb) != bool(rb)), nil
}

// evalOrdering handles <, <=, >, >= with 3VL: NULL operand → NULL.
// openCypher 9 §3.5.4 distinguishes two NaN cases:
//   - NaN compared with a NUMBER (Integer / Float) → FALSE for every
//     ordering operator. This follows IEEE-754: a NaN is not greater,
//     less, or equal to any finite number.
//   - NaN compared with a non-number (String, Bool, Node, …) → NULL.
//     The kinds are incompatible for ordering, so the result is
//     undefined rather than the IEEE-754 FALSE.
//
// The NaN-handling branch runs BEFORE compareValues so the
// sort-friendly cmpFloat64 (which orders NaN after every finite number
// for ORDER BY stability) does not leak that ordering decision into
// runtime comparison results.
func evalOrdering(op string, left, right Value) (Value, error) {
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	if isFloatNaN(left) || isFloatNaN(right) {
		// Determine the "other" operand's kind: NaN-vs-number → FALSE,
		// NaN-vs-anything-else → NULL.
		other := right
		if isFloatNaN(left) {
			other = right
		}
		if isFloatNaN(right) {
			other = left
		}
		switch other.Kind() {
		case KindInteger, KindFloat:
			return BoolValue(false), nil
		}
		return Null, nil
	}
	cmp, err := compareValues(left, right)
	if err != nil {
		return Null, nil //nolint:nilerr // type mismatch → NULL per openCypher
	}
	switch op {
	case "<":
		return BoolValue(cmp < 0), nil
	case "<=":
		return BoolValue(cmp <= 0), nil
	case ">":
		return BoolValue(cmp > 0), nil
	case ">=":
		return BoolValue(cmp >= 0), nil
	}
	return Null, nil
}

// compareValues compares two non-null values of compatible types.
// Returns an error when the types are incompatible for ordering.
func compareValues(a, b Value) (int, error) {
	// Promote Int to Float when comparing with Float.
	a, b = promoteNumeric(a, b)
	switch av := a.(type) {
	case IntegerValue:
		if bv, ok := b.(IntegerValue); ok {
			return cmpInt64(int64(av), int64(bv)), nil
		}
	case FloatValue:
		if bv, ok := b.(FloatValue); ok {
			return cmpFloat64(float64(av), float64(bv)), nil
		}
	case StringValue:
		if bv, ok := b.(StringValue); ok {
			s1, s2 := string(av), string(bv)
			if s1 < s2 {
				return -1, nil
			}
			if s1 > s2 {
				return 1, nil
			}
			return 0, nil
		}
	case BoolValue:
		if bv, ok := b.(BoolValue); ok {
			return compareBool(bool(av), bool(bv)), nil
		}
	}
	// Same-kind list comparison: openCypher 9 §3.5 defines a lexicographic
	// order on lists. NULL elements propagate per 3-valued logic but the
	// dedicated helper [compareListWith3VL] resolves the cases where a
	// definitive non-equality wins over NULLs.
	ka, kb := a.Kind(), b.Kind()
	if ka == KindList && kb == KindList {
		al, _ := a.(ListValue) //nolint:forcetypeassert // kind pre-checked
		bl, _ := b.(ListValue) //nolint:forcetypeassert // kind pre-checked
		return compareListWith3VL(al, bl)
	}
	// Same-kind temporal and duration values delegate to compareSameKind,
	// which already implements the canonical openCypher ordering for
	// dates, local/zoned times, local/zoned date-times and durations.
	if ka == kb {
		switch ka {
		case KindDate, KindLocalDateTime, KindDateTime, KindLocalTime, KindTime, KindDuration:
			return compareSameKind(ka, a, b), nil
		}
	}
	return 0, &EvalError{Msg: fmt.Sprintf("incompatible types for comparison: %s vs %s", a.Kind(), b.Kind())}
}

// compareListWith3VL compares two lists lexicographically with openCypher
// three-valued semantics: a definitive non-equal element wins; otherwise
// any NULL element collapses the result to NULL by returning an error so
// the surrounding ordering helper propagates NULL.
func compareListWith3VL(al, bl ListValue) (int, error) {
	n := len(al)
	if len(bl) < n {
		n = len(bl)
	}
	sawNull := false
	for i := range n {
		if IsNull(al[i]) || IsNull(bl[i]) {
			sawNull = true
			continue
		}
		c, err := compareValues(al[i], bl[i])
		if err != nil {
			// Element-wise type mismatch: collapse to NULL.
			sawNull = true
			continue
		}
		if c != 0 {
			return c, nil
		}
	}
	if sawNull {
		return 0, &EvalError{Msg: "list comparison contained null"}
	}
	if len(al) < len(bl) {
		return -1, nil
	}
	if len(al) > len(bl) {
		return 1, nil
	}
	return 0, nil
}

// promoteNumeric promotes Int/Float pairs so that arithmetic is consistent.
func promoteNumeric(a, b Value) (Value, Value) { //nolint:gocritic // Named returns would add noise; caller always destructures both values.
	_, aIsInt := a.(IntegerValue)
	_, bIsFloat := b.(FloatValue)
	if aIsInt && bIsFloat {
		return FloatValue(float64(a.(IntegerValue))), b //nolint:forcetypeassert // kind pre-checked
	}
	_, aIsFloat := a.(FloatValue)
	_, bIsInt := b.(IntegerValue)
	if aIsFloat && bIsInt {
		return a, FloatValue(float64(b.(IntegerValue))) //nolint:forcetypeassert // kind pre-checked
	}
	return a, b
}

// evalArith evaluates arithmetic binary operators. row carries the
// per-evaluation list-element budget (#1475), consulted before any list
// concatenation allocates so a doubling accumulator (reduce(acc=[0], … | acc +
// acc)) is rejected with a typed [EvalError] rather than allocating an
// exponentially large slice.
func evalArith(row RowContext, op string, left, right Value) (Value, error) {
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	// String concatenation: "+" between strings.
	if op == "+" {
		if ls, lok := left.(StringValue); lok {
			if rs, rok := right.(StringValue); rok {
				// Charge the bytes the concatenation is about to materialise
				// against the per-evaluation byte budget BEFORE allocating, so a
				// doubling accumulator (reduce(s='x', … | s + s)) is rejected
				// with a typed [EvalError] rather than growing one string to
				// gigabytes from O(1) query text (#1482).
				if err := chargeStringGrowth(row, int64(len(ls))+int64(len(rs))); err != nil {
					return nil, err
				}
				return StringValue(string(ls) + string(rs)), nil
			}
		}
		// List concatenation and list+element / element+list append.
		// openCypher spec §3.5 (Collections): list + list → concatenation;
		// list + element → append element; element + list → prepend element.
		// Each branch charges the elements it is about to materialise against
		// the per-evaluation budget BEFORE make(), so an over-budget concat
		// fails fast instead of attempting an oversized allocation (#1475).
		if ll, lok := left.(ListValue); lok {
			if rl, rok := right.(ListValue); rok {
				// list + list
				if err := chargeListGrowth(row, int64(len(ll))+int64(len(rl))); err != nil {
					return nil, err
				}
				result := make(ListValue, len(ll)+len(rl))
				copy(result, ll)
				copy(result[len(ll):], rl)
				return result, nil
			}
			// list + element: wrap right in a single-element list and append.
			if err := chargeListGrowth(row, int64(len(ll))+1); err != nil {
				return nil, err
			}
			result := make(ListValue, len(ll)+1)
			copy(result, ll)
			result[len(ll)] = right
			return result, nil
		}
		if rl, rok := right.(ListValue); rok {
			// element + list: prepend left to right.
			if err := chargeListGrowth(row, int64(len(rl))+1); err != nil {
				return nil, err
			}
			result := make(ListValue, 1+len(rl))
			result[0] = left
			copy(result[1:], rl)
			return result, nil
		}
	}
	// Temporal arithmetic (Date/DateTime/Time/Duration/...): dispatched
	// before numeric promotion to keep typed combinations precise.
	if v, ok := evalTemporalArith(op, left, right); ok {
		return v, nil
	}
	left, right = promoteNumeric(left, right)
	switch lv := left.(type) {
	case IntegerValue:
		rv, ok := right.(IntegerValue)
		if !ok {
			return Null, nil
		}
		return evalIntArith(op, int64(lv), int64(rv))
	case FloatValue:
		rv, ok := right.(FloatValue)
		if !ok {
			return Null, nil
		}
		return evalFloatArith(op, float64(lv), float64(rv))
	}
	return Null, nil
}

// evalTemporalArith handles temporal × temporal and temporal × number
// arithmetic dispatched from [evalArith]. It returns (value, true) when at
// least one operand is a temporal kind and the operation has a defined
// outcome; otherwise (Null, false) leaves dispatch to the numeric path.
//
//nolint:gocyclo // One branch per (kind, op) pair; splitting hides the pattern.
func evalTemporalArith(op string, left, right Value) (Value, bool) {
	// Duration ± Duration, Duration * scalar, Duration / scalar.
	if ld, lok := left.(DurationValue); lok {
		if rd, rok := right.(DurationValue); rok {
			switch op {
			case "+":
				return AddDurations(ld, rd), true
			case "-":
				return SubDurations(ld, rd), true
			}
		}
		if op == "*" {
			if ri, ok := right.(IntegerValue); ok {
				return MulDuration(ld, int64(ri)), true
			}
			if rf, ok := right.(FloatValue); ok {
				return MulDurationFloat(ld, float64(rf)), true
			}
		}
		if op == "/" {
			if ri, ok := right.(IntegerValue); ok {
				return DivDurationFloat(ld, float64(int64(ri))), true
			}
			if rf, ok := right.(FloatValue); ok {
				return DivDurationFloat(ld, float64(rf)), true
			}
		}
	}
	// scalar * Duration.
	if op == "*" {
		if rd, ok := right.(DurationValue); ok {
			if li, ok2 := left.(IntegerValue); ok2 {
				return MulDuration(rd, int64(li)), true
			}
			if lf, ok2 := left.(FloatValue); ok2 {
				return MulDurationFloat(rd, float64(lf)), true
			}
		}
	}
	// Temporal ± Duration → Temporal.
	if rd, rok := right.(DurationValue); rok {
		switch lv := left.(type) {
		case DateValue:
			if op == "+" {
				return AddDurationToDate(lv, rd), true
			}
			if op == "-" {
				return SubDurationFromDate(lv, rd), true
			}
		case LocalDateTimeValue:
			if op == "+" {
				return AddDurationToLocalDateTime(lv, rd), true
			}
			if op == "-" {
				return SubDurationFromLocalDateTime(lv, rd), true
			}
		case DateTimeValue:
			if op == "+" {
				return AddDurationToDateTime(lv, rd), true
			}
			if op == "-" {
				return SubDurationFromDateTime(lv, rd), true
			}
		case LocalTimeValue:
			if op == "+" {
				return AddDurationToLocalTime(lv, rd), true
			}
			if op == "-" {
				return SubDurationFromLocalTime(lv, rd), true
			}
		case TimeValue:
			if op == "+" {
				return AddDurationToTime(lv, rd), true
			}
			if op == "-" {
				return SubDurationFromTime(lv, rd), true
			}
		}
	}
	// Duration + Temporal (commutative add only).
	if ld, lok := left.(DurationValue); lok && op == "+" {
		switch rv := right.(type) {
		case DateValue:
			return AddDurationToDate(rv, ld), true
		case LocalDateTimeValue:
			return AddDurationToLocalDateTime(rv, ld), true
		case DateTimeValue:
			return AddDurationToDateTime(rv, ld), true
		case LocalTimeValue:
			return AddDurationToLocalTime(rv, ld), true
		case TimeValue:
			return AddDurationToTime(rv, ld), true
		}
	}
	// Temporal - Temporal → Duration (same kind only).
	if op == "-" {
		switch lv := left.(type) {
		case DateValue:
			if rv, ok := right.(DateValue); ok {
				return SubDates(lv, rv), true
			}
		case LocalDateTimeValue:
			if rv, ok := right.(LocalDateTimeValue); ok {
				return SubLocalDateTimes(lv, rv), true
			}
		case DateTimeValue:
			if rv, ok := right.(DateTimeValue); ok {
				return SubDateTimes(lv, rv), true
			}
		case LocalTimeValue:
			if rv, ok := right.(LocalTimeValue); ok {
				return SubLocalTimes(lv, rv), true
			}
		case TimeValue:
			if rv, ok := right.(TimeValue); ok {
				return SubTimes(lv, rv), true
			}
		}
	}
	return Null, false
}

func evalIntArith(op string, a, b int64) (Value, error) {
	switch op {
	case "+":
		// Overflow if both operands have the same sign and the result flips.
		if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
			return Null, &EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: integer overflow in %d + %d", a, b)}
		}
		return IntegerValue(a + b), nil
	case "-":
		// Overflow if b and a have opposite signs and the result flips.
		if (b > 0 && a < math.MinInt64+b) || (b < 0 && a > math.MaxInt64+b) {
			return Null, &EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: integer overflow in %d - %d", a, b)}
		}
		return IntegerValue(a - b), nil
	case "*":
		if a != 0 && b != 0 {
			// Use division to detect overflow; handles MinInt64 correctly because
			// we check both directions before committing.
			result := a * b
			if result/a != b {
				return Null, &EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: integer overflow in %d * %d", a, b)}
			}
		}
		return IntegerValue(a * b), nil
	case "/":
		if b == 0 {
			return Null, nil // division by zero → NULL in Cypher
		}
		if a == math.MinInt64 && b == -1 {
			return Null, &EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: %d / -1 is not representable as Int64", a)}
		}
		return IntegerValue(a / b), nil
	case "%":
		if b == 0 {
			return Null, nil
		}
		return IntegerValue(a % b), nil
	case "^":
		return FloatValue(math.Pow(float64(a), float64(b))), nil
	}
	return Null, nil
}

func evalFloatArith(op string, a, b float64) (Value, error) {
	switch op {
	case "+":
		return FloatValue(a + b), nil
	case "-":
		return FloatValue(a - b), nil
	case "*":
		return FloatValue(a * b), nil
	case "/":
		// Float division by zero → Inf/NaN (IEEE 754), not error.
		return FloatValue(a / b), nil
	case "%":
		return FloatValue(math.Mod(a, b)), nil
	case "^":
		return FloatValue(math.Pow(a, b)), nil
	}
	return Null, nil
}

// evalStringOp handles CONTAINS, STARTS WITH, ENDS WITH, =~.
func evalStringOp(op string, left, right Value) (Value, error) {
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	ls, lok := left.(StringValue)
	rs, rok := right.(StringValue)
	if !lok || !rok {
		return Null, nil
	}
	s, pattern := string(ls), string(rs)
	switch op {
	case "CONTAINS":
		return BoolValue(strings.Contains(s, pattern)), nil
	case "STARTS WITH":
		return BoolValue(strings.HasPrefix(s, pattern)), nil
	case "ENDS WITH":
		return BoolValue(strings.HasSuffix(s, pattern)), nil
	case "=~":
		// openCypher `=~` is an ANCHORED full-string match, equivalent to Java
		// java.util.regex.Matcher.matches(): the pattern must match the entire
		// subject string, not merely a substring of it. Go's regexp.MatchString
		// is an unanchored search (find), so we anchor the user pattern before
		// compiling: \A and \z are the absolute start/end of text. The
		// non-capturing group (?:…) binds any top-level alternation in the user
		// pattern to the anchors, so `a|b` becomes `\A(?:a|b)\z` rather than the
		// unsafe `\Aa|b\z` (= `(\Aa)|(b\z)`). \z (not the line anchor $) is used
		// deliberately so a trailing newline does NOT satisfy the match, matching
		// Java matches() semantics. Inline flags such as (?i) at the head of the
		// user pattern remain in scope within the group.
		//
		// The anchored source string is the cache key, so identical user
		// patterns hit the same cached compiled form (no double-anchoring), and
		// the cache stays bounded by the number of distinct user patterns.
		// An invalid pattern yields a compile error, which maps to NULL per
		// openCypher.
		re, err := regexCacheShared.compile(anchorRegexMatch(pattern))
		if err != nil {
			return Null, nil //nolint:nilerr // invalid pattern → NULL per openCypher
		}
		return BoolValue(re.MatchString(s)), nil
	}
	return Null, nil
}

// evalIn evaluates value IN list.
func evalIn(left, right Value) (Value, error) {
	if IsNull(right) {
		return Null, nil
	}
	list, ok := right.(ListValue)
	if !ok {
		return Null, nil
	}
	// Empty-list short-circuit: nothing can be IN [], so the answer
	// is unambiguously false — even for a NULL left operand. Without
	// this short-circuit, `null IN []` would fall through the
	// IsNull(left) branch below and return null, which contradicts
	// openCypher 9 §6.1 (Null3 [4] row 4).
	if len(list) == 0 {
		return BoolValue(false), nil
	}
	if IsNull(left) {
		return Null, nil
	}
	// Scan the list. Track whether we encountered any NULL to decide final result.
	sawNull := false
	for _, elem := range list {
		eq := left.Equal(elem)
		if IsTruthy(eq) {
			return BoolValue(true), nil
		}
		if IsNull(eq) {
			sawNull = true
		}
	}
	if sawNull {
		return Null, nil
	}
	return BoolValue(false), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Unary operator
// ─────────────────────────────────────────────────────────────────────────────

func evalUnaryOp(n *ast.UnaryOp, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) { //nolint:gocyclo // One case per unary operator; splitting would add indirection without reducing real complexity.
	switch n.Operator {
	case "IS NULL":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		return BoolValue(IsNull(operand)), nil

	case "IS NOT NULL":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		return BoolValue(!IsNull(operand)), nil

	case "NOT":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		if IsNull(operand) {
			return Null, nil
		}
		b, ok := operand.(BoolValue)
		if !ok {
			return Null, nil
		}
		return BoolValue(!bool(b)), nil

	case "-":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		if IsNull(operand) {
			return Null, nil
		}
		switch v := operand.(type) {
		case IntegerValue:
			if int64(v) == math.MinInt64 {
				return Null, &EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: integer overflow in -%d", int64(v))}
			}
			return IntegerValue(-int64(v)), nil
		case FloatValue:
			return FloatValue(-float64(v)), nil
		}
		return Null, nil

	case "+":
		operand, err := evalExpr(n.Operand, row, params, reg)
		if err != nil {
			return nil, err
		}
		return operand, nil

	default:
		return nil, &EvalError{Msg: fmt.Sprintf("unsupported unary operator %q", n.Operator)}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Function invocation
// ─────────────────────────────────────────────────────────────────────────────

func evalFunction(n *ast.FunctionInvocation, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	if reg == nil {
		return nil, &EvalError{Msg: fmt.Sprintf("no function registry; cannot call %s()", n.Name)}
	}

	// Resolve function name. Namespaced functions join with ".".
	name := strings.ToLower(n.Name)
	if len(n.Namespace) > 0 {
		parts := make([]string, 0, len(n.Namespace)+1)
		for _, ns := range n.Namespace {
			parts = append(parts, strings.ToLower(ns))
		}
		parts = append(parts, name)
		name = strings.Join(parts, ".")
	}

	// ── Quantifier functions (all, any, none, single) ──────────────────────────
	// These functions receive a ListComprehension as their sole argument from the
	// parser: all(x IN list WHERE pred). Evaluate the source list and the
	// predicate mask directly instead of folding to a filtered list, so that we
	// preserve the original element count.
	switch name {
	case "all", "any", "none", "single":
		if len(n.Args) == 1 {
			if lc, ok := n.Args[0].(*ast.ListComprehension); ok {
				return evalQuantifier(name, lc, row, params, reg)
			}
		}
		// Fall through to normal dispatch if args don't match the expected shape.
		// The registry function will handle type errors.

	// ── reduce() ──────────────────────────────────────────────────────────────
	// reduce(acc = init, x IN list | expr): special form with two sub-expressions.
	case "reduce":
		if len(n.Args) == 2 {
			if lc, ok := n.Args[1].(*ast.ListComprehension); ok {
				return evalReduce(n.Args[0], lc, row, params, reg)
			}
		}
	}

	fn, ok := reg.Resolve(name)
	if !ok {
		return nil, &EvalError{Msg: fmt.Sprintf("unknown function %q", name)}
	}

	args := make([]Value, len(n.Args))
	for i, arg := range n.Args {
		v, err := evalExpr(arg, row, params, reg)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return fn(args)
}

// evalQuantifier handles all(x IN list WHERE pred), any(...), none(...), single(...).
// It evaluates the source list and counts how many elements satisfy the predicate.
//
//nolint:gocyclo // Dispatch over 4 quantifier types × 3-4 count/null branches; extraction would obscure the logic.
func evalQuantifier(name string, lc *ast.ListComprehension, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	src, err := evalExpr(lc.Source, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(src) {
		return Null, nil
	}
	list, ok := src.(ListValue)
	if !ok {
		return Null, nil
	}

	counts, err := countQuantifierMatches(lc, list, row, params, reg)
	if err != nil {
		return nil, err
	}
	return quantifierResult(name, counts), nil
}

// quantifierCounts records the per-element predicate outcomes for a
// list quantifier (all/any/none/single). Each element contributes to
// exactly one counter — true, false, or null — and the total is the
// list length.
type quantifierCounts struct {
	trueCount  int
	falseCount int
	nullCount  int
	total      int
}

// countQuantifierMatches iterates the list and evaluates the predicate for each
// element, partitioning the outcomes into the (true, false, null) counters
// of [quantifierCounts].
func countQuantifierMatches(lc *ast.ListComprehension, list ListValue, row RowContext, params map[string]Value, reg FunctionRegistry) (quantifierCounts, error) {
	// ctx is smuggled through row by EvalWith; on the bare Eval path it is
	// context.Background() and the per-stride cancellation check never fires.
	ctx, _ := extractSubqueryContext(row)
	c := quantifierCounts{total: len(list)}
	for i, elem := range list {
		// Honour cancellation/deadline on a fixed stride so a quantifier
		// (all/any/none/single) over a large list is interruptible (#1477).
		if err := checkIterCtx(ctx, i); err != nil {
			return quantifierCounts{}, err
		}
		innerRow := make(RowContext, len(row)+1)
		for k, v := range row {
			innerRow[k] = v
		}
		innerRow[lc.Variable] = elem

		var predVal Value
		var err error
		if lc.Predicate != nil {
			predVal, err = evalExpr(lc.Predicate, innerRow, params, reg)
			if err != nil {
				return quantifierCounts{}, err
			}
		} else {
			predVal = BoolValue(true)
		}

		switch {
		case IsNull(predVal):
			c.nullCount++
		case IsTruthy(predVal):
			c.trueCount++
		default:
			c.falseCount++
		}
	}
	return c, nil
}

// quantifierResult converts the per-element counters to a 3VL boolean
// for the given quantifier name. The three-valued rules are:
//
//   - all:    FALSE if any element is false; TRUE if every element is
//     true with no nulls; otherwise NULL (mix of true + null,
//     or all-null, or empty list with at least one null).
//   - any:    TRUE if any element is true; FALSE if every element is
//     false; otherwise NULL (any nulls with no true).
//   - none:   TRUE if every element is false (or list is empty); FALSE
//     if any element is true; otherwise NULL.
//   - single: TRUE if exactly one element is true and no nulls; FALSE
//     if more than one element is true; otherwise NULL.
func quantifierResult(name string, c quantifierCounts) Value {
	switch name {
	case "all":
		if c.falseCount > 0 {
			return BoolValue(false)
		}
		if c.nullCount > 0 {
			return Null
		}
		return BoolValue(true)
	case "any":
		if c.trueCount > 0 {
			return BoolValue(true)
		}
		if c.nullCount > 0 {
			return Null
		}
		return BoolValue(false)
	case "none":
		if c.trueCount > 0 {
			return BoolValue(false)
		}
		if c.nullCount > 0 {
			return Null
		}
		return BoolValue(true)
	case "single":
		if c.trueCount > 1 {
			return BoolValue(false)
		}
		if c.nullCount > 0 {
			return Null
		}
		return BoolValue(c.trueCount == 1)
	}
	return Null
}

// evalReduceExpr handles the *ast.ReduceExpr AST node produced by the parser
// for reduce(acc = init, x IN list | expr). This is the primary eval path.
func evalReduceExpr(n *ast.ReduceExpr, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	acc, err := evalExpr(n.Init, row, params, reg)
	if err != nil {
		return nil, err
	}
	src, err := evalExpr(n.Source, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(src) {
		return Null, nil
	}
	list, ok := src.(ListValue)
	if !ok {
		return acc, nil
	}
	// ctx is smuggled through row by EvalWith; on the bare Eval path it is
	// context.Background() and the per-stride cancellation check never fires.
	// The per-evaluation list-element budget that bounds a list-growing
	// accumulator (reduce(acc=[0], … | acc + acc)) is charged inside evalArith
	// before each concat allocates (#1475); this loop adds the cancellation
	// check (#1477).
	ctx, _ := extractSubqueryContext(row)
	for i, elem := range list {
		if err := checkIterCtx(ctx, i); err != nil {
			return nil, err
		}
		innerRow := make(RowContext, len(row)+2)
		for k, v := range row {
			innerRow[k] = v
		}
		innerRow[n.AccVar] = acc
		innerRow[n.ElemVar] = elem
		acc, err = evalExpr(n.Projection, innerRow, params, reg)
		if err != nil {
			return nil, err
		}
	}
	return acc, nil
}

// evalReduce handles reduce(acc = init, x IN list | expr).
// The parser produces: FunctionInvocation{Name: "reduce", Args: [initExpr, ListComprehension{...}]}
// where ListComprehension has a Projection (the accumulator expression) and a Source (the list).
func evalReduce(initExpr ast.Expression, lc *ast.ListComprehension, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	acc, err := evalExpr(initExpr, row, params, reg)
	if err != nil {
		return nil, err
	}
	src, err := evalExpr(lc.Source, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(src) {
		return acc, nil
	}
	list, ok := src.(ListValue)
	if !ok {
		return acc, nil
	}
	if lc.Projection == nil {
		return acc, nil
	}

	// The accumulator variable name is stored in the ListComprehension's
	// variable; the element variable is in the inner row.
	// reduce(acc = init, x IN list | acc + x) →
	//   lc.Variable = "x", lc.Projection = acc + x, initExpr = init
	// However, the parser stores the accumulator variable separately.
	// In the current AST, there is no separate accumulator variable field.
	// The convention used by the visitor is: the initExpr's Variable name is the
	// accumulator. We detect this by looking at the initExpr AST node.
	//
	// Since the exact AST shape depends on how the parser emits reduce(), and
	// that shape is not documented in the visible code, we implement a best-effort
	// reduction: the loop variable iterates over the list and the accumulator
	// is accessible as an outer variable in the row.
	accVarName := "_acc"
	if v, ok := initExpr.(*ast.Variable); ok {
		accVarName = v.Name
	}

	// ctx is smuggled through row by EvalWith; on the bare Eval path it is
	// context.Background() and the per-stride cancellation check never fires.
	// The per-evaluation list-element budget is charged inside evalArith before
	// each concat allocates (#1475); this loop adds the cancellation check (#1477).
	ctx, _ := extractSubqueryContext(row)
	for i, elem := range list {
		if err := checkIterCtx(ctx, i); err != nil {
			return nil, err
		}
		innerRow := make(RowContext, len(row)+2)
		for k, v := range row {
			innerRow[k] = v
		}
		innerRow[accVarName] = acc
		innerRow[lc.Variable] = elem

		acc, err = evalExpr(lc.Projection, innerRow, params, reg)
		if err != nil {
			return nil, err
		}
	}
	return acc, nil
}

// isFloatNaN reports whether v is FloatValue and a NaN. Other kinds
// return false; the NaN check is deliberately limited to FloatValue
// so IntegerValue / StringValue / etc. fall through to normal ordering.
func isFloatNaN(v Value) bool {
	if f, ok := v.(FloatValue); ok {
		return math.IsNaN(float64(f))
	}
	return false
}
