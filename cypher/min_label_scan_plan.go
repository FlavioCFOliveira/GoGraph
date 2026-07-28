package cypher

// min_label_scan_plan.go — min-cardinality multi-label anchor scan (#2077,
// planner increment F1b of docs/optimizer-activation-design.md).
//
// When a node pattern carries more than one label, the IR translator anchors
// the scan on the FIRST syntactic label and re-checks the rest as a residual
// LabelPredicate Filter:
//
//	MATCH (n:A:B:C) ...
//	  → Selection{ (n:B:C) } over NodeByLabelScan(n, A)
//
// A label conjunction is a commutative AND, so the set of nodes carrying every
// label is independent of which label anchors the scan:
//
//	{n : n∈A ∧ n∈B ∧ n∈C}  =  scan(Lᵢ) ∩ filter(the other labels)   for any i.
//
// Anchoring on the SMALLEST-cardinality label therefore produces an identical
// result multiset while scanning min_i|Lᵢ| ≤ |A| candidate rows — never more
// than today's default. This is a BUILD-TIME physical substitution (approach
// (a) of the task): the IR stays byte-identical; only the physical scan target
// and the residual Filter's label set change, exactly like the equality
// index-seek and range-seek peepholes that live alongside it in api.go.
//
// # Why build-time, not IR
//
// Label cardinalities are not available in the pure IR translator (cypher/ir);
// they live only at physical build time where the label resolver (labelSrc /
// *lpgLabelResolver) is threaded, the same place tryBuildIndexSeekFromSelection
// and the hash-join peephole run.
//
// # Guards (all mandatory, all preserving result-identity and no-regression)
//
//   - EXACT counts only. Every candidate label's cardinality is the exact
//     live-node count from the label index (EstExact, #2076). The
//     trustworthiness veto (planStaysDefault) forces the default plan if any
//     estimate is untrustworthy — so a nil/opaque resolver can never drive a
//     deviation.
//   - EMPTY short-circuit. A label whose exact cardinality is zero makes the
//     whole conjunction empty; it is the minimum and is chosen, yielding a
//     provably-empty scan rather than a full scan of a populated label followed
//     by an all-dropping filter.
//   - DETERMINISTIC tie-break. Equal cardinality is broken by the lowest label
//     id, then the lowest syntactic index — a total order, so plans are stable
//     and reproducible run to run.
//   - INDEX-SEEK precedence. This peephole recognises only a bare LabelPredicate
//     Selection; it never matches the equality predicate the index-seek peephole
//     consumes, and it is invoked AFTER tryBuildIndexSeekFromSelection in the
//     Selection build, so an equality seek always wins.
//   - RESULT identity. The residual LabelPredicate covers EXACTLY the labels not
//     chosen as the scan anchor, so the surviving row multiset equals the
//     Labels[0] plan for every case (zero-population, tie, extra labels, inline
//     property, WHERE, OPTIONAL MATCH). The Filter is built through the same
//     newRowPredicate the default Selection build uses, so evaluation is
//     byte-identical.
//
// # Gating
//
// Behind EngineOptions.DisableMinLabelScan → Engine.minLabelScanEnabled →
// buildOpts.minLabelScanEnabled (default ENABLED), mirroring DisableHashJoin /
// DisableRangeIndexSeek. The read path (Engine.Run) sets the build flag; every
// other build path (write path, public BuildPlanWithMutator) leaves it false
// and always builds today's Labels[0] plan.

import (
	"math"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// minLabelScanBuildCount counts how many times the planner has re-anchored a
// multi-label node scan on a smaller-cardinality label than Labels[0]. It is a
// diagnostic seam read only by the in-package differential test to assert the
// substitution actually fired (or, under a guard, did not). It is process-global
// and monotonic; tests snapshot it before/after a query rather than resetting
// it, so concurrent tests do not interfere.
var minLabelScanBuildCount atomic.Uint64

// labelIDResolver exposes a stable label id used only to break ties between
// equal-cardinality candidate labels deterministically. The production
// *lpgLabelResolver implements it; a resolver that does not falls back to the
// syntactic-index tie-break in [pickMinLabel].
type labelIDResolver interface {
	ResolveLabelID(name string) (uint32, bool)
}

// labelID returns a stable id for tie-breaking equal-cardinality labels. An
// unavailable id sorts last (MaxUint32) so a label with a real id wins a tie;
// [pickMinLabel]'s syntactic-index tie-break then guarantees full determinism.
func labelID(idSrc labelIDResolver, label string) uint32 {
	if idSrc == nil {
		return math.MaxUint32
	}
	if id, ok := idSrc.ResolveLabelID(label); ok {
		return id
	}
	return math.MaxUint32
}

// pickMinLabelShape is the STRUCTURAL half of the recogniser: it accepts exactly
// a Selection whose predicate is a bare LabelPredicate over the same node its
// child NodeByLabelScan binds, and reports the scan variable, the scanned label
// and the extra labels the predicate re-checks — with no cardinality lookup and
// no cost decision.
//
// It is shared with the bitmap-intersection peephole (#2133) so the two cannot
// disagree about what a "bare multi-label LabelPredicate Selection" is; a shape
// they read differently would let one fire where the other declines and make the
// precedence order meaningless.
func pickMinLabelShape(sel *ir.Selection) (nodeVar, scanLabel string, extraLabels []string, ok bool) {
	lp, isLP := sel.PredicateExpr.(*ast.LabelPredicate)
	if !isLP || len(lp.Labels) == 0 {
		return "", "", nil, false
	}
	recv, isVar := lp.Receiver.(*ast.Variable)
	if !isVar {
		return "", "", nil, false
	}
	lblScan, isScan := sel.Child.(*ir.NodeByLabelScan)
	if !isScan || lblScan.Label == "" {
		return "", "", nil, false
	}
	if recv.Name != lblScan.NodeVar {
		// The LabelPredicate re-checks a DIFFERENT variable (e.g. a bound endpoint
		// via matchApplyNodeLabels) — not the freshly scanned node — so neither the
		// min-label rewrite nor the intersection applies.
		return "", "", nil, false
	}
	return lblScan.NodeVar, lblScan.Label, lp.Labels, true
}

// pickMinLabel is the pure planner decision shared by the build path and the
// EXPLAIN renderer. It recognises a Selection whose predicate is a bare
// LabelPredicate over the SAME node the child NodeByLabelScan binds, then picks
// the minimum-cardinality label among {scan label} ∪ {residual labels}.
//
// It returns ok == false — meaning "keep today's default plan" — when: the
// Selection is not a bare-LabelPredicate over a NodeByLabelScan of the same
// variable; the trustworthiness veto trips; or the winner is already Labels[0]
// (the substitution would reproduce the default plan byte-for-byte, so the
// default build handles it and the diagnostic counter does not advance).
//
// On success it returns the scan variable, the chosen (minimum) label, and the
// residual labels the Filter must re-check — exactly the candidate set minus the
// chosen label, in original syntactic order.
func pickMinLabel(sel *ir.Selection, labelSrc labelResolverIface) (nodeVar, chosen string, residual []string, ok bool) {
	scanVar, scanLabel, extra, shapeOK := pickMinLabelShape(sel)
	if !shapeOK {
		return "", "", nil, false
	}
	lblScan := &ir.NodeByLabelScan{NodeVar: scanVar, Label: scanLabel}

	// Candidate set in syntactic order: [L0, extra0, extra1, ...] — exactly the
	// node pattern's Labels, so the syntactic-index tie-break matches the written
	// order.
	candidates := make([]string, 0, len(extra)+1)
	candidates = append(candidates, scanLabel)
	candidates = append(candidates, extra...)

	// Trustworthiness veto (#2076, design §2.1): gather an estExact cardinality
	// for every candidate; if ANY estimate is untrustworthy, keep the default
	// plan. The exact label-count provider always yields estExact, so this clears
	// in production; it fails closed for a nil/opaque resolver.
	ests := make([]estimate, len(candidates))
	for i, lbl := range candidates {
		ests[i] = labelCardinalityEstimate(labelSrc, lbl)
	}
	if planStaysDefault(ests...) {
		return "", "", nil, false
	}

	idSrc, _ := labelSrc.(labelIDResolver)

	// Choose the minimum-cardinality candidate. A zero count (empty short-circuit)
	// is the natural minimum and wins. Ties break by lowest label id, then lowest
	// syntactic index (we switch only on a STRICT improvement, so the earliest
	// index survives an exact (count,id) tie) — a total order, fully
	// deterministic.
	chosenIdx := 0
	chosenCount := ests[0].rows
	chosenID := labelID(idSrc, candidates[0])
	for i := 1; i < len(candidates); i++ {
		c := ests[i].rows
		id := labelID(idSrc, candidates[i])
		if c < chosenCount || (c == chosenCount && id < chosenID) {
			chosenIdx, chosenCount, chosenID = i, c, id
		}
	}

	if chosenIdx == 0 {
		// L0 already wins: the substitution would reproduce today's default plan
		// byte-for-byte. Decline so the default build path handles it.
		return "", "", nil, false
	}

	residual = make([]string, 0, len(candidates)-1)
	for i, lbl := range candidates {
		if i == chosenIdx {
			continue
		}
		residual = append(residual, lbl)
	}
	return lblScan.NodeVar, candidates[chosenIdx], residual, true
}

// buildMinLabelScanIfEnabled is the gated entry point invoked from the Selection
// case of buildOperator, AFTER tryBuildIndexSeekFromSelection (so an equality
// index seek always wins). It returns (op, true, nil) when the min-label
// substitution fires and the operator — the minimum-label NodeByLabelScan
// wrapped by the residual LabelPredicate Filter — was built; (nil, false, nil)
// when the optimisation is disabled or the pattern is not eligible (the caller
// then builds the default Labels[0] plan).
func buildMinLabelScanIfEnabled(
	sel *ir.Selection,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
	bopts *buildOpts,
) (exec.Operator, bool, error) {
	if bopts == nil || !bopts.minLabelScanEnabled {
		return nil, false, nil
	}
	nodeVar, chosen, residual, ok := pickMinLabel(sel, labelSrc)
	if !ok {
		return nil, false, nil
	}
	// The residual label Filter's row predicate needs the graph to resolve node
	// labels; the lpgNodeWalker carries it (the same requirement the default
	// Selection Filter has). Without it, decline and let the default build run.
	lw, ok := walker.(*lpgNodeWalker)
	if !ok || lw.g == nil {
		return nil, false, nil
	}

	// Scan the chosen (minimum-cardinality) label, binding the node at the next
	// free schema slot — identical to what the default NodeByLabelScan build
	// leaves — BEFORE newRowPredicate snapshots the schema.
	schema[nodeVar] = schemaWidth(schema)
	scanOp := exec.NewNodeByLabelScan(chosen, &execLabelAdapter{labelSrc: labelSrc})

	// Residual LabelPredicate covers exactly the labels NOT scanned, so the row
	// multiset equals the Labels[0] plan (the conjunction is commutative). It is
	// evaluated through the same newRowPredicate the default Selection build
	// uses, so per-row evaluation is byte-identical.
	residualPred := &ast.LabelPredicate{
		Receiver: &ast.Variable{Name: nodeVar},
		Labels:   residual,
	}
	filter := exec.NewFilter(scanOp, newRowPredicate(residualPred, schema, lw.g, params, reg, bopts))
	minLabelScanBuildCount.Add(1)
	return filter, true, nil
}
