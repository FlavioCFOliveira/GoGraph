package cypher

// read_build_scaffold.go — rmp #2693: one heap object for the per-execution
// physical-build scaffolding.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// readBuildScaffold is the set of per-execution objects [Engine.buildReadPhysical]
// needs in order to assemble a physical operator tree, held in ONE heap
// allocation instead of five.
//
// # Why this exists
//
// The physical tree is rebuilt on every execution of a query, INCLUDING a
// plan-cache hit, and that is not going to change: the tree binds this
// execution's MVCC read view, this execution's parameters and this statement's
// frozen now(), so a tree reused across executions would read at a stale instant
// whose versions the reclamation horizon has already released. What CAN change is
// how much the rebuild allocates.
//
// Five of those allocations were five separate `&T{…}` expressions with
// identical lifetimes — the node walker, the label resolver, the subquery
// evaluator, the pattern evaluator and the build options. Every one of them is
// created at the top of the build, every one is reachable from the finished tree,
// and every one dies when the tree does, so splitting them across five objects
// bought nothing and cost four allocations per read. Measured with
// -memprofilerate=1 on bench/contention's cypher-read-label-small
// ("MATCH (n:N) RETURN count(n)"): those five were 5 of the 33 allocations of the
// whole read, on a plan of exactly two operators (rmp #2693).
//
// # Why merging them is safe
//
// Merging changes lifetime for nobody. [buildOpts] already holds subEval and
// patEval, [subqueryEvaluator.bind] already stores the buildOpts back on the
// evaluator, and both evaluators already hold the same [lpg.ReadView] the walker
// and resolver hold — so the five were mutually reachable before this type
// existed and were collected as one group anyway. Their total is 528 bytes
// against 536 across the five size classes they used to occupy, so this is not a
// memory-for-CPU trade either.
//
// # Concurrency
//
// NOT safe for concurrent use, exactly as the objects it holds are not: a
// scaffold belongs to one [Engine.Run] call, which builds and drains one tree on
// one goroutine. The morsel-parallel operators copy the buildOpts per worker
// ([buildOpts.forWorker]) and DECLINE the fusion outright when the predicate
// carries a subquery or pattern predicate, precisely so no two workers share
// these evaluators (rmp #2257).
//
// It is deliberately NOT pooled. Everything in it stays reachable from the
// operator tree for as long as the caller holds the [Result], and a [Result]'s
// lifetime is the caller's — so returning a scaffold to a pool would hand live
// state to the next query on any caller that closed twice or read after Close.
// A wrong plan is worse than a slow one.
type readBuildScaffold struct {
	walker   lpgNodeWalker
	labelSrc lpgLabelResolver
	subEval  subqueryEvaluator
	patEval  patternEvaluator
	bopts    buildOpts
}

// init wires the scaffold for one execution against the read view rv, and
// returns the pointers the build already expects. The order of assignment
// matters and mirrors the order buildReadPhysical used when these were five
// separate constructors: subEval must see walker/labelSrc, bopts must see both
// evaluators, and [subqueryEvaluator.bind] / [patternEvaluator.bind] are still
// the caller's to make once the parameters are known.
func (sc *readBuildScaffold) init(
	ctx context.Context,
	e *Engine,
	rv *lpg.ReadView[string, float64],
	queryReg expr.FunctionRegistry,
) (*lpgNodeWalker, *lpgLabelResolver, *subqueryEvaluator, *patternEvaluator, *buildOpts) {
	sc.walker.g = rv
	sc.labelSrc.g = rv
	sc.labelSrc.eng = e
	sc.subEval.init(&sc.walker, &sc.labelSrc, queryReg, rv)
	sc.patEval.init(rv, e.maxCollectItems)
	sc.bopts.subEval = &sc.subEval
	sc.bopts.patEval = &sc.patEval
	sc.bopts.queryCtx = ctx
	sc.bopts.maxCollectItems = e.maxCollectItems
	return &sc.walker, &sc.labelSrc, &sc.subEval, &sc.patEval, &sc.bopts
}
