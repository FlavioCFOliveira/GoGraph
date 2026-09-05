package cypher

// count_var_rewrite.go — rmp #2657: rewrite count(<pattern-bound variable>) to
// count(*) when the variable provably cannot be null.
//
// # What the rewrite is for
//
// count(v) counts non-null BINDINGS of v; count(*) counts ROWS. They are the same
// integer whenever v is bound non-null in every row, and only then. The engine's
// cheapest count paths are all keyed on the count(*) spelling:
//
//   - [exec.CountRows] (#2625) removes the pre-projection entirely, but declines any
//     non-empty argument on purpose, because in general the argument has to be
//     evaluated per row;
//   - [exec.countStarKernel] reads nothing but the group's row count, where
//     [exec.countKernel] copies the argument column and consults its validity bitmap.
//
// So the common spelling `RETURN count(n)` forfeited work the engine already knew how
// to skip, purely on the surface form of the query.
//
// # What was already there — a correction to the task's premise
//
// #2657 was raised on the premise that count(v) "cannot reach CountRows,
// LabelCountScan, AllNodesCountScan or ParallelCountScan — all of which require
// count(*)". THREE OF THOSE FOUR ALREADY ACCEPTED IT. [tryBuildParallelCountScan],
// [tryBuildAllNodesCountScan] and [tryBuildLabelCountScan] each admit an argument
// that is empty OR equal to the child scan's own NodeVar, because a bare scan binds a
// non-null node in every row. What they require is a BARE scan child, and that is the
// real boundary: the moment a Selection or an Expand sits between the aggregate and
// the leaf, none of them apply, [tryBuildCountRows] refuses the argument, and the
// query falls to the full pre-projection + EagerAggregation pipeline.
//
// This rewrite therefore does NOT unlock the leaf pushdowns — they were never shut.
// It unlocks the GENERAL path, [exec.CountRows], and the columnar countStarKernel,
// for the shapes the leaf pushdowns cannot serve:
//
//	MATCH (a)-[:R]->(b) RETURN count(b)          -- Expand child
//	MATCH (n:L) WHERE n.age > 5 RETURN count(n)   -- Selection child
//
// # Prior art
//
// Neo4j performs the same normalisation. In neo4j at tag 2026.07.1 (commit f213380),
// community/cypher/cypher-planner/src/main/scala/org/neo4j/cypher/internal/compiler/
// planner/logical/steps/countStorePlanner.scala, `checkForValidAggregations` (line
// 172) routes three spellings to the SAME solver, trySolveNodeOrRelationshipAggregation:
// `COUNT(<id>)` (line 185), `COUNT(*)` (line 199) and `COUNT(n.prop)` (line 213). The
// DISTINCT qualifier is excluded structurally — the pattern is
// `FunctionInvocation(_, false, ...)`, whose second position is the distinct flag.
//
// Its null-safety guard is two conditions, and both were read in the source rather
// than taken from the task text:
//
//   - `optionalMatches` must be empty. In the QueryGraph destructuring at line 93 the
//     sixth position is matched with `SetExtractor()`, the empty-set extractor, and
//     the sixth field of `case class QueryGraph` is `optionalMatches`
//     (community/cypher/ir/src/main/scala/org/neo4j/cypher/internal/ir/QueryGraph.scala
//     line 69). The field positions were verified, not assumed.
//   - `patternHasNoDependencies` (lines 81-84): every pattern node and pattern
//     relationship variable must be disjoint from `argumentIds`, i.e. no pattern
//     variable may arrive from an enclosing correlated scope. `apply` additionally
//     requires `query.queryInput.isEmpty` (line 58).
//
// Only the structural idea is taken. Neo4j is GPLv3 and no code crosses over; the
// walk below is an operator-tree traversal of this project's own IR, which has no
// QueryGraph to destructure.
//
// # Where this diverges from Neo4j, deliberately
//
// Neo4j's is a PUSHDOWN gate, so it also requires no grouping keys and no pagination
// — restrictions of the count store, not of the rewrite. This is a REWRITE, so it
// applies to grouped forms too (`RETURN n.grp, count(n)`), where the win is
// countStarKernel over countKernel. Nothing about the null-safety argument depends on
// the grouping keys.
//
// # The guard is the whole task
//
// The walk is an ALLOWLIST with a default REJECT. Every IR operator that is not
// explicitly admitted declines the rewrite, so a plan node added later cannot silently
// widen the rewrite's reach — it narrows it, which is the safe direction.
// [TestCountVarRewrite_AllowlistIsClosed] pins that property against the live set of
// ir plan types.

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// countVarToCountStarCount counts how many aggregate expressions the rewrite has
// converted from count(<variable>) to count(*). It is a process-global, monotonic
// diagnostic seam read only by the in-package tests, mirroring
// [countRowsBuildCount] and [parallelAggregateScanBuildCount].
//
// BEING PROCESS-GLOBAL, it may only ever be asserted to have MOVED. A test that
// asserts a delta of ZERO around one query reads every concurrently running test's
// increments too, and the sibling tests in this package run t.Parallel(): such an
// assertion fails at random. The negative direction — "this shape was a candidate and
// the guard refused it" — is established instead by calling
// [rewriteCountVarToCountStar] directly on the translated IR, which is local to the
// caller and cannot be polluted.
var countVarToCountStarCount atomic.Uint64

// rewriteCountVarToCountStar returns p with every count(<bare pattern-bound
// variable>) aggregate whose variable provably cannot be null rewritten to count(*)
// — Argument cleared and ArgumentExpr dropped — and returns p ITSELF, unmodified,
// when there is nothing to rewrite.
//
// OutputName is never touched, so `RETURN count(n)` keeps its `count(n)` column name
// and every result stays byte-identical; only the access path changes.
//
// p and p.Child are never mutated. A rewritten aggregation is a fresh
// [ir.EagerAggregation] over a fresh Aggregates slice, because the logical plan it
// comes from is shared: it lives in the plan cache and is read concurrently by every
// execution of the same query text.
func rewriteCountVarToCountStar(p *ir.EagerAggregation) *ir.EagerAggregation {
	if p == nil || len(p.Aggregates) == 0 || p.Child == nil {
		return p
	}
	// Cheap pre-pass so the subtree walk is paid for only by an aggregation that
	// actually spells count(<bare variable>).
	first := -1
	for i := range p.Aggregates {
		if countVarCandidate(&p.Aggregates[i]) != "" {
			first = i
			break
		}
	}
	if first < 0 {
		return p
	}

	// The verdict is per VARIABLE, and a shape like `RETURN count(a), count(b)` asks
	// about more than one, so memoise it rather than re-walking the subtree.
	//
	// This map is FREE, and that was measured rather than assumed: it does not escape
	// and its size hint is a constant, so the compiler stack-allocates it. Replacing it
	// with a one-entry memo was tried and moved allocs/op by exactly nothing
	// (27.00 both arms, p=1.000, n=6 on BenchmarkCountAllNodes), so the map stays.
	// The 2 allocs/op this rewrite does add when it FIRES are the two below — the
	// EagerAggregation copy and the Aggregates slice — and they are the price of not
	// mutating a logical plan that the plan cache shares across concurrent executions.
	verdict := make(map[string]bool, 2)
	var out *ir.EagerAggregation
	for i := first; i < len(p.Aggregates); i++ {
		v := countVarCandidate(&p.Aggregates[i])
		if v == "" {
			continue
		}
		safe, seen := verdict[v]
		if !seen {
			safe = countVarIsNonNullPatternBound(p.Child, v)
			verdict[v] = safe
		}
		if !safe {
			continue
		}
		if out == nil {
			cp := *p
			cp.Aggregates = make([]ir.AggregateExpr, len(p.Aggregates))
			copy(cp.Aggregates, p.Aggregates)
			out = &cp
		}
		out.Aggregates[i].Argument = ""
		out.Aggregates[i].ArgumentExpr = nil
		countVarToCountStarCount.Add(1)
	}
	if out == nil {
		return p
	}
	return out
}

// countVarCandidate returns the variable name when agg is a non-DISTINCT count over
// a BARE variable reference, and "" otherwise.
//
// The argument must be an [ast.Variable] whose name equals the textual Argument: a
// property access (count(n.prop)), a function call, or any computed expression
// carries its own null semantics and is not a candidate. count(DISTINCT n) is never a
// candidate — it deduplicates on the argument's VALUE, so erasing the argument would
// change the answer, and this is the same structural exclusion Neo4j makes.
//
// A nil ArgumentExpr with a non-empty Argument (an IR built directly from strings,
// resolved downstream by schema lookup) is refused rather than trusted.
func countVarCandidate(agg *ir.AggregateExpr) string {
	if agg.Distinct || agg.Argument == "" {
		return ""
	}
	if !strings.EqualFold(agg.Function, "count") {
		return ""
	}
	v, isVar := agg.ArgumentExpr.(*ast.Variable)
	if !isVar || v.Name != agg.Argument {
		return ""
	}
	return v.Name
}

// countVarIsNonNullPatternBound reports whether v is bound by a pattern operator
// inside child and is non-null in EVERY row child emits.
//
// It requires both halves, and the second is the one that carries the risk:
//
//  1. some operator in the subtree binds v as a pattern variable that cannot be null
//     (a node scan's NodeVar, or an [ir.Expand]'s relationship or destination); and
//  2. every operator in the subtree is one that neither introduces a null binding
//     for v nor rebinds v to something else.
//
// (2) is enforced by an allowlist with a default reject, so the answer to "can this
// shape produce a null v?" is "unless it is one of a handful of operators whose
// behaviour was checked, assume yes".
func countVarIsNonNullPatternBound(child ir.LogicalPlan, v string) bool {
	if child == nil || v == "" {
		return false
	}
	bound := false
	if !countVarWalk(child, v, &bound) {
		return false
	}
	return bound
}

// countVarWalk returns false as soon as any node in n's subtree could make v null,
// rebind v, or is simply not on the allowlist. It sets *bound when it passes a
// non-nullable pattern binding of v.
//
// The three explicit rejections named by rmp #2657 — [ir.OptionalExpand],
// [ir.OptionalApply] and [ir.Argument] — are listed first and by name. They would all
// fall into the default reject anyway; they are spelled out because they are the
// three the semantics actually turn on, and because a test asserts each one refuses:
//
//   - [ir.OptionalExpand] binds RelVar and ToVar to NULL when no relationship is
//     found (that is its whole purpose), so count(ToVar) < count(*) is the normal
//     case, not an edge case.
//   - [ir.OptionalApply] keeps the left row with the right side's variables null when
//     the right side yields nothing.
//   - [ir.Argument] injects variables from an enclosing correlated scope, so nothing
//     in this subtree establishes how — or whether — they were bound. This is the
//     counterpart of Neo4j's `patternHasNoDependencies`.
//
// Rejecting the whole subtree when an OptionalExpand appears ANYWHERE is stricter
// than strictly necessary — in `MATCH (a:A) OPTIONAL MATCH (a)-[:R]->(b) RETURN
// count(a)`, a is still non-null — and that conservatism is deliberate: it is
// precisely the "optionalMatches must be empty" shape of Neo4j's own guard, and it
// costs a rewrite on a shape whose cost is dominated by the optional expansion.
func countVarWalk(n ir.LogicalPlan, v string, bound *bool) bool {
	switch t := n.(type) {
	// ── The three explicit rejections (#2657). ───────────────────────────────
	case *ir.OptionalExpand:
		return false
	case *ir.OptionalApply:
		return false
	case *ir.Argument:
		return false

	// ── Non-nullable pattern binders. ────────────────────────────────────────
	// Each emits exactly one row per matched entity with NodeVar bound to a real
	// node; there is no row shape in which NodeVar is null.
	case *ir.AllNodesScan:
		if t.NodeVar == v {
			*bound = true
		}
	case *ir.NodeByLabelScan:
		if t.NodeVar == v {
			*bound = true
		}
	case *ir.NodeByIndexSeek:
		if t.NodeVar == v {
			*bound = true
		}
	case *ir.NodeByIndexRangeScan:
		if t.NodeVar == v {
			*bound = true
		}
	// Expand is the non-optional expansion: a row exists only when a relationship
	// was traversed, so both the relationship and the destination are bound. When
	// IntoVar is set, ToVar carries the synthetic __anon_N_to_<IntoVar> name and an
	// equality Selection sits above; the synthetic can never be a user variable, and
	// IntoVar itself was bound lower down by whichever operator introduced it.
	case *ir.Expand:
		if t.RelVar == v || t.ToVar == v {
			*bound = true
		}

	// ── Transparent, but never a binder of v. ────────────────────────────────
	// Selection filters rows and binds nothing.
	case *ir.Selection:
		// nothing to bind

	// VarLengthExpand is admitted as a PASS-THROUGH only: a subtree containing one
	// may still have v bound non-null by a scan below it. It is not admitted as a
	// binder of v, so a shape whose only binding of v is a variable-length hop
	// declines — the minimum-depth-zero form, the collected relationship list, and
	// the named-path variable each have their own value semantics and none of them
	// was measured to be worth the risk here.
	case *ir.VarLengthExpand:
		if t.RelVar == v || t.ToVar == v || t.PathVar == v {
			return false
		}

	// ── Everything else. ────────────────────────────────────────────────────
	// Default reject. Notably this refuses ir.Projection (a WITH item can bind v to
	// any expression, including a null one), ir.Unwind (UNWIND [1,null] AS v binds
	// null), the Apply family, Union, Limit/Skip, Distinct, Eager, Sort/Top, the
	// subquery operators, and every write operator. A plan type added to the ir
	// package in future lands here and declines.
	default:
		return false
	}

	for _, c := range n.Children() {
		if !countVarWalk(c, v, bound) {
			return false
		}
	}
	return true
}

// ─────────────────────────────────────────────────────────────────────────────
// Cross-execution memo (rmp #2693)
// ─────────────────────────────────────────────────────────────────────────────

// countVarRewriteMemo memoises [rewriteCountVarToCountStar] per
// [ir.EagerAggregation] node for the lifetime of one [planCacheEntry].
//
// # Why it exists
//
// The rewrite is a pure function of an immutable plan node, and when it FIRES it
// allocates twice — the EagerAggregation copy and its Aggregates slice, which
// exist precisely so the cached logical plan is not mutated (see the comment
// inside [rewriteCountVarToCountStar]). Those two allocations were paid on EVERY
// execution of a plan whose answer can never change. Measured with
// -memprofilerate=1 on bench/contention's cypher-read-label-small
// ("MATCH (n:N) RETURN count(n)" — the count(var) spelling, so the rewrite
// fires): 2 of the 33 allocations of the whole read (rmp #2693).
//
// It is the same class as [nodeScalarUseMemo] and deliberately the same shape,
// including the ceiling. The concurrency argument is that type's, unchanged: a
// [planCacheEntry] is shared by every concurrent execution of one query text, so
// this is read-mostly and reached from many goroutines; stores go through
// LoadOrStore so every caller receives the SAME rewritten node; and a stored
// value is immutable, because the rewrite builds a fresh node and no consumer
// writes to one.
//
// # Bound
//
// The keys are [ir.EagerAggregation] pointers read off the cached plan, and this
// package synthesises none — every EagerAggregation is built by the translator in
// cypher/ir, once per cached plan — so the table is bounded by the plan's own
// aggregation count. The ceiling is kept anyway, for the reason
// [nodeScalarUseMemo] documents: a future build path that synthesised a node per
// execution would otherwise turn this into an unbounded cache, and past the
// ceiling the memo simply answers from the live rewrite, which is exactly the
// behaviour that preceded it.
type countVarRewriteMemo struct {
	m sync.Map // *ir.EagerAggregation -> *ir.EagerAggregation (possibly the same pointer)

	// entries counts what has been stored, so the ceiling is enforceable without a
	// Len the sync.Map does not have. Increment-only; a concurrent burst of
	// first-time stores may overshoot slightly, which is harmless because the bound
	// is on unbounded GROWTH, not an exact quota.
	entries atomic.Int64

	// hits and misses exist so a test can prove the memo is CONSULTED rather than
	// merely present. A memo that is populated and never read is invisible to a
	// result-identical differential test, which is the only other check this
	// rewrite has.
	hits   atomic.Int64
	misses atomic.Int64
}

// countVarRewriteMemoMaxEntries is the ceiling on entries in one
// [countVarRewriteMemo]. It matches [scalarUseMemoMaxEntries] because it bounds
// the same thing for the same reason on the same shared entry.
const countVarRewriteMemoMaxEntries = 256

// get returns the memoised rewrite of p, computing and storing it on a miss.
//
// The value stored may be p ITSELF — that is the common case, since most
// aggregations spell no count(<bare variable>) at all — and storing it is the
// point: a hit that answers "unchanged" is what removes the subtree walk as well
// as the two allocations.
func (m *countVarRewriteMemo) get(p *ir.EagerAggregation) *ir.EagerAggregation {
	if p == nil {
		return nil
	}
	if v, ok := m.m.Load(p); ok {
		m.hits.Add(1)
		//nolint:forcetypeassert // the memo map is unexported and only ever stores *ir.EagerAggregation
		return v.(*ir.EagerAggregation)
	}
	m.misses.Add(1)
	out := rewriteCountVarToCountStar(p)
	if m.entries.Load() >= countVarRewriteMemoMaxEntries {
		return out
	}
	actual, loaded := m.m.LoadOrStore(p, out)
	if !loaded {
		m.entries.Add(1)
	}
	//nolint:forcetypeassert // LoadOrStore returns the value already in the memo map, which only stores *ir.EagerAggregation
	return actual.(*ir.EagerAggregation)
}

// rewriteCountVarToCountStarFor routes the rewrite of p through the memo on
// bopts when there is one, and calls [rewriteCountVarToCountStar] directly when
// there is not.
//
// The fallback is not dead code, for the reason [analyseNodeScalarUseFor]'s is
// not: a [buildOpts] is also built on paths with no plan-cache entry to memoise
// against — the plan-rendering paths, the write path's own builder, and the
// scoped child [buildOpts.forSubquery] derives — and those must keep working,
// just without the memo.
func rewriteCountVarToCountStarFor(bopts *buildOpts, p *ir.EagerAggregation) *ir.EagerAggregation {
	if bopts == nil || bopts.countVarMemo == nil {
		return rewriteCountVarToCountStar(p)
	}
	return bopts.countVarMemo.get(p)
}
