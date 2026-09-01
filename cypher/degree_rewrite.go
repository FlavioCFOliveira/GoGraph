package cypher

// degree_rewrite.go — answer a degree-answerable EXISTS { … } / COUNT { … }
// from the adjacency's own degree instead of compiling and driving an inner
// plan once per outer row (rmp #2232, part 2 of #2218).
//
// # The cost being removed
//
// [subqueryEvaluator] compiles a subquery once and then, for every outer row,
// seeds its Argument leaf, re-Inits the whole inner pipeline, drains it, and
// closes it. That is a constant per-row tax rather than a re-scan — the round-4
// audit confirmed every shape is LINEAR in outer rows — but the constant is
// large. Measured per outer row at 8 000 nodes of out-degree 4, against a bare
// label scan at 0.028 µs:
//
//	EXISTS { (a)-[:K]->() }        0.243 µs   (8.8×)
//	COUNT  { (a)-->() }    > 0     0.929 µs   (33.6×)
//	COUNT  { (a)-[:K]->() } > 0    1.605 µs   (58.1×)
//	COUNT  { (a)-[:K]->() } = 4    1.624 µs   (58.8×)
//
// Enumerating every neighbour of a node in order to compare a count against
// zero is indefensible when the adjacency can answer "how many" in O(1).
//
// # Reference design
//
// Neo4j 5.26's getDegreeRewriter.scala, read at pinned SHA
// eccd584a64d468af3daeab421478fe78567c518f. It rewrites an eligible EXISTS to
// HasDegreeGreaterThan(node, type, dir, 0), an eligible count-like expression to
// GetDegree, and a comparison of a degree against a literal to a
// short-circuiting HasDegree variant.
//
// # Eligibility is Neo4j's, deliberately and exactly
//
// The structural gate is QuerySolvableByGetDegree, and its most consequential
// clause is `Selections.empty`: the subquery's query graph must carry NO
// predicate at all. A label on a pattern node is a Selection (HasLabels) in
// Neo4j's IR, so `(a)-[:K]->(:P)` is NOT degree-rewritable — in Neo4j either.
// This matters because that is the exact shape the round-4 audit benchmarked at
// 88×: a degree counts every out-edge and cannot filter on the far node's
// label. Serving it needs a different mechanism (a filtered single walk), which
// is rmp #2235, NOT a widening of this recogniser.
//
// The second clause is isEligible: the subquery expression's free variables must
// contain the anchor and must NOT contain the relationship or the far node. A
// far-node VARIABLE is fine when it is introduced by the pattern — it is scoped
// to the subquery and unobservable outside — but a far node that names an
// ALREADY-BOUND outer variable makes the pattern an expand-into between two
// fixed endpoints, which a degree cannot answer. [degreeShape] checks both.
//
// # Direction
//
// Outgoing only. Neo4j's rewriter also anchors on the far node with
// `dir.reversed`, which GoGraph cannot do: the adjacency appends forward only
// and there is no reverse degree source (recorded as a consequence of #2218 when
// the primitive was scoped to out-degree). An incoming or undirected pattern
// therefore keeps the inner plan.
//
// # Why the recogniser must not be widened
//
// The round-3 lesson, which cost four separate defects: widening a recogniser
// silently steals from cardinality-reducing rewrites, and every instance was
// caught only by the differential suites. [TestDegreeRewrite_IneligibleShapes]
// pins each exclusion with a test that fails if the gate loosens.

import (
	"context"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// degreeRewriteCount counts how many times a subquery evaluation was answered
// from the adjacency degree rather than by driving the compiled inner plan. It
// is a diagnostic seam, read only by the in-package tests so they can assert the
// rewrite fired (or, for an ineligible shape, did not). Process-global and
// monotonic; tests snapshot it around a query rather than resetting it.
var degreeRewriteCount atomic.Uint64

// degreeShape describes a recognised degree-answerable pattern: how many
// out-edges the anchor has, optionally restricted to one relationship type.
//
// typeName is kept as the NAME rather than a resolved LabelID because a type may
// not be interned when the subquery is first recognised and may be by the time a
// later row evaluates it. [lpg.LabelRegistry] is append-only and never reassigns
// an id, so the resolution is cached on first success and retried until then.
type degreeShape struct {
	anchorVar string
	typeName  string
	typed     bool

	// resolved caches the interned LabelID for typeName once the registry has
	// it. Guarded by nothing: a subqueryEvaluator is owned by one query run and
	// is documented as not safe for concurrent use.
	resolved   lpg.LabelID
	haveResolv bool
}

// recogniseDegreePattern reports whether the pattern form of an EXISTS/COUNT
// subquery is degree-answerable for an outer row shaped like row, and returns
// the shape when it is.
//
// where is the subquery's inline WHERE; a non-nil WHERE is a Selection and
// disqualifies the pattern. It is threaded for COUNT as well as EXISTS: until
// rmp #2242 gave [ast.CountSubquery] a Where field, callers passed nil here
// because there was nothing to pass, so a COUNT carrying an inline WHERE was
// answered from the degree WITHOUT its predicate.
//
// Every rejection below mirrors a clause of Neo4j's QuerySolvableByGetDegree or
// isEligible; see the file comment for the correspondence.
//
// The STRUCTURAL half of the test lives in [ir.DegreeCountableShape], because
// the IR translator must make the identical decision one phase earlier and with
// no row in hand (rmp #2264). Only the two row-dependent clauses remain here.
// Sharing one definition is what keeps the two call sites from drifting apart.
func recogniseDegreePattern(pat *ast.Pattern, where *ast.Where, row expr.RowContext) (*degreeShape, bool) {
	shape, ok := ir.DegreeCountableShape(pat, where)
	if !ok {
		return nil, false
	}
	// ANCHOR must be BOUND: the degree is taken of the node the outer row
	// supplies. An unbound anchor has no node to take a degree of.
	if _, bound := row[shape.AnchorVar]; !bound {
		return nil, false
	}
	// FAR NODE must NOT be bound. Its variable may be introduced here — it is
	// scoped to the subquery — but naming a variable the outer row already binds
	// makes this an expand-into between two fixed endpoints rather than a degree.
	if shape.FarVar != "" {
		if _, bound := row[shape.FarVar]; bound {
			return nil, false
		}
	}

	sh := &degreeShape{anchorVar: shape.AnchorVar}
	if shape.RelType != "" {
		sh.typed = true
		sh.typeName = shape.RelType
	}
	return sh, true
}

// count returns the number of out-edges the shape describes for the anchor bound
// in row, capped at limit when limit >= 0 (an uncapped count uses limit < 0).
//
// ok is false when the anchor is not a resolvable node in this graph — a NULL
// binding, a deleted entity, or a value that is not a node — in which case the
// caller must fall back to the inner plan rather than assume zero. Guessing here
// would turn an unusual binding into a silently wrong answer.
func (s *degreeShape) count(g *lpg.ReadView[string, float64], row expr.RowContext, limit int64) (int64, bool) {
	if g == nil {
		return 0, false
	}
	v, present := row[s.anchorVar]
	if !present {
		return 0, false
	}
	// Stay in id space throughout. Going through the node-value-keyed degree API
	// would cost an id → key Resolve plus a key → id Lookup (a string hash) per
	// outer row, which measurement showed dominated this path once the inner plan
	// was gone.
	id, resolved := nodeIDFromValue(v, g.AdjList().Mapper())
	if !resolved {
		return 0, false
	}

	if !s.typed {
		// O(1) on a graph with no tombstones: one adjacency column length. Once
		// anything anywhere has been deleted the count must exclude edges landing
		// on a tombstone, which costs a walk — so the cap is handed DOWN and the
		// walk stops itself, rather than being applied to a count that has already
		// been paid for in full (rmp #2265).
		n, found := g.OutDegreeBoundedByID(id, degreeCeiling(limit))
		if !found {
			return 0, false
		}
		return int64(n), true
	}

	relID, have := s.relTypeID(g)
	if !have {
		// The type is not interned, so no edge can carry it and the degree is
		// zero. This is a real answer, not a failure to resolve: an
		// un-interned name matches nothing by construction.
		return 0, true
	}
	n, found := g.OutDegreeByTypeBoundedByID(id, relID, degreeCeiling(limit))
	if !found {
		return 0, false
	}
	return int64(n), true
}

// degreeCeiling turns a caller's cap into the limit a bounded degree walk takes:
// the cap itself when one was asked for, and [maxDegreeLimit] — effectively
// unbounded — when it was not (limit < 0).
//
// It clamps rather than converting blindly so that on a platform whose int is
// narrower than int64 a cap beyond the int range widens to "no cap" instead of
// wrapping to a small, or negative, limit — which would silently truncate the
// count.
func degreeCeiling(limit int64) int {
	if limit < 0 || limit > int64(maxDegreeLimit) {
		return maxDegreeLimit
	}
	return int(limit)
}

// maxDegreeLimit is the effectively-unbounded cap for a bounded degree walk,
// typed or untyped, used when the caller asked for the true count.
const maxDegreeLimit = int(^uint(0) >> 1)

// relTypeID resolves and caches the LabelID for the shape's relationship type.
func (s *degreeShape) relTypeID(g *lpg.ReadView[string, float64]) (lpg.LabelID, bool) {
	if s.haveResolv {
		return s.resolved, true
	}
	id, ok := g.Registry().Lookup(s.typeName)
	if !ok {
		return 0, false
	}
	s.resolved = id
	s.haveResolv = true
	return id, true
}

// degreeShapeFor returns the memoised degree shape for a subquery occurrence, or
// nil when the pattern is not degree-answerable. The recogniser runs at most
// once per occurrence: the verdict is a property of the AST and of which outer
// variables are bound at that point, both fixed for the life of the query.
//
// A nil result is also what [EngineOptions.DisableAdjacencyCountRewrites]
// produces, deliberately: "no shape" is already the caller's signal to drive the
// inner plan, so the gate needs no second code path and cannot answer differently
// from a genuine rejection. The gate is checked BEFORE the memo so the two never
// interact — a verdict cached on one engine is not consulted by another, since an
// evaluator belongs to a single query run.
func (e *subqueryEvaluator) degreeShapeFor(key ast.Expression, pat *ast.Pattern, where *ast.Where, row expr.RowContext) *degreeShape {
	if e.adjacencyCountsDisabled {
		return nil
	}
	if sh, seen := e.degree[key]; seen {
		return sh
	}
	sh, ok := recogniseDegreePattern(pat, where, row)
	if !ok {
		sh = nil
	}
	e.degree[key] = sh
	return sh
}

// EvalCountBounded implements [expr.BoundedCountEvaluator]. It is EvalCount with
// a ceiling: the returned IntegerValue is min(trueCount, limit) when limit is
// non-negative. The expression evaluator calls it when a COUNT { … } is compared
// against an integer literal, where a capped count decides the comparison
// exactly as the true count would — see [expr.BoundedCountEvaluator] for the
// proof that a cap of literal+1 is sufficient for all six operators.
//
// The cap has teeth on the degree path, in both its forms, and none of it is
// observable: a count capped at limit decides the comparison exactly as the true
// count would.
//
//   - TYPED. The walk has to resolve each slot's relationship type, and it stops
//     once limit matching edges have been counted.
//   - UNTYPED. One adjacency column length, in O(1), while the graph holds no
//     tombstones. Once anything has been deleted the count must exclude edges
//     landing on a tombstone, so it becomes a walk — and that walk stops once
//     limit LIVE edges have been counted.
//
// The untyped half of that is what rmp #2265 fixed. The cap used to be applied to
// the RESULT of an uncapped walk, so a single unrelated DELETE anywhere in the
// graph turned every untyped degree count into a full traversal of the anchor's
// adjacency — 1170× at degree 400 000 — to answer a question the cap had already
// settled. The answers were correct throughout; only the cost was wrong.
//
// When the pattern is not degree-answerable this falls back to the full inner
// drive and the exact count, where the cap is simply not applied.
func (e *subqueryEvaluator) EvalCountBounded(ctx context.Context, sub *ast.CountSubquery, row expr.RowContext, params map[string]expr.Value, limit int64) (expr.Value, error) {
	// sub.Where is threaded so both recognisers refuse a pattern carrying an
	// inline WHERE, which neither can evaluate (rmp #2242).
	if sh := e.degreeShapeFor(sub, sub.Pattern, sub.Where, row); sh != nil {
		if n, ok := sh.count(e.g, row, limit); ok {
			degreeRewriteCount.Add(1)
			return expr.IntegerValue(n), nil
		}
	}
	// Labelled single hop (#2235): a separate path for the shape the degree
	// recogniser must refuse. It honours the same cap — the walk stops as soon as
	// limit matching neighbours have been seen.
	if n, ok := e.countLabelledHop(sub, sub.Pattern, sub.Where, row, limit); ok {
		return expr.IntegerValue(n), nil
	}
	return e.EvalCount(ctx, sub, row, params)
}

// CountPatternComp implements [expr.PatternCountEvaluator]: it answers
// `size([ (a)-[:R]->(b) | … ])` from the anchor's degree when the pattern is
// degree-answerable, without building the list.
//
// The projection is deliberately ignored, and that is sound rather than a
// shortcut: it is applied once per match and cannot change how MANY matches
// there are. Neo4j's rewriter relies on the same property when it treats
// Size(ListIRExpression) as count-like. An inner WHERE is a different matter —
// it filters matches — so a comprehension carrying one is refused.
//
// # Where this is reached from
//
// Both spellings, since rmp #2264. A comprehension in a WHERE has always
// reached here. One in a RETURN or WITH item did NOT: the IR translator hoisted
// every projected comprehension into a RollUpApply and substituted a variable,
// so no [ast.PatternComprehension] survived to evaluation and this function was
// unreachable from a projection — while its documentation claimed otherwise.
// The gap was worth 24.4× on a 100 000-degree anchor (2.159 ms for the
// COUNT { } spelling against 52.737 ms for the size([ … ]) one). The translator
// now leaves exactly this shape unhoisted; see [ir.DegreeCountableShape], which
// is the same structural test used below, shared so the two cannot drift.
//
// ok is false whenever the pattern is not answerable this way; the caller then
// builds the list and takes its length exactly as before.
func (pe *patternEvaluator) CountPatternComp(_ context.Context, pc *ast.PatternComprehension, row expr.RowContext) (expr.Value, bool, error) {
	if pc == nil || pc.Predicate != nil || pc.Variable != nil {
		return nil, false, nil
	}
	// EngineOptions.DisableAdjacencyCountRewrites: decline exactly as an
	// unrecognised shape does, so the caller builds the list and takes its length.
	if pe.adjacencyCountsDisabled {
		return nil, false, nil
	}
	// recogniseDegreePattern works on an *ast.Pattern; a comprehension carries a
	// single *ast.PathPattern, so wrap it. The wrapper is stack-allocated and
	// discarded — the verdict is memoised by the caller, not this shape.
	sh, ok := recogniseDegreePattern(&ast.Pattern{Paths: []*ast.PathPattern{pc.Pattern}}, nil, row)
	if !ok {
		return nil, false, nil
	}
	n, resolved := sh.count(pe.g, row, -1)
	if !resolved {
		return nil, false, nil
	}
	degreeRewriteCount.Add(1)
	return expr.IntegerValue(n), true, nil
}
