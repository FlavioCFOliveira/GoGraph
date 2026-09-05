package cypher

// plan_estimate_physical.go — attaching the planner's cardinality estimate to the
// PHYSICAL operator it planned (rmp #2765).
//
// # The problem this solves
//
// The estimates live on the LOGICAL plan. [explainWithIndexesNode] computes them
// during its walk and [Engine.ExplainTable] renders them in an Est.Rows column,
// but that walk executes nothing and its lines describe logical nodes.
// [Engine.ProfileTable] renders the PHYSICAL tree with the MEASURED rows. The two
// tables therefore describe two different plans, their lines do not correspond one
// to one, and a reader could not put a prediction next to its outcome — divergence
// D3 of docs/explain-profile-honesty-audit-2026-09-03.md §5, recorded there as the
// largest gap.
//
// # The rule, and why it is sound
//
// There is exactly one point in the codebase where a logical node and the operator
// built from it are both in hand: [buildOperator], the single funnel every physical
// operator passes through on its way out of the recursive lowering. This file is
// what happens there.
//
// The attribution rule is: **the first (deepest) logical node whose lowering
// RETURNS a given operator owns it.** [buildOperator] records a claim for every
// operator it returns — with an estimate when one is derivable, and with an
// explicitly EMPTY estimate when it is not — and a claim is never overwritten.
//
// That single rule is what makes the mapping sound rather than plausible, because
// several lowerings return an operator they did not build:
//
//   - a [ir.Selection] whose pushed seek hint no seek claimed returns its CHILD's
//     operator unchanged, deliberately not evaluating its own predicate
//     (cypher/api.go, the dropped-hint branch). Attributing the Selection's
//     (selective) estimate to it would promise a filtered count from an operator
//     that filters nothing;
//   - a [ir.Selection] over a shortestPath fuses its predicate INTO the child
//     operator and returns that child;
//   - an [ir.Expand] built without a graph returns its child untraversed.
//
// In all three the child claimed the operator first, so the parent's estimate is
// never attached. The rule is enforced by the map itself rather than by three
// special cases, which is what stops a fourth such lowering from silently
// producing a wrong number.
//
// # Shapes that CANNOT be mapped, and are left empty on purpose
//
// These render "-" in the table and print nothing in the tree. Each is a deliberate
// absence, not an oversight — a wrong correspondence between an estimate and an
// operator would read as a planner error that never happened, which is worse than
// no correspondence at all.
//
//  1. The two BUILD-SYNTHESISED leaves. The range-seek leaf
//     ([exec.NodeByIndexRangeScan] substituted for a Selection's scan child, #1505)
//     and the min-label re-anchored scan (#2077) are created inside a composite
//     lowering and have NO logical node at all: the [ir.NodeByLabelScan] they
//     replace is never built, and the label the min-label rewrite scans is picked at
//     BUILD time. The logical table does show an estimate on both — it synthesises
//     the line and calls [rangeSeekLeafEstimate] / [labelScanEstimate] for it — but
//     that is the logical renderer inventing a line, and there is no operator-side
//     equivalent to invent it from. Reaching them would need a second
//     estimate-computation site inside each rewrite; that is not done here.
//  2. The MORSEL-PARALLEL leaves. Their fused sub-plan is rebuilt per morsel from
//     FRESH IR on a worker goroutine, and the per-worker build options clear this
//     map for the same reason they clear the profiler — a shared map written from N
//     goroutines is `fatal error: concurrent map writes`, which is rmp #2664's
//     defect with a different field. The leaf itself is not built from any of the
//     four estimated node types.
//  3. The COLUMNAR fusion chains. A recognised scan→filter→project chain builds
//     [exec.ColumnarFilter] / [exec.ColumnarProject] directly, and the operators
//     [buildOperator] produced for the underlying logical nodes are discarded, so
//     their claims are never looked up.
//
// # Which resolver the estimate is read through, and why it is NOT the build's
//
// The estimate is read through a LIVE resolver — [Engine.estimateResolver], the
// same `&lpgLabelResolver{g: e.g.ReadAt(nil), eng: e}` [Engine.explainInputsFor]
// builds for the logical walk — and NOT through the resolver the physical build
// binds to the query's pinned snapshot.
//
// That is a decision, and the reason is measured rather than stylistic.
// [lpgLabelResolver.ResolveLabelCount] answers "EXACT or nothing" and declines the
// moment any MVCC history is live, which in a mixed read/write workload is
// essentially always (the finding that motivated [lpgLabelResolver.ResolveLabelCountBound],
// rmp #2392). statsRangeEstimateInner takes that count as its Δ/N denominator with
// `n, _ := src.ResolveLabelCount(label)` — the decline is not distinguished from a
// real zero — and its `n <= 0` guard then demotes the whole estimate to
// estFallback. Read through the build's snapshot resolver, a histogram range
// estimate therefore renders as "-" on the physical surface while the logical table
// shows the same node's estimate as "~84, stats": the same node of the same plan,
// with a figure on one surface and an absence on the other, the absence appearing
// and disappearing with unrelated write traffic. That is precisely the class of
// silent, data-dependent dishonesty this sprint exists to remove.
//
// Reading live makes the two surfaces agree BY CONSTRUCTION, which is the property
// TestProfileEstimate_PhysicalAndLogicalAgreeAboutTheSameNode holds. It costs one
// resolver allocation per EXPLAIN or PROFILE build and nothing on the query path,
// and it is sound for the same reason explain_estimate.go already gives for the
// logical walk: the estimate is DISPLAY-ONLY, every provider read is individually
// lock-free and tear-free, and no plan decision consults it. The decisions that DO
// consult a resolver — the min-label re-anchor, the anchor swap, the disjoint
// reorder — keep reading the build's pinned snapshot, untouched.
//
// The `n, _ :=` conflation itself is a defect in the estimate provider, not in this
// file, and it belongs to the planner-statistics work (rmp #2766) rather than here:
// nothing this task does may change what the planner decides.
//
// # Cost when off
//
// The map is nil for every ordinary [Engine.Run]: only the EXPLAIN and PROFILE
// build paths allocate one. [buildOperator] therefore adds one nil comparison per
// operator built, and computes no estimate and allocates nothing on the query path.

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// planEstimateCollector is the state a plan-RENDERING build carries so
// [buildOperator] can record an estimate for each operator it returns.
//
// The two fields are ONE object because they are inseparable: a build either
// collects estimates — in which case it needs both the map to put them in and the
// resolver to read them through — or it does not, and needs neither. Carrying them
// as two fields on [buildOpts] made a half-set state representable and cost the
// query path two words on a struct it allocates per execution; one pointer makes
// the invariant structural and costs one.
type planEstimateCollector struct {
	// into is the caller's map, which the caller reads back after the build.
	into exec.PlanEstimates
	// src is the LIVE label resolver — deliberately not the build's snapshot-pinned
	// one. See the file header for the measured reason they differ.
	src *lpgLabelResolver
}

// recordPlanEstimate claims op for plan and records the planner's estimate for it,
// unless some deeper logical node has already claimed op.
//
// It is called from [buildOperator] and nowhere else, so the claim order is the
// recursion's own: children are claimed before their parents, which is precisely
// what "the deepest node owns it" requires.
//
// A claim is ALWAYS written when the operator is unclaimed, even when no estimate
// is derivable. The empty claim is load-bearing: it is what tells a pass-through
// parent that the operator is already spoken for. Distinguishing "claimed with no
// estimate" from "not yet claimed" is the whole reason [exec.PlanEstimates] is a
// map of values rather than a set.
func recordPlanEstimate(
	c *planEstimateCollector,
	plan ir.LogicalPlan,
	op exec.Operator,
	walker nodeWalkerIface,
	params map[string]expr.Value,
) {
	if c == nil || c.into == nil || op == nil || plan == nil {
		return
	}
	if _, claimed := c.into[op]; claimed {
		return
	}
	c.into[op] = physicalPlanEstimate(plan, walker, c.src, params)
}

// physicalPlanEstimate returns the planner's estimate for plan, in the form the
// physical plan carries, or the empty (absent) estimate when none is derivable.
//
// It consults the SAME providers, through the same helpers, that
// [explainWithIndexesNode] consults for the same node — [scanEstimate],
// [labelScanEstimate], [selectionEstimate], [expandEstimate]. Nothing is
// re-derived here and no second estimation model exists: a number that appears in
// the physical Est.Rows column is the number the logical walk would print for the
// same node, so the two surfaces cannot disagree about what the planner thought.
//
// Only four logical node types carry an estimate at all, which is why the switch is
// short: everything else returns the absent estimate and renders as no figure.
func physicalPlanEstimate(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	src *lpgLabelResolver,
	params map[string]expr.Value,
) exec.PlanEstimate {
	if src == nil {
		return exec.PlanEstimate{}
	}
	switch p := plan.(type) {
	case *ir.AllNodesScan:
		return toPlanEstimate(scanEstimate(plan, src, src.g))
	case *ir.NodeByLabelScan:
		return toPlanEstimate(labelScanEstimate(src, p.Label))
	case *ir.Selection:
		// A Selection's estimate is the count its PREDICATE selects, so it may only be
		// attached to an operator that actually applies that predicate. The lowering
		// falls back to a constant-true pass-through Filter when the walker carries no
		// graph, and this mirrors that exact condition rather than assuming it cannot
		// happen: an estimate on a filter that filters nothing would be a fabrication
		// of the most misleading kind — a small number beside a large measurement,
		// looking like a planner error.
		if !selectionAppliesItsPredicate(walker) {
			return exec.PlanEstimate{}
		}
		return toPlanEstimate(selectionEstimate(p, src, params))
	case *ir.Expand:
		return toPlanEstimate(expandEstimate(p, src))
	default:
		return exec.PlanEstimate{}
	}
}

// selectionAppliesItsPredicate reports whether a Selection lowered against walker
// builds a real predicate filter rather than the constant-true pass-through.
//
// It is the same test the lowering makes (`walker.(*lpgNodeWalker)` with a non-nil
// graph), written once here so the two cannot drift apart. On the engine's read
// path it is always true; it is false only for a build driven by a walker that is
// not the engine's own, which is a test construction.
func selectionAppliesItsPredicate(walker nodeWalkerIface) bool {
	lw, ok := walker.(*lpgNodeWalker)
	return ok && lw != nil && lw.g != nil
}

// toPlanEstimate converts one of the estimate helpers' three-value return into the
// physical plan's form, discarding the rendered annotation the tree renderer uses.
//
// A helper that reports ok=false has no estimate. So does one that reports
// estFallback: the number it carries stands in for a statistic that is absent,
// dirty or stale, and the established rule for that everywhere in this codebase is
// to omit rather than to print (see [planLine.estCell], which renders it "-", and
// [estimateAnnotation], which renders it as nothing). Collapsing both to
// [exec.EstimateAbsent] is what keeps the physical column and the logical column
// saying the same thing about the same node.
func toPlanEstimate(e estimate, _ string, ok bool) exec.PlanEstimate {
	if !ok {
		return exec.PlanEstimate{}
	}
	switch e.source {
	case estExact:
		return exec.PlanEstimate{Rows: estRows(e.rows), Source: exec.EstimateExact}
	case estStats:
		return exec.PlanEstimate{Rows: estRows(e.rows), Source: exec.EstimateStats}
	case estHeuristic:
		return exec.PlanEstimate{Rows: estRows(e.rows), Source: exec.EstimateHeuristic}
	default: // estFallback — absent, dirty or stale statistic.
		return exec.PlanEstimate{}
	}
}

// planEstimatesFor returns a map sized for a plan of about n operators, for a build
// that renders its plan. It exists so the three rendering call sites
// ([Engine.explainPhysical], [Engine.runExplainPrefixed] and
// [Engine.profileMaterialised]) allocate the same way from one place, and so that
// the ordinary [Engine.Run] — which passes nil — cannot acquire one by accident.
//
// The COLLECTOR is assembled inside [Engine.buildReadPhysical], which is where the
// live resolver can be built; the caller supplies only the map it will read back.
//
// The size hint is the logical plan's own node count, which bounds the physical
// tree only loosely (a composite lowering emits several operators for one logical
// node, and a rewrite subsumes two into one). It is a hint, not a bound: a map that
// grows is correct, merely slower, and this path renders a diagnostic.
func planEstimatesFor(plan ir.LogicalPlan) exec.PlanEstimates {
	return make(exec.PlanEstimates, logicalPlanSize(plan))
}

// logicalPlanSize counts the nodes of a logical plan, bounded so a pathological
// tree cannot make the size hint itself expensive. The bound is generous relative
// to any plan the parser's nesting guard admits.
func logicalPlanSize(plan ir.LogicalPlan) int {
	const (
		ceiling = 256
		floor   = 8
	)
	n := 0
	var walk func(ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil || n >= ceiling {
			return
		}
		n++
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	if n < floor {
		return floor
	}
	return n
}
