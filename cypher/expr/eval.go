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

// BoundedCountEvaluator is an OPTIONAL capability a [SubqueryEvaluator] may
// also implement. When it does, a COUNT { … } compared against an integer
// literal is evaluated with a ceiling instead of exactly, so an implementation
// able to stop counting early can do so (rmp #2232, mirroring Neo4j's
// short-circuiting HasDegree family over its plain GetDegree).
//
// # Why a cap of literal+1 decides every comparison
//
// Let T be the true count, k the literal and C = min(T, k+1) the capped count.
// For each of the six comparison operators, `T op k` and `C op k` agree:
//
//   - T > k:  T ≤ k ⟹ C = T, same verdict; T ≥ k+1 ⟹ C = k+1 > k, both true.
//   - T ≥ k:  T ≤ k ⟹ C = T; T > k ⟹ C = k+1 ≥ k, both true.
//   - T < k:  T ≤ k ⟹ C = T; T > k ⟹ C = k+1 ≮ k, both false.
//   - T ≤ k:  T ≤ k ⟹ C = T; T > k ⟹ C = k+1 > k, both false.
//   - T = k:  T ≤ k ⟹ C = T; T > k ⟹ C = k+1 ≠ k, both false.
//   - T ≠ k:  the negation of the previous line.
//
// A negative k is covered too, since T ≥ 0 always and the cap clamps to 0.
//
// An implementation that cannot exploit the cap must still return the EXACT
// count; the cap is permission to stop early, never a licence to under-report
// below it.
type BoundedCountEvaluator interface {
	// EvalCountBounded is [SubqueryEvaluator.EvalCount] returning
	// min(trueCount, limit) when limit >= 0, and the exact count when limit < 0.
	EvalCountBounded(ctx context.Context, sub *ast.CountSubquery, row RowContext, params map[string]Value, limit int64) (Value, error)
}

// PatternCountEvaluator is an OPTIONAL capability a [PatternEvaluator] may also
// implement, letting `size([ (a)-[:R]->(b) | … ])` be answered without building
// the list. Neo4j's getDegreeRewriter treats Size(ListIRExpression) as
// count-like for exactly this reason: the projection cannot change how MANY
// matches there are, so a degree answers it (rmp #2232).
//
// ok is false when the pattern is not answerable this way, in which case the
// caller builds the list and takes its length as before.
type PatternCountEvaluator interface {
	// CountPatternComp returns the number of matches of pc's pattern for row.
	CountPatternComp(ctx context.Context, pc *ast.PatternComprehension, row RowContext) (Value, bool, error)
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

// chargeListGrowth charges n newly-materialised list elements against the
// per-evaluation budget st carries. On the bare [Eval] path (st == nil) there
// is no cumulative budget, so it instead enforces an intrinsic per-call ceiling
// of [DefaultMaxListElements] on n alone, which still rejects a single
// oversized list. It returns a typed [EvalError] on breach.
func chargeListGrowth(st *evalCallState, n int64) error {
	if st != nil {
		return st.budget.charge(n)
	}
	if n > DefaultMaxListElements {
		return errListTooLarge(DefaultMaxListElements)
	}
	return nil
}

// chargeStringGrowth charges n newly-materialised string bytes against the
// per-evaluation byte budget st carries. On the bare [Eval] path (st == nil) it
// instead enforces an intrinsic per-call ceiling of [DefaultMaxStringEvalBytes]
// on n alone, which still rejects a single oversized concatenation. It returns
// a typed [EvalError] on breach (#1482).
func chargeStringGrowth(st *evalCallState, n int64) error {
	if st != nil {
		return st.budget.chargeBytes(n)
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

// checkIterCtx polls the evaluation's context for cancellation when iter is a
// multiple of [ctxIterCheckStride]. It returns the context error
// (context.Canceled / context.DeadlineExceeded) promptly so a long in-expression
// loop — reduce(), a comprehension, or a quantifier over a large list — can be
// aborted by a caller's deadline or cancellation. On the bare [Eval] path the
// context is context.Background() and the check never fires.
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
	return evalExpr(expr, row, nil, params, reg)
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
	// The per-evaluation state is an ordinary local threaded down the recursive
	// evaluator as an explicit parameter (#2653). It used to ride inside the
	// RowContext under a reserved NUL-bracketed sentinel key, which cost three
	// string-keyed map operations on every single evaluation — a save, an
	// install and an erase — plus a pooled holder to acquire and release. A
	// CPU profile of `MATCH (p:Person) RETURN p.bucket` over 120 000 nodes
	// attributed 7.83% of all samples to exactly that bookkeeping, and an exact
	// counter compiled into the three sentinel readers showed the smuggled
	// value was read ZERO times on that shape: the key was written and erased
	// for nothing. At a one-variable row schema it was also half the map's
	// keys, so it distorted the cost of real variable binding alongside it.
	//
	// Re-entrancy is now structural rather than arranged. A nested EvalWith —
	// reached through a pattern comprehension, which re-enters via
	// [PatternEvaluator] — builds its own state in its own frame, so the outer
	// call's context, evaluators and budget are untouched by construction and
	// need no save/restore. That reproduces the previous nesting behaviour
	// exactly: the old code likewise gave each nested call a fresh holder with
	// a fresh budget, and restored the outer holder on return.
	//
	// A nil RowContext (a legal input: an expression with no variable bindings)
	// now stays nil, because there is no longer anything to carry. That removes
	// the pooled one-entry map binding-free expressions used to need on every
	// evaluated row.
	st := evalCallState{
		ctx: ctx,
		sub: subEval,
		pat: patEval,
		budget: evalBudget{
			remaining:      DefaultMaxListElements,
			limit:          DefaultMaxListElements,
			bytesRemaining: DefaultMaxStringEvalBytes,
			bytesLimit:     DefaultMaxStringEvalBytes,
		},
	}
	return evalExpr(expr, row, &st, params, reg)
}

// evalCallState is the per-evaluation state [EvalWith] threads through the
// recursive evaluator: the cancellation context, the optional subquery and
// pattern evaluators, and the cumulative list/string growth budget that bounds
// what one evaluation may materialise (#1475, #1482).
//
// It is passed by pointer purely so the budget is shared across the whole
// evaluation — every helper reads it, and [evalBudget.charge] mutates it — not
// because it is heap state: the value lives in [EvalWith]'s frame, no callee
// retains it, and it never crosses an evaluator interface boundary (the
// evaluators receive st.ctx, never st), so escape analysis keeps it on the
// stack.
//
// A nil *evalCallState is the bare [Eval] path: no context, no evaluators, and
// no cumulative budget. Every accessor below is nil-safe so both paths share
// one call graph, exactly as the absent sentinel used to make them.
type evalCallState struct {
	ctx    context.Context //nolint:containedctx // per-evaluation cancellation scope, not stored beyond the call; see EvalWith
	sub    SubqueryEvaluator
	pat    PatternEvaluator
	budget evalBudget
}

// evalContext returns the evaluation's cancellation context, or
// context.Background() on the bare [Eval] path.
func (st *evalCallState) evalContext() context.Context {
	if st == nil {
		return context.Background()
	}
	return st.ctx
}

// subquery returns the evaluation's context and [SubqueryEvaluator], or
// (context.Background(), nil) on the bare [Eval] path.
func (st *evalCallState) subquery() (context.Context, SubqueryEvaluator) {
	if st == nil {
		return context.Background(), nil
	}
	return st.ctx, st.sub
}

// pattern returns the evaluation's context and [PatternEvaluator], or
// (context.Background(), nil) on the bare [Eval] path.
func (st *evalCallState) pattern() (context.Context, PatternEvaluator) {
	if st == nil {
		return context.Background(), nil
	}
	return st.ctx, st.pat
}

func evalExpr(e ast.Expression, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) { // Main dispatch switch; all branches are simple delegations and cannot be split without obscuring the type mapping.
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
		return evalListLiteral(n, row, st, params, reg)
	case *ast.MapLiteral:
		return evalMapLiteral(n, row, st, params, reg)

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
		return evalProperty(n, row, st, params, reg)

	// ── Label predicate ────────────────────────────────────────────────────────
	case *ast.LabelPredicate:
		return evalLabelPredicate(n, row, st, params, reg)

	// ── Subscript access ───────────────────────────────────────────────────────
	case *ast.SubscriptExpr:
		return evalSubscript(n, row, st, params, reg)

	// ── Slice access ───────────────────────────────────────────────────────────
	case *ast.SliceExpr:
		return evalSlice(n, row, st, params, reg)

	// ── List comprehension ─────────────────────────────────────────────────────
	case *ast.ListComprehension:
		return evalListComprehension(n, row, st, params, reg)

	// ── Map projection ─────────────────────────────────────────────────────────
	case *ast.MapProjection:
		return evalMapProjection(n, row, st, params, reg)

	// ── Binary operator ────────────────────────────────────────────────────────
	case *ast.BinaryOp:
		return evalBinaryOp(n, row, st, params, reg)

	// ── Unary operator ─────────────────────────────────────────────────────────
	case *ast.UnaryOp:
		return evalUnaryOp(n, row, st, params, reg)

	// ── CASE expression ────────────────────────────────────────────────────────
	case *ast.CaseExpression:
		return evalCase(n, row, st, params, reg)

	// ── Function call ──────────────────────────────────────────────────────────
	case *ast.FunctionInvocation:
		return evalFunction(n, row, st, params, reg)

	// ── EXISTS { … } subquery ──────────────────────────────────────────────────
	case *ast.ExistsSubquery:
		ctx, subEval := st.subquery()
		if subEval == nil {
			return nil, &EvalError{Msg: "EXISTS { … } subquery is not supported in this evaluation context (no SubqueryEvaluator wired)"}
		}
		return subEval.EvalExists(ctx, n, row, params)

	// ── COUNT { … } subquery ───────────────────────────────────────────────────
	case *ast.CountSubquery:
		ctx, subEval := st.subquery()
		if subEval == nil {
			return nil, &EvalError{Msg: "COUNT { … } subquery is not supported in this evaluation context (no SubqueryEvaluator wired)"}
		}
		return subEval.EvalCount(ctx, n, row, params)

	// ── Pattern predicate (existential check) ─────────────────────────────────
	// WHERE (a)-[:T]->(b) is an existential check: true iff at least one path
	// matching the pattern exists in the graph given the bindings in row.
	case *ast.PathPattern:
		ctx, patEval := st.pattern()
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
		ctx, patEval := st.pattern()
		if patEval == nil {
			return nil, &EvalError{Msg: "pattern comprehension is not supported in this evaluation context (no PatternEvaluator wired)"}
		}
		return patEval.EvalPatternComp(ctx, n, row, params, reg)

	case *ast.ReduceExpr:
		return evalReduceExpr(n, row, st, params, reg)

	default:
		return nil, &EvalError{Msg: fmt.Sprintf("unsupported expression type %T", e)}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Composite literals
// ─────────────────────────────────────────────────────────────────────────────

func evalListLiteral(n *ast.ListLiteral, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	result := make(ListValue, len(n.Elements))
	for i, elem := range n.Elements {
		v, err := evalExpr(elem, row, st, params, reg)
		if err != nil {
			return nil, err
		}
		result[i] = v
	}
	return result, nil
}

func evalMapLiteral(n *ast.MapLiteral, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	result := make(MapValue, len(n.Keys))
	for i, k := range n.Keys {
		v, err := evalExpr(n.Values[i], row, st, params, reg)
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
func evalLabelPredicate(n *ast.LabelPredicate, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	recv, err := evalExpr(n.Receiver, row, st, params, reg)
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
	case *LazyRelationshipValue:
		// Lazy relationship: the type is resolved eagerly by the engine
		// (it is one cheap label read, not a property map), so this is the
		// identical conjunctive walk over the identical string.
		for _, want := range n.Labels {
			if r.RelType() != want {
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

func evalProperty(n *ast.Property, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	recv, err := evalExpr(n.Receiver, row, st, params, reg)
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
	case *LazyRelationshipValue:
		// Lazy relationship fast path: resolve only the touched property from
		// storage. A LazyRelationshipValue is constructed solely for
		// relationships the static analysis proved are accessed only through
		// scalar accessors, and never for an entity deleted in the same
		// statement (the DELETE operator stamps those as a Deleted
		// RelationshipValue, which the engine forwards before the lazy path is
		// reached and which the branch above handles), so the
		// DeletedEntityAccess contract is preserved without a flag here. A
		// missing key reads as Null (Property does this).
		return r.Property(n.Key), nil
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

func evalSubscript(n *ast.SubscriptExpr, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	container, err := evalExpr(n.Expr, row, st, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(container) {
		return Null, nil
	}
	idx, err := evalExpr(n.Index, row, st, params, reg)
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
	case *LazyRelationshipValue:
		// Lazy relationship subscript: only a String key is valid (same
		// TypeError surface as the eager RelationshipValue branch). The static
		// analysis only produces a LazyRelationshipValue when subscripts use
		// literal-string keys, but resolving any runtime String key on demand is
		// equally sound.
		sk, ok := idx.(StringValue)
		if !ok {
			return nil, &EvalError{Msg: fmt.Sprintf("MapElementAccessByNonString: map key must be String, got %s", idx.Kind())}
		}
		return c.Property(string(sk)), nil
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

func evalBinaryOp(n *ast.BinaryOp, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) { // One case per binary operator; splitting would obscure the operator mapping without reducing real complexity.
	// AND and OR short-circuit under 3VL before evaluating right.
	switch n.Operator {
	case "AND":
		return eval3VLAND(n, row, st, params, reg)
	case "OR":
		return eval3VLOR(n, row, st, params, reg)
	}

	// A COUNT { … } compared against an integer literal never needs the exact
	// count — only enough of it to decide the comparison. When the wired
	// SubqueryEvaluator can stop early, give it the ceiling (#2232). See
	// [BoundedCountEvaluator] for why literal+1 is sufficient for all six
	// operators.
	left, right, done, err := evalBoundedCountComparison(n, row, st, params, reg)
	if err != nil {
		return nil, err
	}
	if !done {
		left, err = evalExpr(n.Left, row, st, params, reg)
		if err != nil {
			return nil, err
		}
		right, err = evalExpr(n.Right, row, st, params, reg)
		if err != nil {
			return nil, err
		}
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
		return evalArith(st, "+", left, right)
	case "-":
		return evalArith(st, "-", left, right)
	case "*":
		return evalArith(st, "*", left, right)
	case "/":
		return evalArith(st, "/", left, right)
	case "%":
		return evalArith(st, "%", left, right)
	case "^":
		return evalArith(st, "^", left, right)

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

// logicalOperandError reports the InvalidArgumentType error to raise when v is
// used as the operand of a logical operator (AND/OR/XOR/NOT) but is not a legal
// three-valued-logic operand. openCypher treats NULL as a legal logical operand
// (it drives the Kleene truth tables), so only a non-null, non-Boolean value is
// an error. It returns nil when v is legal (Boolean or NULL).
//
// The compile-time guard in cypher/sema (invalidBooleanOperandError) rejects
// non-boolean literal operands before evaluation; this covers the runtime
// values it cannot see — parameters, properties, and variables. The message
// carries the "InvalidArgumentType:" prefix used by every other runtime type
// error in this package (property access, subscripting), so it maps to the same
// InvalidArgumentType taxonomy (#2059).
func logicalOperandError(op string, v Value) error {
	if IsNull(v) {
		return nil
	}
	if _, ok := v.(BoolValue); ok {
		return nil
	}
	return &EvalError{Msg: fmt.Sprintf("InvalidArgumentType: operator %q expects Boolean operands, got %s", op, v.Kind())}
}

// eval3VLAND evaluates AND with Kleene 3VL short-circuit.
func eval3VLAND(n *ast.BinaryOp, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	left, err := evalExpr(n.Left, row, st, params, reg)
	if err != nil {
		return nil, err
	}
	// false AND _ = false (short-circuit, even over NULL): the right operand is
	// left unevaluated, so a non-boolean right is never type-checked here.
	if b, ok := left.(BoolValue); ok && !bool(b) {
		return BoolValue(false), nil
	}
	// left was evaluated and did not short-circuit: it must be Boolean or NULL.
	if err := logicalOperandError(n.Operator, left); err != nil {
		return nil, err
	}
	right, err := evalExpr(n.Right, row, st, params, reg)
	if err != nil {
		return nil, err
	}
	// false AND _ = false
	if b, ok := right.(BoolValue); ok && !bool(b) {
		return BoolValue(false), nil
	}
	if err := logicalOperandError(n.Operator, right); err != nil {
		return nil, err
	}
	// left and right are each Boolean(true) or NULL at this point.
	// NULL AND NULL = NULL; NULL AND true = NULL
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	return BoolValue(true), nil
}

// eval3VLOR evaluates OR with Kleene 3VL short-circuit.
func eval3VLOR(n *ast.BinaryOp, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	left, err := evalExpr(n.Left, row, st, params, reg)
	if err != nil {
		return nil, err
	}
	// true OR _ = true (short-circuit, even over NULL): the right operand is
	// left unevaluated, so a non-boolean right is never type-checked here.
	if IsTruthy(left) {
		return BoolValue(true), nil
	}
	// left was evaluated and did not short-circuit: it must be Boolean or NULL.
	if err := logicalOperandError(n.Operator, left); err != nil {
		return nil, err
	}
	right, err := evalExpr(n.Right, row, st, params, reg)
	if err != nil {
		return nil, err
	}
	// true OR _ = true
	if IsTruthy(right) {
		return BoolValue(true), nil
	}
	if err := logicalOperandError(n.Operator, right); err != nil {
		return nil, err
	}
	// left and right are each Boolean(false) or NULL at this point.
	// NULL OR false = NULL; NULL OR NULL = NULL
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	return BoolValue(false), nil
}

// eval3VLXOR evaluates XOR with 3VL: NULL XOR _ = NULL. XOR does not
// short-circuit, so both operands are always evaluated; a non-null, non-Boolean
// operand is a type error rather than being coerced to NULL (#2059).
func eval3VLXOR(left, right Value) (Value, error) {
	if err := logicalOperandError("XOR", left); err != nil {
		return nil, err
	}
	if err := logicalOperandError("XOR", right); err != nil {
		return nil, err
	}
	if IsNull(left) || IsNull(right) {
		return Null, nil
	}
	// Both operands are non-null and (per the guard above) Boolean.
	//nolint:forcetypeassert // logicalOperandError (eval.go:921-929) returns nil only for Null or BoolValue, and BOTH operands pass it (eval.go:1003, 1006) before Null is ruled out at eval.go:1009
	return BoolValue(bool(left.(BoolValue)) != bool(right.(BoolValue))), nil
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
		al, _ := a.(ListValue) // kind pre-checked
		bl, _ := b.(ListValue) // kind pre-checked
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
func promoteNumeric(a, b Value) (Value, Value) { // Named returns would add noise; caller always destructures both values.
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

// evalArith evaluates arithmetic binary operators. st carries the
// per-evaluation list-element and string-byte budgets (#1475, #1482), consulted
// before any list concatenation or string growth allocates so a doubling
// accumulator (reduce(acc=[0], … | acc + acc)) is rejected with a typed
// [EvalError] rather than allocating an exponentially large slice. It is the
// only thing this function needs from the evaluation, which is why it takes no
// row: before #2653 the budget travelled inside the RowContext, so the row had
// to be passed here purely to reach it.
func evalArith(st *evalCallState, op string, left, right Value) (Value, error) {
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
				if err := chargeStringGrowth(st, int64(len(ls))+int64(len(rs))); err != nil {
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
				if err := chargeListGrowth(st, int64(len(ll))+int64(len(rl))); err != nil {
					return nil, err
				}
				result := make(ListValue, len(ll)+len(rl))
				copy(result, ll)
				copy(result[len(ll):], rl)
				return result, nil
			}
			// list + element: wrap right in a single-element list and append.
			if err := chargeListGrowth(st, int64(len(ll))+1); err != nil {
				return nil, err
			}
			result := make(ListValue, len(ll)+1)
			copy(result, ll)
			result[len(ll)] = right
			return result, nil
		}
		if rl, rok := right.(ListValue); rok {
			// element + list: prepend left to right.
			if err := chargeListGrowth(st, int64(len(rl))+1); err != nil {
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
// One branch per (kind, op) pair; splitting hides the pattern.
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
			// Integer division by zero raises (matches Neo4j; openCypher leaves
			// it implementation-defined). Float /0 stays IEEE-754 (+Inf), handled
			// in the float arithmetic path. (#1766)
			return Null, &EvalError{Msg: "ArithmeticError: / by zero"}
		}
		if a == math.MinInt64 && b == -1 {
			return Null, &EvalError{Msg: fmt.Sprintf("ArithmeticOverflow: %d / -1 is not representable as Int64", a)}
		}
		return IntegerValue(a / b), nil
	case "%":
		if b == 0 {
			// Integer modulo by zero raises (matches Neo4j). Float %0 stays
			// IEEE-754 (NaN), handled in the float arithmetic path. (#1766)
			return Null, &EvalError{Msg: "ArithmeticError: % by zero"}
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

func evalUnaryOp(n *ast.UnaryOp, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) { // One case per unary operator; splitting would add indirection without reducing real complexity.
	switch n.Operator {
	case "IS NULL":
		operand, err := evalExpr(n.Operand, row, st, params, reg)
		if err != nil {
			return nil, err
		}
		return BoolValue(IsNull(operand)), nil

	case "IS NOT NULL":
		operand, err := evalExpr(n.Operand, row, st, params, reg)
		if err != nil {
			return nil, err
		}
		return BoolValue(!IsNull(operand)), nil

	case "NOT":
		operand, err := evalExpr(n.Operand, row, st, params, reg)
		if err != nil {
			return nil, err
		}
		if IsNull(operand) {
			return Null, nil
		}
		// A non-null operand must be Boolean; a non-boolean runtime value
		// (parameter, property, variable) is a type error, not NULL (#2059).
		if err := logicalOperandError(n.Operator, operand); err != nil {
			return nil, err
		}
		//nolint:forcetypeassert // logicalOperandError (eval.go:921-929) returns nil only for Null or BoolValue, and the operand passes it before Null is ruled out earlier in this function
		return BoolValue(!bool(operand.(BoolValue))), nil

	case "-":
		operand, err := evalExpr(n.Operand, row, st, params, reg)
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
		operand, err := evalExpr(n.Operand, row, st, params, reg)
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

// evalBoundedCountComparison recognises `COUNT { … } <cmp> <integer literal>`
// in either operand order and evaluates the COUNT with a ceiling, so an
// evaluator able to stop counting early does. done is false when the shape does
// not apply or no [BoundedCountEvaluator] is wired, in which case the caller
// evaluates both operands normally.
//
// The returned operands keep the ORIGINAL left/right positions, so the operator
// switch that follows is unchanged and no comparison is reversed.
func evalBoundedCountComparison(n *ast.BinaryOp, row RowContext, st *evalCallState, params map[string]Value, _ FunctionRegistry) (left, right Value, done bool, err error) {
	switch n.Operator {
	case "=", "<>", "<", "<=", ">", ">=":
	default:
		return nil, nil, false, nil
	}
	// NOTE the naming: these report which SIDE carries the COUNT, not which side
	// carries the literal. Getting that backwards swaps the operands and inverts
	// every comparison — it did, and the differential suite missed it because
	// both arms of the comparison went through this same code. The identity
	// suite now also asserts absolute expected values for exactly that reason.
	countOnLeft, isCountLeft := n.Left.(*ast.CountSubquery)
	countOnRight, isCountRight := n.Right.(*ast.CountSubquery)
	// Exactly one side must be the COUNT; `COUNT{…} = COUNT{…}` has no literal
	// to bound against.
	if isCountLeft == isCountRight {
		return nil, nil, false, nil
	}
	sub, other := countOnLeft, n.Right
	if isCountRight {
		sub, other = countOnRight, n.Left
	}
	k, ok := integerLiteralValue(other)
	if !ok {
		return nil, nil, false, nil
	}
	ctx, subEval := st.subquery()
	if subEval == nil {
		return nil, nil, false, nil
	}
	bounded, capable := subEval.(BoundedCountEvaluator)
	if !capable {
		return nil, nil, false, nil
	}
	limit := k + 1
	if limit < 0 {
		limit = 0
	}
	counted, cerr := bounded.EvalCountBounded(ctx, sub, row, params, limit)
	if cerr != nil {
		return nil, nil, false, cerr
	}
	// Put each operand back in its ORIGINAL position so the operator switch that
	// follows compares them the way the query wrote them.
	lit := IntegerValue(k)
	if isCountRight {
		return lit, counted, true, nil
	}
	return counted, lit, true, nil
}

// integerLiteralValue extracts a signed integer literal, including one behind a
// unary minus, and reports false for anything else.
func integerLiteralValue(e ast.Expression) (int64, bool) {
	switch v := e.(type) {
	case *ast.IntLiteral:
		return v.Value, true
	case *ast.UnaryOp:
		if v.Operator == "-" {
			if inner, ok := integerLiteralValue(v.Operand); ok {
				return -inner, true
			}
		}
	}
	return 0, false
}

func evalFunction(n *ast.FunctionInvocation, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
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
	// ── size([ (a)-[:R]->(b) | … ]) ────────────────────────────────────────────
	// A pattern comprehension's length is a COUNT of matches; the projection
	// cannot change it. When the wired PatternEvaluator can answer that from a
	// degree it does, and the list is never built (#2232). Falls through to the
	// ordinary path — build the list, take its length — whenever it cannot.
	if name == "size" && len(n.Args) == 1 {
		if pc, ok := n.Args[0].(*ast.PatternComprehension); ok {
			if ctx, patEval := st.pattern(); patEval != nil {
				if pce, capable := patEval.(PatternCountEvaluator); capable {
					v, answered, err := pce.CountPatternComp(ctx, pc, row)
					if err != nil {
						return nil, err
					}
					if answered {
						return v, nil
					}
				}
			}
		}
	}

	switch name {
	case "all", "any", "none", "single":
		if len(n.Args) == 1 {
			if lc, ok := n.Args[0].(*ast.ListComprehension); ok {
				return evalQuantifier(name, lc, row, st, params, reg)
			}
		}
		// Fall through to normal dispatch if args don't match the expected shape.
		// The registry function will handle type errors.

	// ── reduce() ──────────────────────────────────────────────────────────────
	// reduce(acc = init, x IN list | expr): special form with two sub-expressions.
	case "reduce":
		if len(n.Args) == 2 {
			if lc, ok := n.Args[1].(*ast.ListComprehension); ok {
				return evalReduce(n.Args[0], lc, row, st, params, reg)
			}
		}
	}

	fn, ok := reg.Resolve(name)
	if !ok {
		return nil, &EvalError{Msg: fmt.Sprintf("unknown function %q", name)}
	}

	args := make([]Value, len(n.Args))
	for i, arg := range n.Args {
		v, err := evalExpr(arg, row, st, params, reg)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	result, err := fn(args)
	if err != nil {
		return nil, err
	}
	// A registry function that materialises a list (range, split, keys, labels,
	// nodes, …) is dispatched here without threading the per-evaluation
	// list-element budget, so charge its result length against that budget now.
	// This bounds a single call (an untrusted range(1, 1e8) would otherwise
	// allocate ~2.4 GB) and, because the budget is cumulative across one row
	// evaluation, also closes multi-column compounding
	// (RETURN range(1, N), range(1, N), …). Comprehensions and reduce() are
	// handled above and charge their growth directly, so they never reach this
	// path; the charge here is conservative (a function returning an existing
	// list re-charges it) but the ceiling is DefaultMaxListElements, five orders
	// of magnitude above any TCK-covered list, so it never rejects a legitimate
	// query.
	if lv, ok := result.(ListValue); ok {
		if cerr := chargeListGrowth(st, int64(len(lv))); cerr != nil {
			return nil, cerr
		}
	}
	return result, nil
}

// evalQuantifier handles all(x IN list WHERE pred), any(...), none(...), single(...).
// It evaluates the source list and counts how many elements satisfy the predicate.
//
// Dispatch over 4 quantifier types × 3-4 count/null branches; extraction would obscure the logic.
func evalQuantifier(name string, lc *ast.ListComprehension, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	src, err := evalExpr(lc.Source, row, st, params, reg)
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

	counts, err := countQuantifierMatches(lc, list, row, st, params, reg)
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
func countQuantifierMatches(lc *ast.ListComprehension, list ListValue, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (quantifierCounts, error) {
	// ctx comes from the evaluation state; on the bare Eval path it is
	// context.Background() and the per-stride cancellation check never fires.
	ctx := st.evalContext()
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
			predVal, err = evalExpr(lc.Predicate, innerRow, st, params, reg)
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
func evalReduceExpr(n *ast.ReduceExpr, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	acc, err := evalExpr(n.Init, row, st, params, reg)
	if err != nil {
		return nil, err
	}
	src, err := evalExpr(n.Source, row, st, params, reg)
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
	// ctx comes from the evaluation state; on the bare Eval path it is
	// context.Background() and the per-stride cancellation check never fires.
	// The per-evaluation list-element budget that bounds a list-growing
	// accumulator (reduce(acc=[0], … | acc + acc)) is charged inside evalArith
	// before each concat allocates (#1475); this loop adds the cancellation
	// check (#1477).
	ctx := st.evalContext()
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
		acc, err = evalExpr(n.Projection, innerRow, st, params, reg)
		if err != nil {
			return nil, err
		}
		// reduce can deepen the accumulator by one nesting level per iteration
		// (reduce(acc=[0], … | [acc])) while charging only one element against
		// the element budget, so depth escapes the element ceiling. Reject an
		// over-deep/over-large accumulator early — on the same stride as the
		// cancellation check — before it can overflow a downstream recursive
		// walker's stack (#value-depth). The loop-end check below is the
		// backstop for a reduce that ends before the first stride.
		if i%ctxIterCheckStride == 0 && ExceedsValueDepth(acc) {
			return nil, errValueTooDeep()
		}
	}
	if ExceedsValueDepth(acc) {
		return nil, errValueTooDeep()
	}
	return acc, nil
}

// evalReduce handles reduce(acc = init, x IN list | expr).
// The parser produces: FunctionInvocation{Name: "reduce", Args: [initExpr, ListComprehension{...}]}
// where ListComprehension has a Projection (the accumulator expression) and a Source (the list).
func evalReduce(initExpr ast.Expression, lc *ast.ListComprehension, row RowContext, st *evalCallState, params map[string]Value, reg FunctionRegistry) (Value, error) {
	acc, err := evalExpr(initExpr, row, st, params, reg)
	if err != nil {
		return nil, err
	}
	src, err := evalExpr(lc.Source, row, st, params, reg)
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

	// ctx comes from the evaluation state; on the bare Eval path it is
	// context.Background() and the per-stride cancellation check never fires.
	// The per-evaluation list-element budget is charged inside evalArith before
	// each concat allocates (#1475); this loop adds the cancellation check (#1477).
	ctx := st.evalContext()
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

		acc, err = evalExpr(lc.Projection, innerRow, st, params, reg)
		if err != nil {
			return nil, err
		}
		// See evalReduceExpr: bound accumulator nesting depth so a reduce
		// cannot build a value deep enough to overflow a downstream walker.
		if i%ctxIterCheckStride == 0 && ExceedsValueDepth(acc) {
			return nil, errValueTooDeep()
		}
	}
	if ExceedsValueDepth(acc) {
		return nil, errValueTooDeep()
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
