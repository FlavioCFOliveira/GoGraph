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
// Two guards must hold (see the design spike §2.3, §4):
//
//   - SIZE FLOOR: only worth the hash-build overhead when the build side is
//     non-trivial. The floor is structural-eligibility only here (the operator
//     itself self-selects the smaller side at runtime); the asymptotic win is
//     unconditional for an equi-join, and the constant-factor loss on tiny
//     inputs is bounded by [hashJoinSizeFloor].
//   - RESULT IDENTITY: the hash join produces exactly the multiset the
//     nested-loop product + equi-join filter would, with identical
//     null/type-coercion/NaN semantics for the join key (see exec.HashJoin).
//
// There was a third — ORDER SAFETY, a whole-query IR scan that disabled the
// substitution for the entire query whenever a bare LIMIT/SKIP without ORDER BY
// or an arrival-order aggregation appeared anywhere in the plan. It is gone (rmp
// #2234). The substitution does not merely preserve the multiset, it emits the
// nested loop's row SEQUENCE position for position, so there was nothing for the
// scan to protect: it was rejecting queries over a reordering that cannot happen,
// and those queries silently got the O(n·m) nested loop. The argument is at
// [hashJoinBuildOnLeft] and its empirical half is
// TestHashJoinOrder_SequenceMatchesNestedLoop, which compares full row sequences
// for exactly the shapes the scan used to exclude.
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

// hashJoinColumnarBuildCount counts the subset of those substitutions that chose
// the COLUMNAR operator ([exec.ColumnarHashJoin]) over the row-mode one. Like
// [hashJoinBuildCount] it is a diagnostic seam, incremented once per plan build
// (never per row), read only by the in-package differential tests so they can
// prove a case exercises the operator they mean to exercise — the order-preservation
// proof (#2225 part B) has to hold for BOTH operators, and without this the
// row-mode path could silently stop being covered.
var hashJoinColumnarBuildCount atomic.Uint64

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
	if bopts == nil || !bopts.hashJoinEnabled {
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
	// Freeze both key layouts at plan-build time (#2645): each key fn below runs
	// once per row, and a rowSchema is written once here and only ever read
	// afterwards, so both are safe to share across the join's parallel workers.
	buildKeySchema := newRowSchema(copySchema(innerSchema))
	probeKeySchema := newRowSchema(probeSchema)
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
	//
	// THIS ASSIGNMENT IS THE ORDER-PRESERVATION GUARANTEE (#2225 part B) — see
	// [hashJoinBuildOnLeft]. It is not a performance heuristic and must not become
	// one: pinning build to the INNER arm, never self-selecting the smaller side at
	// runtime, is what makes the emitted sequence row-for-row identical to the
	// nested loop and lets the write path admit the substitution at all.
	//
	// When both arms build to a ChunkProducer (a bare scan on each side — the
	// common disconnected-equi-join shape), prefer the columnar hash join
	// (exec.ColumnarHashJoin, #2105): it drains both children column-major and
	// retains build-side row-ids into a column-major buffer instead of a
	// per-row make(Row)+copy snapshot, a result-identical allocation win. Any
	// non-ChunkProducer arm (an Expand/Filter subtree) keeps the row-mode
	// exec.HashJoin, so existing plans are unchanged (design §6.2).
	hjMB, hjEst := resultByteBudget(bopts)
	var op exec.Operator
	if chj, colOK := exec.NewColumnarHashJoin(innerOp, outerOp, buildFn, probeFn, hashJoinBuildOnLeft); colOK {
		op = chj.WithByteBudget(hjMB, hjEst)
		hashJoinColumnarBuildCount.Add(1)
	} else {
		op = exec.NewHashJoin(innerOp, outerOp, buildFn, probeFn, hashJoinBuildOnLeft).
			WithByteBudget(hjMB, hjEst)
	}

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
	g *lpg.ReadView[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) exec.Operator {
	exprs := residual
	rs := newRowSchema(schema)
	return exec.NewFilter(child, func(row exec.Row) (expr.Value, error) {
		rc := buildRowCtx(row, rs, g, bopts)
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

// hashJoinBuildOnLeft is the buildOnLeft argument both join operators are
// constructed with, and the invariant that lets a WRITING statement admit the
// hash join without any order guard (rmp #2225 part B).
//
// It is false: the output is probe||build, which is outer||inner, which is the
// column order the Apply being replaced emits. It is a named constant rather
// than a bare `false` at the call site because it is not an incidental argument
// — it is half of the order-preservation guarantee, and the other half (that
// build is ALWAYS apply.Inner) is enforced at the same call site.
//
// # The claim
//
// The hash join's output is row-for-row IDENTICAL to the nested loop it replaces
// — not merely multiset-identical, which is all [HashJoin]'s own doc comment
// claims and all the read path's order-safety scan assumes.
//
// # Why it holds
//
//  1. THE BUILD SIDE IS PINNED BY THE PLANNER, NOT SELECTED AT RUNTIME.
//     [tryBuildHashJoin] is the only construction site for either join operator,
//     and it always passes apply.Inner as build and apply.Outer as probe. Neither
//     [exec.HashJoin] nor [exec.ColumnarHashJoin] contains any code that swaps
//     them; both take the assignment from their constructor and keep it.
//  2. THE PROBE DRIVES THE OUTPUT, IN OUTER ORDER. Both operators emit
//     probe-major: they pull one probe row, drain its bucket to exhaustion, then
//     pull the next. So the outer arm's own emission order is the output's major
//     order — exactly the Apply's.
//  3. WITHIN A PROBE ROW, MATCHES COME OUT IN INNER-SCAN ORDER. Equal keys hash
//     equal, so every row that matches a given probe key lives in ONE bucket, and
//     a bucket is append-only over the build drain — its contents are in inner-scan
//     order. The scan walks the bucket front-to-back, skipping hash collisions with
//     an exact [expr.Value.Equal] check, so the surviving matches are emitted in
//     inner-scan order: the minor order the Apply produces.
//  4. THE DISCARDED ROWS ARE EXACTLY THE NON-MATCHES. Only NULL/NaN keys are
//     dropped before bucketing ([isUnjoinableKey]), and those can never satisfy the
//     equi-join the Selection applies, so the nested loop drops them too.
//
// (1)+(2)+(3)+(4) give the same rows in the same positions. The residual
// predicate is then re-applied above the join, preserving Selection semantics.
//
// # What would break it
//
// Making the operator choose its build side by measured cardinality — the classic
// optimisation, and what the round-4 audit assumed was already happening. Doing
// that would make the substitution order-CHANGING, would reintroduce the need for
// an order guard on the read path, and would make it unusable for a writing
// statement, where `SET` is last-write-wins. If that optimisation is ever wanted,
// the swap must be gated on the write path being absent AND on the read path's
// order-safety scan.
const hashJoinBuildOnLeft = false

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
