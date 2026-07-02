package cypher

// hash_join_plan.go — planner trigger for the disconnected-equi-join hash join
// (#1506, increment A of the optimizer-activation design spike, docs/
// optimizer-activation-design.md).
//
// The optimisation replaces the nested-loop Cartesian product that today serves
// a disconnected multi-pattern MATCH joined by an equality predicate:
//
//	MATCH (a:A), (b:B) WHERE a.x = b.y RETURN a, b
//
// which the IR lowers to Selection(a.x = b.y, Apply(scanA, scanB)). The nested
// loop is O(|A|·|B|); the hash join is O(|A|+|B|).
//
// The trigger is STRUCTURAL, not estimate-based: it fires only when a Selection
// directly above a plain (uncorrelated) Apply carries a conjunctive equality
// `L = R` whose two operands resolve to variables on opposite arms of the Apply.
// A true Cartesian product with no equi-join key (e.g. `MATCH (a),(b) RETURN a,
// b`) admits no hash key and keeps the nested-loop plan — a hash join cannot
// help it.
//
// Three guards must all hold (see the design spike §2.3, §4):
//
//   - ORDER SAFETY: no operator above the join may observe the changed row
//     order. Verified once per query by [hashJoinOrderSafe]; a bare LIMIT/SKIP
//     without ORDER BY, or a collect()/arrival-order aggregation anywhere in the
//     plan, disables the optimisation for the whole query.
//   - SIZE FLOOR: only worth the hash-build overhead when the build side is
//     non-trivial. The floor is structural-eligibility only here (the operator
//     itself self-selects the smaller side at runtime); the asymptotic win is
//     unconditional for an equi-join, and the constant-factor loss on tiny
//     inputs is bounded by [hashJoinSizeFloor].
//   - RESULT IDENTITY: the hash join produces exactly the multiset the
//     nested-loop product + equi-join filter would, with identical
//     null/type-coercion/NaN semantics for the join key (see exec.HashJoin).
//
// The residual predicate (every conjunct other than the chosen equi-join key)
// is re-applied as an ordinary Filter above the hash join, so the result is
// identical to Selection(fullPredicate, Apply(...)).

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// hashJoinBuildCount counts how many times the planner has substituted a hash
// join for a nested-loop Cartesian product. It is a diagnostic seam read only by
// the in-package differential test to assert the structural trigger actually
// fired (or, under a guard, did not). It is process-global and monotonic; tests
// snapshot it before/after a query rather than resetting it, so concurrent tests
// do not interfere.
var hashJoinBuildCount atomic.Uint64

// hashJoinSizeFloor is the build-side row-count below which the hash join is not
// worth its build overhead and the nested loop is kept. The asymptotic win
// (O(n+m) vs O(n·m)) is unconditional for an equi-join, but for very small
// inputs the constant factor of materialising a hash table and snapshotting
// rows loses to a tight nested loop. The design spike (§4) suggests ~64 as a
// conservative starting point, validated by the bench gate. The build side
// streams once, so the floor is enforced after the build phase has counted the
// rows: below the floor the operator behaves like the nested loop would have.
//
// NOTE: the runtime operator does not abort below the floor (that would require
// buffering the probe side too); the floor instead governs the *planner's*
// willingness to substitute. Because the structural trigger already restricts
// the substitution to genuine equi-joins where the nested loop is the
// asymptotic worst case, and the differential test proves multiset identity,
// the floor is a performance knob, not a correctness one.
const hashJoinSizeFloor = 64

// equiJoinKey describes a single `L = R` conjunct chosen as the hash-join key:
// outerKey is evaluated against the outer (probe) arm, innerKey against the
// inner (build) arm.
type equiJoinKey struct {
	outerKey ast.Expression
	innerKey ast.Expression
}

// tryBuildHashJoin attempts to build an [exec.HashJoin] for a Selection over a
// plain Apply. It returns (op, true, nil) when the structural trigger and all
// guards hold and the operator was built; (nil, false, nil) when the pattern is
// not eligible (the caller then falls back to the normal Selection build); or
// (nil, false, err) on a build error.
//
// On success the returned operator already includes any residual (non-key)
// predicate as a Filter on top, and the shared schema has been populated with
// the combined outer||inner column layout exactly as the plain-Apply build
// would have left it — so downstream operators address columns identically.
func tryBuildHashJoin(
	sel *ir.Selection,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
	procReg *procs.Registry,
	argByTag map[uint32]*exec.Argument,
	bopts *buildOpts,
) (exec.Operator, bool, error) {
	if bopts == nil || !bopts.hashJoinEnabled || !bopts.hashJoinOrderSafe {
		return nil, false, nil
	}
	// Need the parsed predicate AST and a plain Apply child.
	if sel.PredicateExpr == nil {
		return nil, false, nil
	}
	apply, ok := sel.Child.(*ir.Apply)
	if !ok {
		return nil, false, nil
	}
	// A graph is required to evaluate property keys against node rows; the
	// lpgNodeWalker carries it (the same requirement the Selection filter has).
	lw, ok := walker.(*lpgNodeWalker)
	if !ok || lw.g == nil {
		return nil, false, nil
	}

	outerVars := collectPlanVars(apply.Outer)
	innerVars := collectPlanVars(apply.Inner)
	if len(outerVars) == 0 || len(innerVars) == 0 {
		return nil, false, nil
	}

	// Decompose the predicate into top-level AND conjuncts and find an
	// equi-join conjunct that straddles the two arms.
	conjuncts := splitConjuncts(sel.PredicateExpr)
	keyIdx, key := findEquiJoinKey(conjuncts, outerVars, innerVars)
	if keyIdx < 0 {
		return nil, false, nil
	}

	// SIZE FLOOR (§4): skip the hash join when the build arm is trivially small,
	// where the build-table overhead loses to a tight nested loop. The estimate
	// is the EXACT cardinality of the build arm's leading scan (label bitmap
	// cardinality, or total node count for an all-nodes scan) — an upper bound on
	// the rows actually built, computed without touching row data. When the
	// leading scan cannot be classified the floor is treated as not-met (keep the
	// nested loop) so an unanalysable plan never regresses.
	buildRows, ok := estimateLeadingScanRows(apply.Inner, labelSrc)
	if !ok || buildRows < hashJoinSizeFloor {
		return nil, false, nil
	}

	g := lw.g

	// Build the outer (probe) arm into the shared schema, mirroring the
	// plain-Apply build so the combined layout is preserved.
	outerOp, err := buildOperator(apply.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
	if err != nil {
		return nil, false, err
	}
	outerWidth := schemaWidth(schema)
	// Snapshot the probe-side schema (outer columns only) for the probe key fn.
	probeSchema := copySchema(schema)

	// Build the inner (build) arm with a fresh schema, then merge with the
	// outer offset — identical bookkeeping to the *ir.Apply case in
	// buildOperator (including the bopts metadata column shifts).
	innerSchema := map[string]int{}
	var preEdgeKeys, prePathChainKeys, prePathMetaKeys, preVLEKeys map[string]struct{}
	var preTripletLen int
	if bopts != nil {
		preEdgeKeys = setSnap(bopts.edgeVarMeta)
		prePathChainKeys = setSnap(bopts.pathVarChain)
		prePathMetaKeys = setSnap(bopts.pathVarMeta)
		preVLEKeys = setSnap(bopts.vleRelMeta)
		preTripletLen = len(bopts.expandTripletSeq)
	}
	// Note: a plain (uncorrelated) Apply's inner ir.Argument leaf is never seeded
	// with outer data — it emits a single empty row per Init (Cartesian). The
	// inner build allocates its own fresh exec.Argument for that leaf, so the
	// build arm drains fully and independently. No correlation wiring is needed.
	innerOp, err := buildOperator(apply.Inner, walker, labelSrc, reg, params, innerSchema, idxMgr, procReg, argByTag, bopts)
	if err != nil {
		return nil, false, err
	}
	for k, v := range innerSchema {
		schema[k] = v + outerWidth
	}
	shiftApplyMetaColumns(bopts, outerWidth, preEdgeKeys, prePathChainKeys, prePathMetaKeys, preVLEKeys, preTripletLen)

	// The build-side (inner) key function evaluates innerKey against an
	// inner-only row using the fresh inner schema (0-based, exactly the row
	// shape the build operator emits before the join combines arms). The
	// probe-side (outer) key function uses the outer schema.
	buildKeySchema := copySchema(innerSchema)
	probeKeySchema := probeSchema
	innerKeyExpr := key.innerKey
	outerKeyExpr := key.outerKey

	buildFn := func(row exec.Row) (expr.Value, error) {
		rc := buildRowCtx(row, buildKeySchema, g, bopts)
		return evalRow(bopts, innerKeyExpr, rc, params, reg)
	}
	probeFn := func(row exec.Row) (expr.Value, error) {
		rc := buildRowCtx(row, probeKeySchema, g, bopts)
		return evalRow(bopts, outerKeyExpr, rc, params, reg)
	}

	// The Apply emits outer||inner. Here outer is the probe, inner is the build.
	// Keep that exact column order: probe||build, i.e. buildOnLeft=false.
	hjMB, hjEst := resultByteBudget(bopts)
	var op exec.Operator = exec.NewHashJoin(innerOp, outerOp, buildFn, probeFn, false).
		WithByteBudget(hjMB, hjEst)

	// Re-apply every residual conjunct (all but the chosen key) as a Filter on
	// the combined row, preserving Selection(fullPredicate, …) semantics.
	residual := make([]ast.Expression, 0, len(conjuncts)-1)
	for i, c := range conjuncts {
		if i == keyIdx {
			continue
		}
		residual = append(residual, c)
	}
	if len(residual) > 0 {
		combinedSchema := copySchema(schema)
		op = buildResidualFilter(op, residual, combinedSchema, g, params, reg, bopts)
	}
	hashJoinBuildCount.Add(1)
	return op, true, nil
}

// buildResidualFilter wraps op with a Filter applying the conjunction of the
// residual predicates against the combined (outer||inner) row.
func buildResidualFilter(
	child exec.Operator,
	residual []ast.Expression,
	schema map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) exec.Operator {
	exprs := residual
	return exec.NewFilter(child, func(row exec.Row) (expr.Value, error) {
		rc := buildRowCtx(row, schema, g, bopts)
		for _, e := range exprs {
			v, err := evalRow(bopts, e, rc, params, reg)
			if err != nil {
				return nil, err
			}
			// Three-valued AND: any non-true conjunct drops the row. Mirrors a
			// chain of Selection operators (each Filter keeps only truthy rows).
			if !expr.IsTruthy(v) {
				return expr.BoolValue(false), nil
			}
		}
		return expr.BoolValue(true), nil
	})
}

// splitConjuncts flattens a top-level AND tree into its conjuncts. A non-AND
// expression yields a single-element slice. Only the boolean AND operator is
// split; OR and other operators are opaque (returned whole).
func splitConjuncts(e ast.Expression) []ast.Expression {
	bin, ok := e.(*ast.BinaryOp)
	if !ok || bin.Operator != "AND" {
		return []ast.Expression{e}
	}
	out := splitConjuncts(bin.Left)
	out = append(out, splitConjuncts(bin.Right)...)
	return out
}

// findEquiJoinKey scans conjuncts for the first `L = R` whose two operands
// reference variables on opposite arms of the join (one side only outer, the
// other side only inner). It returns the conjunct index and the oriented key
// (outerKey evaluated on the outer arm, innerKey on the inner arm), or
// (-1, equiJoinKey{}) when no such conjunct exists.
func findEquiJoinKey(conjuncts []ast.Expression, outerVars, innerVars map[string]struct{}) (int, equiJoinKey) {
	allVars := make(map[string]struct{}, len(outerVars)+len(innerVars))
	for v := range outerVars {
		allVars[v] = struct{}{}
	}
	for v := range innerVars {
		allVars[v] = struct{}{}
	}
	for i, c := range conjuncts {
		bin, ok := c.(*ast.BinaryOp)
		if !ok || bin.Operator != "=" {
			continue
		}
		lOuter, lInner := classifySide(bin.Left, outerVars, innerVars, allVars)
		rOuter, rInner := classifySide(bin.Right, outerVars, innerVars, allVars)
		// Require each operand to reference exactly one arm, and the two
		// operands to reference different arms. An operand that references both
		// arms (or neither, or an unknown variable) disqualifies this conjunct.
		if lOuter && !lInner && rInner && !rOuter {
			return i, equiJoinKey{outerKey: bin.Left, innerKey: bin.Right}
		}
		if lInner && !lOuter && rOuter && !rInner {
			return i, equiJoinKey{outerKey: bin.Right, innerKey: bin.Left}
		}
	}
	return -1, equiJoinKey{}
}

// classifySide reports whether expression e references any outer variable and
// whether it references any inner variable. An expression that touches a
// variable in neither set (e.g. a literal, a parameter, or an unknown name)
// reports (false, false). It reuses [referencedVars] (the shared AST walker
// already used for the Cartesian-product connectedness check), passing the
// union of both arms' variables as the known set so the walk covers property
// access, function calls, arithmetic, CASE, comprehensions and subscripts.
func classifySide(e ast.Expression, outerVars, innerVars, allVars map[string]struct{}) (touchesOuter, touchesInner bool) {
	for _, v := range referencedVars(e, allVars) {
		if _, ok := outerVars[v]; ok {
			touchesOuter = true
		}
		if _, ok := innerVars[v]; ok {
			touchesInner = true
		}
	}
	return touchesOuter, touchesInner
}

// collectPlanVars returns the deduplicated union of variable names introduced
// by plan and its entire subtree. Several IR operators (Expand, VarLengthExpand)
// report only the variables they themselves introduce in Vars(), so a
// non-recursive scan would miss the leading scan's node variable — hence the
// full descent.
func collectPlanVars(plan ir.LogicalPlan) map[string]struct{} {
	out := make(map[string]struct{})
	var walk func(ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil {
			return
		}
		for _, v := range p.Vars() {
			if v != "" {
				out[v] = struct{}{}
			}
		}
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return out
}

// estimateLeadingScanRows returns the exact cardinality of the leading scan of
// an arm, descending the leftmost child chain to the leaf scan. It returns the
// label-bitmap cardinality for a NodeByLabelScan, the total node count for an
// AllNodesScan, and (0, false) for any other leaf shape (index seek, expand-only
// subtree, …). The leading scan is an UPPER bound on the rows the arm builds —
// subsequent Expands and Selections only reduce the count — so it is a safe and
// cheap floor input. labelSrc is the live label resolver; a nil bitmap (label
// never interned) yields a zero count.
func estimateLeadingScanRows(arm ir.LogicalPlan, labelSrc labelResolverIface) (int, bool) {
	p := arm
	for p != nil {
		switch n := p.(type) {
		case *ir.NodeByLabelScan:
			if labelSrc == nil {
				return 0, false
			}
			bm := labelSrc.ResolveLabelBitmap(n.Label)
			if bm == nil {
				return 0, true
			}
			return int(bm.GetCardinality()), true
		case *ir.AllNodesScan:
			// An all-nodes scan's count is not available from the label
			// resolver; treat as eligible (a bare disconnected MATCH (a),(b)
			// would not reach here because it has no equi-join key, but a labelled
			// arm joined to an unlabelled one can). Be conservative: require a
			// classifiable count, so an all-nodes build arm does not trigger.
			return 0, false
		default:
			children := p.Children()
			if len(children) == 0 {
				return 0, false
			}
			p = children[0]
		}
	}
	return 0, false
}

// shiftApplyMetaColumns shifts the inner-relative column positions recorded in
// bopts metadata maps by outerWidth, for entries added during the inner build.
// It is the exact bookkeeping the *ir.Apply case performs after merging the
// inner schema, factored out so the hash-join path stays byte-identical to it.
func shiftApplyMetaColumns(
	bopts *buildOpts,
	outerWidth int,
	preEdgeKeys, prePathChainKeys, prePathMetaKeys, preVLEKeys map[string]struct{},
	preTripletLen int,
) {
	if bopts == nil {
		return
	}
	for name, info := range bopts.edgeVarMeta {
		if _, was := preEdgeKeys[name]; was {
			continue
		}
		info.srcCol += outerWidth
		info.edgeCol += outerWidth
		info.dstCol += outerWidth
		bopts.edgeVarMeta[name] = info
	}
	for name, info := range bopts.pathVarChain {
		if _, was := prePathChainKeys[name]; was {
			continue
		}
		info.leadingCol += outerWidth
		for i := range info.steps {
			info.steps[i].srcCol += outerWidth
			info.steps[i].edgeCol += outerWidth
			info.steps[i].dstCol += outerWidth
		}
		bopts.pathVarChain[name] = info
	}
	for name, info := range bopts.pathVarMeta {
		if _, was := prePathMetaKeys[name]; was {
			continue
		}
		info.listCol += outerWidth
		bopts.pathVarMeta[name] = info
	}
	for name, info := range bopts.vleRelMeta {
		if _, was := preVLEKeys[name]; was {
			continue
		}
		info.listCol += outerWidth
		bopts.vleRelMeta[name] = info
	}
	for i := preTripletLen; i < len(bopts.expandTripletSeq); i++ {
		bopts.expandTripletSeq[i].srcCol += outerWidth
		bopts.expandTripletSeq[i].edgeCol += outerWidth
		bopts.expandTripletSeq[i].dstCol += outerWidth
	}
}

// hashJoinOrderSafe reports whether the whole-query IR plan contains no operator
// that would observe the row order a hash join changes. It returns false (the
// optimisation is disabled for the query) when the plan contains:
//
//   - a Limit or Skip operator NOT dominated by an order-establishing Sort/Top
//     above it (a bare LIMIT/SKIP without ORDER BY — the specific rows returned
//     are then observable, openCypher 9 §8.4), or
//   - an EagerAggregation whose aggregation captures arrival order
//     (collect / collect(DISTINCT) — the list value is order-dependent).
//
// This is deliberately conservative: any uncertainty disables the optimisation.
// It is computed once per query and threaded into [buildOpts.hashJoinOrderSafe].
func hashJoinOrderSafe(plan ir.LogicalPlan) bool {
	safe := true
	// sortedAbove tracks whether an order-establishing operator (Sort/Top) sits
	// above the current node on the path from the root. A LIMIT/SKIP under such
	// an operator observes a defined order, so the hash join's reordering below
	// it is masked by the sort.
	var walk func(p ir.LogicalPlan, sortedAbove bool)
	walk = func(p ir.LogicalPlan, sortedAbove bool) {
		if p == nil || !safe {
			return
		}
		switch p.(type) {
		case *ir.Sort, *ir.Top:
			sortedAbove = true
		case *ir.Limit, *ir.Skip:
			if !sortedAbove {
				safe = false
				return
			}
		case *ir.EagerAggregation:
			if aggregationObservesOrder(p) {
				safe = false
				return
			}
		}
		for _, c := range p.Children() {
			walk(c, sortedAbove)
		}
	}
	walk(plan, false)
	return safe
}

// aggregationObservesOrder reports whether an EagerAggregation contains an
// aggregate whose result depends on the arrival order of its input rows — i.e.
// a hash join's row reordering below it would change the observable result.
//
// The cypher-expert taxonomy (openCypher aggregation semantics): collect (and
// collect(DISTINCT), whose list reflects first-occurrence order) materialises
// rows in arrival order, so the list value is order-dependent. count / sum /
// avg / min / max / stDev are commutative/associative or use orderability, not
// arrival order, and are therefore order-safe. Any unrecognised aggregate is
// treated conservatively as order-observing.
func aggregationObservesOrder(p ir.LogicalPlan) bool {
	agg, ok := p.(*ir.EagerAggregation)
	if !ok {
		return false
	}
	for _, a := range agg.Aggregates {
		switch normaliseAggName(a.Function) {
		case "count", "sum", "avg", "min", "max", "stdev", "stdevp":
			// Order-insensitive.
		default:
			// collect, percentileCont/Disc-with-observable-list, and any
			// unknown aggregate: assume order-observing.
			return true
		}
	}
	return false
}

// normaliseAggName lowercases and trims an aggregate function name for
// case-insensitive comparison.
func normaliseAggName(name string) string {
	b := make([]byte, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
