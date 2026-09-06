package cypher

// write_build_scaffold.go — rmp #2660: the expression-level evaluators a
// WRITING statement's physical build needs.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// writeEvalScaffold is the write path's counterpart to the two evaluator fields
// of [readBuildScaffold]: the per-statement [expr.SubqueryEvaluator] and
// [expr.PatternEvaluator] that [evalRow] dispatches EXISTS { … }, COUNT { … }
// and bare pattern-predicate expressions to.
//
// # Why it exists
//
// [buildPlanWithMutatorFull] built its [buildOpts] with neither evaluator set,
// so [evalRow] saw `subEval == nil && patEval == nil` and degraded to the bare
// [expr.Eval] path for EVERY expression in a statement that writes. Three
// constructs that work in the identical read-only statement therefore refused to
// run — not a wrong answer, a typed error:
//
//   - a pattern predicate in a WHERE that follows a write clause
//     ("pattern predicate is not supported in this evaluation context");
//   - EXISTS { … } anywhere an expression is evaluated rather than lowered to a
//     SemiApply — a RETURN item, a CASE branch, a composite predicate
//     ("EXISTS { … } subquery is not supported in this evaluation context");
//   - COUNT { … } in the same positions, which has no SemiApply lowering at all.
//
// The read path has had both since task-396 / task-961, wired in
// [Engine.buildReadPhysical]; this type is what closes the gap for the path that
// writes. It adds no construct: a statement that does not write already
// evaluates all three, and this changes nothing about what any of them means.
//
// # Which graph the evaluators read
//
// The WRITING TRANSACTION's own view (lpg.Graph.WriterViewOf), which is the same
// view the statement's own scans and expands read through — as of the instant the
// transaction began, plus the versions it has written itself. That is what makes
// `MATCH (a:P) CREATE (a)-[:Z]->(a) WITH a WHERE (a)-[:Z]->(:P)` observe the edge
// the same statement just created, exactly as the clause order requires. Handing
// the evaluators a plain committed read view would have them evaluate against a
// graph the enclosing statement has already moved on from.
//
// # Concurrency
//
// NOT safe for concurrent use, for the same reason [readBuildScaffold] is not:
// both evaluators memoise into plain maps at evaluation time. One scaffold
// belongs to one statement, built and drained on one goroutine inside one
// visibility bracket. The write path never builds a morsel-parallel leaf — its
// [buildOpts] leaves parallelScanEnabled false — so no worker goroutine can reach
// either evaluator.
type writeEvalScaffold struct {
	subEval subqueryEvaluator
	patEval patternEvaluator
	// ctx is the statement's cancellation scope, threaded onto buildOpts.queryCtx
	// so a subquery drive started from an expression honours the deadline of the
	// query that started it rather than falling back to context.Background().
	ctx context.Context //nolint:containedctx // per-statement build scope, mirrors buildOpts.queryCtx
}

// init wires the scaffold for one writing statement against wv — that
// statement's writer view — and the walker and label resolver already bound to
// it. It mirrors [readBuildScaffold.init] field for field; the two bind calls
// are still the caller's to make, because they need the [buildOpts] that holds
// this scaffold's own evaluators (see [buildPlanWithMutatorFull]).
func (sc *writeEvalScaffold) init(
	ctx context.Context,
	e *Engine,
	wv *lpg.ReadView[string, float64],
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	queryReg expr.FunctionRegistry,
) {
	sc.ctx = ctx
	sc.subEval.init(walker, labelSrc, queryReg, wv)
	sc.patEval.init(wv, e.maxCollectItems)
	// Same knob, same polarity, same reason as [Engine.buildReadPhysical]: the
	// Engine field is positive, the evaluator fields are NEGATIVE so their zero
	// value keeps both adjacency-answered rewrites live. Set here so a statement
	// that writes takes the same rewrite decisions as the identical statement that
	// does not — the point of this whole type is that the two agree.
	sc.subEval.adjacencyCountsDisabled = !e.adjacencyCountRewritesEnabled
	sc.patEval.adjacencyCountsDisabled = !e.adjacencyCountRewritesEnabled
}

// bindInto points bopts at this scaffold's evaluators and hands each of them the
// statement's parameters, completing the two-step construction the evaluators
// require: [subqueryEvaluator.bind] stores the buildOpts back on the evaluator,
// so neither can be built with the other already in hand.
//
// A nil receiver is the public [BuildPlanWithMutator] path, which has no Engine
// behind it and therefore no writer view to evaluate against. It leaves bopts
// untouched, which keeps that path on the bare [expr.Eval] behaviour it has
// always had — and, critically, leaves the two interface fields genuinely nil
// rather than holding a nil pointer, which [evalRow] tests for.
func (sc *writeEvalScaffold) bindInto(bopts *buildOpts, params map[string]expr.Value) {
	if sc == nil {
		return
	}
	bopts.queryCtx = sc.ctx
	bopts.subEval = &sc.subEval
	bopts.patEval = &sc.patEval
	sc.subEval.bind(params, bopts)
	sc.patEval.bind(params, &sc.subEval)
}
