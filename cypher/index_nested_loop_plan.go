package cypher

// index_nested_loop_plan.go — planner trigger and cost gate for the index
// nested-loop join (rmp #2233).
//
// # The plan, and why it needs a gate rather than a preference
//
// A disconnected equi-join whose key varies per outer row has three plans, at
// three costs over a batch of B outer rows against a population of N indexed
// nodes:
//
//	nested loop            Θ(B·N)      — the pre-#1506 plan
//	hash join              Θ(N+B)      — #1506 / #2228
//	index nested-loop join Θ(B·log N)  — this file
//
// The last two do not order. #2228's decision record
// (docs/benchmarks/write-path-hash-join-2026-07-27.md §6) chose the hash join and
// recorded WHY the index nested-loop join was deferred rather than rejected: at
// the bulk-load harness shape (B=5000, N=20000) Θ(N+B) ≈ 25 000 beats
// Θ(B log N) ≈ 71 500, so shipping the seek unconditionally would have REGRESSED
// the 957× that task delivered. At B=500 the arithmetic reverses — 7 150 against
// 20 500 — and small-batch incremental ingest is the common regime.
//
// So the substitution is admitted only where the estimate says it wins. That gate
// is the substance of this change; the operator without it would be a regression
// dressed as an optimisation.
//
// # Why this is a new operator rather than a wider rewrite
//
// #2182's pass (correlated_seek_plan.go) already pushes a bound key into an
// Apply's inner arm so the ordinary seek machinery claims it — but it substitutes
// the key's AST at PLAN time, and therefore declines anything whose value can
// differ per row. That is not a limitation to widen: a plan-time substitution
// cannot express a per-row key at all. The key has to be evaluated at RUN time
// against the current row, which is a physical operator
// ([exec.IndexNestedLoopJoin]) — the same structure Neo4j plans here, an Apply
// over a per-row-parameterised NodeIndexSeek.
//
// # Order safety
//
// None is needed, on either path. The operator emits outer-major and, within an
// outer row, ascending node ids from both its paths — row-for-row identical to the
// nested loop, exactly as [hashJoinBuildOnLeft] establishes for the hash join. A
// writing statement can therefore use it too.

import (
	"math"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// indexNestedLoopBuildCount counts how many times the planner has substituted an
// index nested-loop join. Like [hashJoinBuildCount] it is a process-global,
// monotonic diagnostic seam, incremented once per plan build and read only by
// tests, which snapshot it around a query rather than resetting it.
//
// rmp #2233 acceptance criterion 4 requires the chosen plan to be asserted from
// this counter rather than from Engine.Explain: a rendering can agree with the
// planner's intent while the built operator differs, and #2222 found exactly that
// class of divergence in Explain itself.
var indexNestedLoopBuildCount atomic.Uint64

// indexNestedLoopMinPopulation is the inner-population floor below which the seek
// is not attempted at all.
//
// It exists for the same reason [hashJoinSizeFloor] does: below it the asymptotic
// argument stops paying for its constant factor, and the log N term is small
// enough that the gate's arithmetic is dominated by noise. It is also the point
// below which a full label scan is a handful of cache lines.
const indexNestedLoopMinPopulation = 64

// tryBuildIndexNestedLoopJoin attempts to build an [exec.IndexNestedLoopJoin] for
// a Selection over a plain Apply whose equi-join key varies per outer row. It
// returns (op, true, nil) when the structural trigger, the index requirement and
// the cost gate all hold; (nil, false, nil) when the shape is not eligible (the
// caller falls through to the hash join and then to the plain Selection build); or
// (nil, false, err) on a build error.
//
// It must be tried BEFORE [tryBuildHashJoin]: both claim the same structural
// shape, and this one is admitted only in the regime where it is the cheaper of
// the two, so the hash join is the correct fallback for everything it declines.
func tryBuildIndexNestedLoopJoin(
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
	if bopts == nil || !bopts.indexNestedLoopEnabled || idxMgr == nil {
		return nil, false, nil
	}
	if sel.PredicateExpr == nil {
		return nil, false, nil
	}
	apply, ok := sel.Child.(*ir.Apply)
	if !ok {
		return nil, false, nil
	}
	lw, ok := walker.(*lpgNodeWalker)
	if !ok || lw.g == nil {
		return nil, false, nil
	}

	// The inner arm must be a bare label scan: that is what makes the population
	// exactly "the nodes an index on (label, property) covers", and what lets the
	// operator's fallback path BE the nested loop's inner arm.
	innerVar, innerLabel, ok := scanLeafNodeVar(apply.Inner)
	if !ok || innerLabel == "" {
		return nil, false, nil
	}

	outerVars := collectPlanVars(apply.Outer)
	innerVars := collectPlanVars(apply.Inner)
	conjuncts := splitConjuncts(sel.PredicateExpr)
	keyIdx, key := findEquiJoinKey(conjuncts, outerVars, innerVars)
	if keyIdx < 0 {
		return nil, false, nil
	}
	// The inner side of the key must be exactly innerVar.<prop>, so the index that
	// covers (innerLabel, prop) is the index that answers the seek. Anything else
	// — a function over the property, arithmetic, a second variable — is not a
	// seekable key.
	propKey, ok := propertyKeyOf(key.innerKey, innerVar)
	if !ok {
		return nil, false, nil
	}
	numIdx, ok := findBoundNumericBTree(idxMgr, innerLabel, propKey)
	if !ok {
		return nil, false, nil
	}
	pointIdx, ok := numIdx.(exec.NumericPointLookup)
	if !ok {
		return nil, false, nil
	}

	// ── The cost gate ──
	innerRows, ok := estimateLeadingScanRows(apply.Inner, labelSrc)
	if !ok || innerRows < indexNestedLoopMinPopulation {
		return nil, false, nil
	}
	// COVERAGE. The numeric companion must hold an entry for EVERY node the inner
	// scan would produce, which — since a node contributes at most one entry —
	// proves that every one of those nodes carries a numeric value for this
	// property.
	//
	// This is not a refinement, it is the guard that makes the plan safe to offer
	// at all, and it was added because measurement caught it missing. Without it,
	// the mere EXISTENCE of a companion admitted the seek: on the three-way load
	// harness, whose join key is the string `sid`, the companion exists and is
	// empty, so every outer row fell through to the operator's scan fallback and
	// the shape went Θ(B·N). Measured at N=16000 that was 9.157 ms → 3.336 s on the
	// read and 9.651 ms → 3.174 s on the write — a 364× regression, and precisely
	// the outcome #2233 warned that a missing gate would produce.
	//
	// With coverage proved, a key of any non-numeric kind matches nothing rather
	// than needing a scan to discover that, so the fallback is confined to the one
	// case that genuinely needs it: an integer too large for float64 to hold
	// exactly (see exec.exactFloat64Key).
	if !numericIndexCoversScan(numIdx, innerRows) {
		return nil, false, nil
	}
	outerRows, ok := estimateOuterRows(apply.Outer, labelSrc, params)
	if !ok {
		// No estimate means no basis to prefer this plan over the hash join that
		// ships today. Decline rather than guess: an unanalysable outer arm is
		// exactly where B could be anything.
		return nil, false, nil
	}
	if !indexNestedLoopWins(outerRows, innerRows) {
		return nil, false, nil
	}

	g := lw.g

	// Build the outer (probe) arm into the shared schema, mirroring the
	// plain-Apply build so the combined layout is preserved — identical
	// bookkeeping to tryBuildHashJoin.
	outerOp, err := buildOperator(apply.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
	if err != nil {
		return nil, false, err
	}
	outerWidth := schemaWidth(schema)
	probeKeySchema := copySchema(schema)

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
	// The inner arm is built exactly as the plain-Apply case builds it, and serves
	// the operator's FALLBACK path. A plain Apply's inner arm is uncorrelated, so
	// re-Init'ing it per fallback row restarts it independently — the same contract
	// tryBuildHashJoin relies on for its build drain.
	innerOp, err := buildOperator(apply.Inner, walker, labelSrc, reg, params, innerSchema, idxMgr, procReg, argByTag, bopts)
	if err != nil {
		return nil, false, err
	}
	for k, v := range innerSchema {
		schema[k] = v + outerWidth
	}
	shiftApplyMetaColumns(bopts, outerWidth, preEdgeKeys, prePathChainKeys, prePathMetaKeys, preVLEKeys, preTripletLen)

	buildKeySchema := copySchema(innerSchema)
	innerKeyExpr := key.innerKey
	outerKeyExpr := key.outerKey

	outerKeyFn := func(row exec.Row) (expr.Value, error) {
		rc := buildRowCtx(row, probeKeySchema, g, bopts)
		return evalRow(bopts, outerKeyExpr, rc, params, reg)
	}
	innerKeyFn := func(row exec.Row) (expr.Value, error) {
		rc := buildRowCtx(row, buildKeySchema, g, bopts)
		return evalRow(bopts, innerKeyExpr, rc, params, reg)
	}

	// Coverage was proved above, so tell the operator: a non-numeric key then needs
	// no scan to establish that it matches nothing.
	var op exec.Operator = exec.NewIndexNestedLoopJoin(outerOp, innerOp, pointIdx, outerKeyFn, innerKeyFn).
		WithProvenNumericCoverage(true)

	// Re-apply every residual conjunct as a Filter on the combined row, preserving
	// Selection(fullPredicate, …) semantics — the same treatment the hash join
	// gives them.
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
	indexNestedLoopBuildCount.Add(1)
	return op, true, nil
}

// numericIndexCoversScan reports whether the numeric companion holds an entry for
// every node the inner scan would produce.
//
// Each node contributes at most one entry to the companion, so entries ≥ scanRows
// is equivalent to "every scanned node has a numeric value for this property". The
// count is taken over the whole key domain, with a budget large enough that the
// answer is EXACT — an inexact count would make the guard a guess, and the guard
// is the difference between a 40× win and a 364× regression.
//
// A NaN-valued property is not counted (NaN falls outside [-∞, +∞] under the
// btree's float64 ordering), which makes the guard conservative in the right
// direction: a population containing a NaN reads as not-covered and declines.
func numericIndexCoversScan(numIdx boundNumericRange, scanRows int) bool {
	if scanRows <= 0 {
		return false
	}
	// The budget must exceed the population, or RangeCount stops early and reports
	// inexact. One extra row is enough to distinguish "at least scanRows" from
	// "exactly scanRows".
	budget := uint64(scanRows) + 1
	entries, exact := numIdx.RangeCount(math.Inf(-1), math.Inf(1), budget)
	if !exact {
		return false
	}
	return entries >= uint64(scanRows)
}

// indexSeekLevelsPerHashRow is how many btree levels cost what ONE hash-join row
// costs. It is the constant that makes Θ(B·log₂N) and Θ(N+B) comparable, and it
// is measured, not assumed.
//
// #2228's decision record compared the two plans as raw UNIT counts — build rows
// against tree levels — on the implicit assumption that a unit of each costs about
// the same. It does not, and the difference is an order of magnitude. Fitting
// bench/r4audit's TestIndexNestedLoopCrossover at N=20000 (six batch sizes from
// B=500 to B=500000, each plan forced):
//
//	one btree level         ≈  30 ns   — a bounded search inside a cached node
//	one hash build row      ≈ 550 ns   — allocates and copies a whole row
//	one hash probe row      ≈ 450 ns
//
// so a level is worth roughly 1/15 of a row. That single constant explains the
// whole measured table, including the two cells the unit model got backwards: at
// N=20000 it puts 30·log₂N ≈ 429 ns below the 450 ns a probe alone costs, so the
// seek's per-row price is under the hash join's BEFORE the N-row build is counted
// — which is why the seek measured faster at every ratio up to B = 25·N, by 40.2×
// at B=500 and still 1.15× at B=500000.
//
// It is deliberately ONE round number rather than three separate nanosecond
// figures. The absolute times are machine-specific and would rot; the ratio is a
// property of the two algorithms' inner loops (a cache-resident comparison against
// an allocation plus a row copy) and travels much better. Re-derive it with
// TestIndexNestedLoopCrossover before changing it.
const indexSeekLevelsPerHashRow = 15

// indexNestedLoopWins reports whether the seek plan beats the hash join for the
// given estimates.
//
// The comparison is B·log₂N levels against [indexSeekLevelsPerHashRow]·(N+B)
// level-equivalents. It remains a REGIME test rather than a cost model — there is
// no attempt to predict a time — and it stays biased towards the incumbent: ties
// go to the hash join, which is what ships today and which needs no index.
//
// The regime where the hash join wins is real but distant: the seek's advantage
// narrows as B grows (40.2× at B=500 down to 1.15× at B=500000, N=20000) and the
// crossover moves out with the index depth, reaching B ≈ 1.1M for N=80000. This
// gate exists to hold that boundary, and to hold it against a graph deep enough
// that log₂N stops being small — not to second-guess the measured range.
func indexNestedLoopWins(outerRows, innerRows int) bool {
	if outerRows <= 0 || innerRows <= 0 {
		return false
	}
	seekLevels := float64(outerRows) * math.Log2(float64(innerRows))
	hashLevels := indexSeekLevelsPerHashRow * (float64(innerRows) + float64(outerRows))
	return seekLevels < hashLevels
}

// propertyKeyOf returns the property key when e is exactly `nodeVar.<key>`, the
// only inner-side key shape an index can answer.
func propertyKeyOf(e ast.Expression, nodeVar string) (string, bool) {
	prop, ok := e.(*ast.Property)
	if !ok || prop.Key == "" {
		return "", false
	}
	v, ok := prop.Receiver.(*ast.Variable)
	if !ok || v.Name != nodeVar {
		return "", false
	}
	return prop.Key, true
}

// estimateOuterRows estimates B, the number of rows the outer (probe) arm will
// produce. ok is false when no sound estimate is available, which the caller
// treats as "decline the substitution".
//
// Two sources, and no guessing beyond them:
//
//   - An UNWIND over a parameter. The list's length is EXACT and known at bind
//     time, which is what makes the gate trustworthy for the bulk-load shape this
//     task targets — the batch size is the one number the caller always knows.
//     An UNWIND over a literal list is equally exact.
//   - Otherwise the arm's leading scan cardinality, the same estimate the hash
//     join's size floor uses for its build arm.
//
// An UNWIND whose list is neither (a computed expression, a subquery) has no
// bind-time length, so it returns false rather than substituting a default: a
// wrong B is how this optimisation would become the regression #2228 warned of.
func estimateOuterRows(arm ir.LogicalPlan, labelSrc labelResolverIface, params map[string]expr.Value) (int, bool) {
	if n, ok := unwindRowCount(arm, params); ok {
		return n, true
	}
	return estimateLeadingScanRows(arm, labelSrc)
}

// unwindRowCount returns the exact row count of an UNWIND-rooted arm whose list
// length is known at bind time, multiplied by its child's row count when it has
// one. ok is false for any other shape.
func unwindRowCount(arm ir.LogicalPlan, params map[string]expr.Value) (int, bool) {
	unwind, ok := arm.(*ir.Unwind)
	if !ok {
		return 0, false
	}
	listLen, ok := boundListLength(unwind.ListExpr, params)
	if !ok {
		return 0, false
	}
	// UNWIND fans each input row out into listLen rows. With no child there is one
	// implicit input row.
	if unwind.Child == nil {
		return listLen, true
	}
	childRows, ok := unwindRowCount(unwind.Child, params)
	if !ok {
		return 0, false
	}
	return childRows * listLen, true
}

// boundListLength returns the length of a list expression whose value is known at
// bind time: a parameter holding a list, or a list literal. ok is false for
// anything computed.
func boundListLength(e ast.Expression, params map[string]expr.Value) (int, bool) {
	switch n := e.(type) {
	case *ast.Parameter:
		v, ok := params[n.Name]
		if !ok {
			return 0, false
		}
		lv, ok := v.(expr.ListValue)
		if !ok {
			return 0, false
		}
		return len(lv), true
	case *ast.ListLiteral:
		return len(n.Elements), true
	default:
		return 0, false
	}
}
