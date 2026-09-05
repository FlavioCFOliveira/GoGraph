package cypher

// join_reorder_plan.go — the disjoint-component ordering peephole (task #2091,
// the P-disjoint peephole of docs/reordering-design.md §0/§1/§2). It is the
// first, reverse-hazard-free unit of count-store-gated reordering.
//
// # What it does
//
// When a query joins DISJOINT single-scan components by a plain, uncorrelated
// Apply — a nested-loop Cartesian product with NO equi-join predicate, e.g.
//
//	MATCH (a:A), (b:B) RETURN a, b   →   Apply(scan(a:A), scan(b:B))
//
// it reorders the Apply so the component with the SMALLER exact base cardinality
// drives (is the outer side). [exec.Apply] is a Volcano dependent join: it
// re-initialises and re-drains the inner plan once per outer row, so driving
// with the smaller side re-runs the larger side fewer times. The Cartesian
// multiset is unchanged (a Cartesian product is commutative); only the emission
// order and the internal column layout change — the layout is addressed by name
// through the schema map, and the emission order is proven unobserved by
// [SuppressReorder].
//
// The equi-join case (`… WHERE a.x = b.y`) is the hash-join peephole's job
// (hash_join_plan.go); this peephole never builds a hash join and never touches
// that path.
//
// # Components
//
// A qualifying component is a BARE node scan — [ir.NodeByLabelScan] (exactly
// N(label) rows) or [ir.AllNodesScan] (exactly the live node total) — a FILTERED
// label scan (an [ir.Selection] carrying a single property comparison directly
// over a [ir.NodeByLabelScan], see below) — or a plain Apply of such components
// (a nested Cartesian). No expand, no edge/path metadata to relocate. A
// relationship scan is not a leaf operator in this engine (`()-[r]->()` lowers to
// a scan + Expand), so an E(relType)-costed component never arises here.
//
// # Two costs per component: what it EMITS and what it must TOUCH (rmp #2766)
//
// Until the filtered component existed, every arm emitted exactly the rows it
// scanned, so ONE number described it. A `Selection` breaks that: `(a:A {x: 1})`
// over a million-node `:A` with three matching rows must TOUCH a million rows to
// EMIT three. [exec.Apply] pays those two quantities in different places — the
// drain cost once for the outer arm and once per outer row for the inner arm — so
// a component now carries both ([componentCost]):
//
//	cost(outer, inner) = drain(outer) + rows(outer) * drain(inner)
//
// Driving with the three-row side is right precisely BECAUSE its million-row
// drain is then paid once instead of once per row of the other arm. Under the
// one-number model the filtered arm was not a component at all, so the candidate
// was skipped and the written order stood.
//
// # The filtered component's predicate shape
//
// A `Selection` qualifies only when its predicate is a SINGLE comparison of the
// scanned node's OWN property against a literal or a bound parameter — `n.p = v`
// or `n.p <op> v` for op in {<, <=, >, >=}, in either operand order. The shape is
// recognised by the same extractors the EXPLAIN estimate annotations use
// ([extractEqFromAST], [extractRangeComparison]), which require one operand to be
// `nodeVar.key` for the scan's OWN variable and the other to be a literal or a
// parameter. That structurally forbids a predicate referencing the other
// component's variables, so the arms stay independent and the product stays a
// Cartesian. A conjunction, a disjunction, an IS NULL, a function call or a
// cross-variable comparison is not a component and keeps today's plan.
//
// The emitted-row estimate comes from the property statistics
// ([statsEqualityEstimate] / [statsRangeEstimate], docs/statistics-design.md) —
// the FIRST plan decision in this engine to read them. The drain estimate is the
// exact N(label) the bare scan would have contributed.
//
// # The admissibility gate (design §1)
//
// A swap is admitted only when ALL hold, read from ONE count-store snapshot
// (the read-path build resolves them against the query's pinned snapshot, so the
// live node total and every label count are consistent with the query's graph):
//
//   - EVERY estimate on the path — both arms' drain AND rows — is trustworthy
//     (estExact or estStats). Node counts always are for the live resolver; a
//     nil/opaque resolver yields estFallback; an equality on a non-MCV literal
//     yields estHeuristic; an absent or stale statistic yields estFallback. The
//     trustworthiness veto [planStaysDefault] keeps the written order for every
//     one of those, UNCHANGED by rmp #2766. That veto is the safety property of
//     the whole peephole: wherever the statistic is not trustworthy the planner is
//     provably inert.
//   - The candidate is order-safe: SuppressReorder(spine) == false (design §4).
//   - The swap is a strict improvement under BOTH cost rules ([reorderSwapWins]):
//     the drain-aware nested-loop cost above, AND the plain emitted-row rule the
//     peephole has always used. The second conjunct is not redundant — see
//     [reorderSwapWins] for the index-rewrite realisation it guards against — and
//     it is what makes the whole gate collapse, bit for bit, onto today's rule
//     whenever no filtered component is present.
//
// Order-safety and structural qualification are pure functions of the immutable
// plan and are computed once per query at parse time ([collectReorderCandidates],
// memoised in the plan-cache entry). The live cardinality gate runs per query at
// build time ([computeReorderSwaps]).
//
// # Gating
//
// Behind EngineOptions.DisableJoinReorder → Engine.joinReorderEnabled →
// buildOpts.reorderSwap (default ENABLED), mirroring DisableHashJoin /
// DisableRangeIndexSeek / DisableMinLabelScan. Only the read path (Engine.Run)
// populates the swap map; every other build path leaves it nil and always builds
// the written order. The peephole is inert whenever a component count is not
// exact.

import (
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// joinReorderBuildCount counts how many times the planner has swapped the
// build/outer side of a disjoint-component Cartesian. It is a diagnostic seam
// read only by the in-package differential test to assert the swap actually
// fired (or, under a guard, did not). It is process-global and monotonic; tests
// snapshot it before/after a query rather than resetting it, so concurrent tests
// do not interfere.
var joinReorderBuildCount atomic.Uint64

// joinReorderMargin is the constant-factor guard on the cost comparison (design
// §1.5, margin ≥ 1). A swap is admitted only when the inner cardinality times
// this margin does not exceed the outer cardinality, in addition to being a
// strict improvement. It is 1.0 today (swap on any strict win, which is provably
// non-regressing for a re-executing nested loop); it is the single knob a future
// stats-driven mode would raise to demand a larger margin before deviating.
const joinReorderMargin = 1.0

// joinReorderStatsMargin is the constant-factor guard applied INSTEAD of
// [joinReorderMargin] when any estimate on the decision path is estStats — a
// histogram-derived selectivity rather than a maintained exact count.
//
// It is 3.0, the figure docs/optimizer-activation-design.md §4 prescribes for a
// statistics-driven reorder ("the candidate is >= 3x cheaper"), on the ground that
// join-ordering errors compound multiplicatively along a chain (Ioannidis &
// Christodoulakis, SIGMOD 1991) and a histogram estimate carries a certified but
// non-zero absolute selectivity error. An MCV hit is an exact per-value count and
// keeps the 1.0 margin; so does every all-exact decision, which is why every plan
// today's code chooses is reproduced unchanged.
const joinReorderStatsMargin = 3.0

// componentCost is what one reorder component costs the nested-loop join, in the
// two quantities [exec.Apply] pays separately:
//
//   - drain: the rows the arm must TOUCH to produce its output ONCE. For a scan
//     that is its cardinality; for a filtered label scan it is the FULL label
//     count, because the Selection filters a scan rather than seeking an index.
//   - rows: the rows the arm EMITS, which is how many times the OTHER arm is
//     re-initialised and re-drained when this one drives.
//
// They are equal for every component kind that existed before rmp #2766, which is
// why one number sufficed until a filtered component could appear.
type componentCost struct {
	drain estimate
	rows  estimate
	// rowsErr is the CERTIFIED ABSOLUTE error on rows, in rows. It is 0 for every
	// exact quantity (a label count, the live node total, an MCV per-value count)
	// and delta*N for a histogram range estimate, where delta = 1/B + Delta/N is the
	// error [statsRangeEstimate] certifies on the SELECTIVITY
	// (docs/statistics-design.md §3) and N is the label count — an upper bound on
	// the histogram's summarised total, so the product is an upper bound on the
	// row error.
	//
	// It exists because a POINT estimate cannot certify "never slower": the swap
	// decision is monotone increasing in rows(outer) and decreasing in rows(inner),
	// so an over-estimate of the first or an under-estimate of the second both push
	// TOWARDS deviating from the written order. Only a one-sided interval closes
	// that: the gate is evaluated with rows(outer) at its LOW bound and rows(inner)
	// at its HIGH bound, so a swap fires only when it wins across the whole
	// certified interval. With rowsErr == 0 the interval is a point and the gate is
	// identical to the plain comparison.
	rowsErr float64
}

// rowsLo is the component's emitted-row count at the LOW end of its certified
// interval. It is the figure used for the INCUMBENT driver, because fewer driver
// rows makes the written order look better and so is the conservative choice for
// the plan already in force.
func (c componentCost) rowsLo() float64 {
	return clampComponentRows(c.rows.rows-c.rowsErr, c.drain.rows)
}

// rowsHi is the component's emitted-row count at the HIGH end of its certified
// interval. It is the figure used for the CANDIDATE driver, because more
// candidate rows makes the swap look worse.
func (c componentCost) rowsHi() float64 {
	return clampComponentRows(c.rows.rows+c.rowsErr, c.drain.rows)
}

// clampComponentRows enforces the component invariant 0 <= rows <= drain, which
// both interval ends must satisfy: a scan cannot emit a negative number of rows,
// and a filter cannot emit more rows than it read. It is the single place the
// invariant lives, so neither the estimate providers nor the interval arithmetic
// can put a row count outside it on a decision path.
//
// For a component whose drain equals its rows and whose certified error is zero —
// every component kind that existed before rmp #2766 — both bounds are no-ops and
// each accessor returns the row count BIT-IDENTICALLY (x - 0.0 and x + 0.0 are
// exactly x for every finite x, and a value equal to the drain is not greater than
// it). That is what keeps [reorderSwapWins]'s reduction to the previous rule
// exact.
func clampComponentRows(v, drain float64) float64 {
	if v < 0 {
		return 0
	}
	if v > drain {
		return drain
	}
	return v
}

// isReorderComponent reports whether plan is a qualifying disjoint-reorder
// component: a bare node scan, or a plain Apply composing two such components
// with disjoint variables (a nested Cartesian). It is purely structural — no
// counts — so it is evaluated once at parse time.
func isReorderComponent(plan ir.LogicalPlan) bool {
	switch n := plan.(type) {
	case *ir.NodeByLabelScan, *ir.AllNodesScan:
		return true
	case *ir.Selection:
		_, ok := reorderFilteredScan(n)
		return ok
	case *ir.Apply:
		return isReorderBareComponent(n.Outer) && isReorderBareComponent(n.Inner) &&
			reorderVarsDisjoint(n.Outer, n.Inner)
	default:
		return false
	}
}

// isReorderBareComponent is [isReorderComponent] WITHOUT the filtered arm: a bare
// node scan, or a plain Apply composing two such. It is what a COMPOSED component
// is built from, and the restriction is deliberate.
//
// A composed component's drain is modelled as the product of its arms' drains,
// which is exactly the single number the peephole computed before rmp #2766 —
// so every plan today's code chooses over a composed component is reproduced
// unchanged, and, because the product is commutative, a component's modelled drain
// does NOT depend on whether its own arms were swapped. That second property is
// what keeps the flat pre-order candidate loop in [computeReorderSwaps] sound: a
// parent decides on a child's drain that the child's own decision cannot alter.
//
// Both properties hold only while every leaf inside a composed component has
// drain == rows. Admitting a filtered arm INSIDE a composed component breaks both:
// the true composed drain is drain(o) + rows(o)*drain(i), which is swap-DEPENDENT,
// and against which the product is not an approximation but a gross over-estimate
// when the outer arm is the selective one (a 10^6-row scan emitting 3, composed
// with a 1000-row scan, is 10^9 by the product and 1.003*10^6 in truth). Fixing
// that needs the swap-dependent drain AND a post-order pass that feeds each
// child's POST-decision drain to its parent — a change that also moves plans the
// current code chooses, and therefore belongs to its own task, not to this one.
//
// So a filtered component is admitted only as a DIRECT arm of a candidate Apply,
// where its drain is the exact N(label) and nothing composes it further.
func isReorderBareComponent(plan ir.LogicalPlan) bool {
	switch n := plan.(type) {
	case *ir.NodeByLabelScan, *ir.AllNodesScan:
		return true
	case *ir.Apply:
		return isReorderBareComponent(n.Outer) && isReorderBareComponent(n.Inner) &&
			reorderVarsDisjoint(n.Outer, n.Inner)
	default:
		return false
	}
}

// reorderFilteredScan recognises the FILTERED-component shape (rmp #2766): a
// Selection carrying a single property comparison, directly over a
// NodeByLabelScan, on that scan's OWN node variable. It returns the underlying
// scan, or false for every other Selection.
//
// It is PURELY STRUCTURAL — it resolves no parameter and reads no statistic — so
// it is a pure function of the immutable plan and may be evaluated once at parse
// time inside [isReorderComponent], alongside the rest of the memoised candidate
// collection. The extractors are handed a nil parameter map for exactly that
// reason: [astLiteralToValue] answers a parameter reference with expr.Null rather
// than an error, so the SHAPE is recognised identically whether the operand is a
// literal or a parameter, while the VALUE is deferred to build time
// ([reorderFilteredRows]) where this execution's parameters are bound.
//
// An [ir.AllNodesScan] child is deliberately NOT accepted: the statistics are
// keyed by (label, property), so an unlabelled scan has no statistic to consult
// and would contribute an estFallback the veto would reject anyway.
func reorderFilteredScan(sel *ir.Selection) (*ir.NodeByLabelScan, bool) {
	if sel == nil || sel.PredicateExpr == nil {
		return nil, false
	}
	scan, ok := sel.Child.(*ir.NodeByLabelScan)
	if !ok || scan.Label == "" || scan.NodeVar == "" {
		return nil, false
	}
	if _, _, okEq := extractEqFromAST(sel.PredicateExpr, scan.NodeVar, nil); okEq {
		return scan, true
	}
	if _, _, _, okRange := extractRangeComparison(sel.PredicateExpr, scan.NodeVar, nil); okRange {
		return scan, true
	}
	return nil, false
}

// reorderVarsDisjoint reports whether the variable sets introduced by two
// subplans are disjoint. A shared variable would make the join correlated (the
// translator emits a CorrelatedApply, a different IR type, in that case), so a
// plain Apply of two components is expected to be disjoint; the check is an
// explicit guard rather than an assumption.
func reorderVarsDisjoint(a, b ir.LogicalPlan) bool {
	av := collectPlanVars(a)
	for v := range collectPlanVars(b) {
		if _, ok := av[v]; ok {
			return false
		}
	}
	return true
}

// isReorderCandidate reports whether a plain Apply is a structurally-qualifying
// reorder point: both arms are disjoint components. Order safety and the live
// cost gate are applied separately.
func isReorderCandidate(ap *ir.Apply) bool {
	return isReorderComponent(ap.Outer) && isReorderComponent(ap.Inner) &&
		reorderVarsDisjoint(ap.Outer, ap.Inner)
}

// collectReorderCandidates returns every plain Apply in the plan that is a
// structurally-qualifying, ORDER-SAFE disjoint-reorder point, in a stable
// pre-order. It is a pure function of the immutable plan (structure + spine
// order-safety) and is memoised in the plan-cache entry; the per-query cost gate
// ([computeReorderSwaps]) consumes its output.
//
// The spine handed to [SuppressReorder] is the candidate's ancestor chain,
// nearest ancestor first, so a candidate inside a subquery or under a
// pattern-comprehension collector is correctly suppressed by the boundary
// operator on its spine.
func collectReorderCandidates(root ir.LogicalPlan) []*ir.Apply {
	if root == nil {
		return nil
	}
	var out []*ir.Apply
	var stack []ir.LogicalPlan // ancestors, root-first (top-down)
	var walk func(ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil {
			return
		}
		if ap, ok := p.(*ir.Apply); ok && isReorderCandidate(ap) {
			// Spine nearest-first = the ancestor stack reversed.
			spine := make([]ir.LogicalPlan, len(stack))
			for i := range stack {
				spine[i] = stack[len(stack)-1-i]
			}
			if !SuppressReorder(spine) {
				out = append(out, ap)
			}
		}
		stack = append(stack, p)
		for _, c := range p.Children() {
			walk(c)
		}
		stack = stack[:len(stack)-1]
	}
	walk(root)
	return out
}

// reorderComponentCardinality returns the drain and emitted-row cost of a
// qualifying component as provenance-tagged estimates, or (zero, false) when plan
// is not a component.
//
//   - NodeByLabelScan: drain == rows == N(label), estExact via the label index.
//   - AllNodesScan: drain == rows == the live node total passed in, estExact.
//   - Selection over a NodeByLabelScan (rmp #2766): drain is the EXACT N(label)
//     the underlying scan contributes, because the filter reads every row of the
//     label; rows is the property-statistics estimate of what survives the
//     predicate ([reorderFilteredRows]).
//   - Apply: the product of its arms in each quantity, tagged with the WEAKER of
//     the two provenances.
//
// A nil/opaque label resolver makes labelCardinalityEstimate return estFallback,
// which the caller's [planStaysDefault] veto rejects.
//
// # Why the Apply drain is the product of the arms' DRAINS
//
// One full drain of a nested Cartesian Apply(a, b) truly costs
// drain(a) + rows(a)*drain(b). The product drain(a)*drain(b) is used instead, for
// one reason that outranks its slight inaccuracy: for arms where drain == rows —
// every component kind that existed before this change — it is EXACTLY the single
// number the peephole has always computed, so no plan the current code chooses is
// disturbed. The two agree asymptotically (the exact form differs by the additive
// drain(a) term), and the product is the conservative direction for a filtered
// arm, since it never charges the composed component more than its parts.
func reorderComponentCardinality(plan ir.LogicalPlan, labelSrc labelResolverIface, params map[string]expr.Value, totalNodes int64) (componentCost, bool) {
	switch n := plan.(type) {
	case *ir.NodeByLabelScan:
		e := labelCardinalityEstimate(labelSrc, n.Label)
		return componentCost{drain: e, rows: e}, true
	case *ir.AllNodesScan:
		e := estimate{rows: float64(totalNodes), source: estExact}
		return componentCost{drain: e, rows: e}, true
	case *ir.Selection:
		scan, ok := reorderFilteredScan(n)
		if !ok {
			return componentCost{}, false
		}
		drain := labelCardinalityEstimate(labelSrc, scan.Label)
		rows, rowsErr := reorderFilteredRows(n, scan, labelSrc, params, drain)
		// The 0 <= rows <= drain invariant is enforced by [clampComponentRows] at
		// both interval ends rather than here, so a stale statistic reporting more
		// rows than the label now holds cannot reach the cost rule either way.
		return componentCost{drain: drain, rows: rows, rowsErr: rowsErr}, true
	case *ir.Apply:
		lo, ok1 := reorderComponentCardinality(n.Outer, labelSrc, params, totalNodes)
		hi, ok2 := reorderComponentCardinality(n.Inner, labelSrc, params, totalNodes)
		if !ok1 || !ok2 {
			return componentCost{}, false
		}
		if lo.rowsErr != 0 || hi.rowsErr != 0 {
			// Unreachable while [isReorderBareComponent] gates what may compose: a
			// bare arm's estimate is exact. Declining rather than compounding two
			// certified intervals keeps that gate the single place the restriction
			// is expressed.
			return componentCost{}, false
		}
		return componentCost{
			drain: estimate{rows: lo.drain.rows * hi.drain.rows, source: weakerSource(lo.drain.source, hi.drain.source)},
			rows:  estimate{rows: lo.rows.rows * hi.rows.rows, source: weakerSource(lo.rows.source, hi.rows.source)},
		}, true
	default:
		return componentCost{}, false
	}
}

// weakerSource combines two estimate provenances into the weaker of the pair.
// [estSource]'s constants are declared in exactly that order — estExact, estStats,
// estHeuristic, estFallback — so the weaker is the larger, and a pair is
// trustworthy iff both members are.
//
// It replaces the "estExact unless both are estExact, else estFallback" rule the
// composed component used before rmp #2766, and reproduces it exactly over the
// provenances that rule could ever see: [labelCardinalityEstimate] and the
// AllNodesScan total yield only estExact or estFallback, and this function maps
// (exact, exact) to exact and (exact, fallback) to fallback identically. It
// differs only for estStats and estHeuristic, neither of which could reach a
// component before the filtered arm existed.
func weakerSource(a, b estSource) estSource {
	if a > b {
		return a
	}
	return b
}

// reorderFilteredRows estimates how many rows a filtered component EMITS, from
// the property statistics (rmp #2766). It is the first plan decision in this
// engine that reads them; until now [statsEqualityEstimate] and
// [statsRangeEstimate] had no consumer outside the EXPLAIN renderer.
//
// labelRows is the component's drain estimate — the exact N(label) — passed in
// rather than re-derived; see [reorderStatsFreshness] for why the caller's copy
// is the one that can be trusted.
//
// An estimate is produced only for the shape [reorderFilteredScan] admitted, with
// THIS execution's parameters bound. An unbound or null operand yields
// estFallback: `n.p = null` and `n.p < null` match nothing under openCypher three-
// valued logic, which is not a row count a distribution statistic describes.
//
// Every verdict except an MCV hit or a fresh histogram range is untrustworthy by
// construction — an equality on a value absent from the MCV list is the 1/NDV
// distribution average (estHeuristic), and an absent statistic is estFallback —
// and [planStaysDefault] then keeps the written order.
func reorderFilteredRows(
	sel *ir.Selection,
	scan *ir.NodeByLabelScan,
	labelSrc labelResolverIface,
	params map[string]expr.Value,
	labelRows estimate,
) (estimate, float64) {
	src, ok := labelSrc.(statsSource)
	if !ok {
		return estimate{source: estFallback}, 0
	}
	if prop, lit, okEq := extractEqFromAST(sel.PredicateExpr, scan.NodeVar, params); okEq {
		if lit == nil || expr.IsNull(lit) {
			return estimate{source: estFallback}, 0
		}
		// An MCV hit is an exact per-value count for the snapshot the statistic was
		// built from, so its certified error is zero and the freshness screen below
		// is the only thing standing between it and a plan decision.
		e := reorderStatsFreshness(src, scan.Label, prop, labelRows,
			statsEqualityEstimate(src, scan.Label, prop, lit))
		return e, 0
	}
	if prop, op, bound, okRange := extractRangeComparison(sel.PredicateExpr, scan.NodeVar, params); okRange {
		if bound == nil || expr.IsNull(bound) {
			return estimate{source: estFallback}, 0
		}
		e, absErr := statsRangeEstimate(src, scan.Label, prop, op, bound)
		e = reorderStatsFreshness(src, scan.Label, prop, labelRows, e)
		if !e.trustworthy() {
			return e, 0
		}
		// absErr is an error on the SELECTIVITY; scale it by the label count to get
		// rows. N over-states the histogram's summarised total (which excludes
		// out-of-domain and NaN values), so the product over-states the row error —
		// the conservative direction.
		return e, absErr * labelRows.rows
	}
	return estimate{source: estFallback}, 0
}

// reorderStatsFreshness demotes a trustworthy statistics estimate to estFallback
// when the statistic behind it is STALE, so the trustworthiness veto keeps the
// written order. It is a planner-side screen applied on top of the estimate
// providers, and it changes neither of them.
//
// It exists because the two providers do not screen alike. [statsRangeEstimate]
// applies the design's staleness rule itself (docs/statistics-design.md §3:
// demote once the accumulated-write term reaches the firing region b - 1/B, or
// once deletes exceed the rebuild tolerance). [statsEqualityEstimate] does NOT: an
// MCV hit returns the recorded per-value count tagged estExact no matter how many
// writes have landed since the snapshot was published. A plan decision must not
// rest on a count that stopped being true, so the screen is applied to BOTH paths
// here, at the one place that consumes them for a decision.
//
// # The denominator, and the ResolveLabelCount finding (rmp #2765, #2392)
//
// The staleness fraction needs a live-node count N for the label, and
// [lpgLabelResolver.ResolveLabelCount] is exact-or-nothing: it declines whenever
// any MVCC history is live, which under a concurrent writer is always.
// [statsRangeEstimateInner] takes it as `n, _ :=`, cannot distinguish that decline
// from a real zero, and its `n <= 0` guard then demotes the whole range estimate.
//
// This screen does not repeat that mistake. Its denominator is the SMALLER of two
// numbers that are always available:
//
//   - the statistic's own build-time label count (stats.Stats.LabelCount), which is
//     recorded in the snapshot and needs no resolver at all; and
//   - the caller's drain estimate, but only when it is estExact —
//     [labelCardinalityEstimate] falls back to the label bitmap's cardinality when
//     the count declines, so it yields an exact live count where ResolveLabelCount
//     yields nothing.
//
// Taking the SMALLER maximises the staleness fraction, which is the conservative
// direction: it demotes sooner, never later. That also answers why an UPPER bound
// (ResolveLabelCountBound, the capability built for rmp #2392) is the WRONG
// instrument here — a bound that over-states N under-states staleness and would
// keep the planner trusting a statistic it should have demoted.
func reorderStatsFreshness(src statsSource, label, prop string, labelRows, e estimate) estimate {
	if !e.trustworthy() {
		return e
	}
	st, ok := lookupStats(src, label, prop)
	if !ok {
		return estimate{rows: e.rows, source: estFallback}
	}
	if st.NeedsRebuildForDeletes(statsDeleteRebuildTol) {
		return estimate{rows: e.rows, source: estFallback}
	}
	n := float64(st.LabelCount())
	if labelRows.source == estExact && labelRows.rows < n {
		n = labelRows.rows
	}
	if n <= 0 {
		return estimate{rows: e.rows, source: estFallback}
	}
	if float64(st.Delta())/n >= statsRangeBreakEven-1.0/float64(statsHistogramBuckets) {
		return estimate{rows: e.rows, source: estFallback}
	}
	return e
}

// reorderSwapWins reports whether exchanging a candidate Apply's arms is a strict
// improvement. It requires BOTH of two rules to admit, and it evaluates them on
// the pessimistic end of each certified interval.
//
// # Why two rules: the realisation this peephole cannot observe
//
// The gate runs without an index manager, so it cannot know which of TWO physical
// realisations a filtered component will get:
//
//   - scan-and-filter, when no index covers the predicate. drain = N(label),
//     rows = the filtered estimate. Rule 2 is the correct rule for this one.
//   - an INDEX SEEK, when the range-seek (#1505) or hash-seek rewrite fires at
//     build time and replaces the Selection outright. The seek touches only the
//     rows it returns, so drain collapses to rows and rule 1 is the correct rule.
//
// A rule that is correct for one realisation is a REGRESSION under the other, in
// both directions:
//
//   - Rule 1 alone, under scan-and-filter: an outer Selection scanning 10^9 rows to
//     emit 5, against an inner bare scan of 4, satisfies 4 < 5 — and the swap costs
//     4 + 4*10^9 against the written order's 10^9 + 5*4.
//   - Rule 2 alone, under a seek: an outer bare scan of 2 rows against an inner
//     Selection emitting 3 from 10^6 satisfies rule 2 — and if the property is
//     indexed the swapped order costs 3 + 3*2 = 9 against the written order's
//     2 + 2*3 = 8.
//
// Requiring both is the minimax choice under that uncertainty: the swap fires only
// when it wins whichever realisation the build produces. It is not free — it
// declines the shape graph-theory-expert identified as the dominant one this
// change introduces (a small bare arm against a large, selective filtered arm,
// where only rule 2 admits). Recovering that case needs the index manager on the
// gate so the realisation is decided rather than bounded; that is a follow-up, and
// declining it keeps the no-regression guarantee intact meanwhile.
//
// # Rule 1 — the emitted-row rule (what the peephole has always applied)
//
//	rows(inner) * joinReorderMargin <= rows(outer)  AND  rows(inner) < rows(outer)
//
// # Rule 2 — the drain-aware nested-loop rule (rmp #2766)
//
//	cost(o, i) = drain(o) + rows(o) * drain(i)
//	swap  iff  cost(inner, outer) is strictly cheaper, under the margin
//
// This is the System R nested-loop cost C_outer + card(outer)*C_inner (Selinger et
// al., SIGMOD 1979) with drain in place of pages and no index term, and it is what
// makes a filtered component worth driving with: its full label scan is then paid
// ONCE rather than once per row of the other arm.
//
// # Why the pair reproduces today's decisions bit for bit
//
// For every component kind that existed before rmp #2766, drain == rows and
// rowsErr == 0, so rowsLo == rowsHi == rows. Write D == R for both arms and the
// rule-2 gain collapses to
//
//	(Ro - Ri) + (Ro*Ri - Ri*Ro)
//
// whose second term is exactly +0.0 in IEEE-754 (multiplication is commutative and
// exactly rounded), so the test is exactly Ri < Ro — rule 1 with the 1.0 margin.
// The margin comparison is implied by the monotonicity of IEEE addition. And the
// stats margin cannot apply, because no pre-existing component kind can carry an
// estStats provenance. So with no filtered component anywhere, this function
// admits exactly the swaps the previous one-number gate admitted. (Above 2^53 the
// cost sums lose exactness, but only by a relative 1e-16, at which point the two
// orders are equally good and a spurious tie keeps the written order.)
func reorderSwapWins(outer, inner componentCost) bool {
	// The interval: the incumbent gets the row count that flatters it, the
	// candidate the one that penalises it, so a swap must win across the whole
	// certified range. Both are the plain row count when nothing is estimated.
	rOut, rIn := outer.rowsLo(), inner.rowsHi()
	// Rule 1, unchanged apart from the interval. Equal cardinalities keep the
	// written order, so a swap never merely reshuffles an even split.
	if !(rIn*joinReorderMargin <= rOut && rIn < rOut) {
		return false
	}
	dOut, dIn := outer.drain.rows, inner.drain.rows
	// Rule 2, as a single difference so the D == R case cancels exactly.
	if gain := (dOut - dIn) + (rOut*dIn - rIn*dOut); !(gain > 0) {
		return false
	}
	margin := joinReorderMargin
	if reorderPathHasStats(outer, inner) {
		margin = joinReorderStatsMargin
	}
	return (dIn+rIn*dOut)*margin <= dOut+rOut*dIn
}

// reorderPathHasStats reports whether any estimate on the decision path is
// histogram-derived rather than a maintained exact count, which is what selects
// [joinReorderStatsMargin] over [joinReorderMargin].
func reorderPathHasStats(outer, inner componentCost) bool {
	for _, e := range [...]estimate{outer.drain, outer.rows, inner.drain, inner.rows} {
		if e.source == estStats {
			return true
		}
	}
	return false
}

// computeReorderSwaps applies the live cardinality gate to the memoised,
// order-safe candidates and returns the set of Apply nodes whose arms should be
// swapped at build time (the cheaper drive order wins). It returns nil when
// nothing qualifies. totalNodes is the exact live node count read once from the
// query's snapshot (for AllNodesScan components), and params is this execution's
// parameter binding, needed to resolve a filtered component's operand.
//
// The gate (design §1): EVERY estimate on the path is trustworthy — both arms'
// drain AND rows (else the trustworthiness veto keeps the written order) — and the
// swap is a strict improvement under [reorderSwapWins].
func computeReorderSwaps(candidates []*ir.Apply, labelSrc labelResolverIface, params map[string]expr.Value, totalNodes int64) map[*ir.Apply]bool {
	if len(candidates) == 0 {
		return nil
	}
	var swaps map[*ir.Apply]bool
	for _, ap := range candidates {
		outer, ok1 := reorderComponentCardinality(ap.Outer, labelSrc, params, totalNodes)
		inner, ok2 := reorderComponentCardinality(ap.Inner, labelSrc, params, totalNodes)
		if !ok1 || !ok2 {
			continue
		}
		// §1 trustworthiness veto: any untrustworthy input keeps the written order.
		if planStaysDefault(outer.drain, outer.rows, inner.drain, inner.rows) {
			continue
		}
		if !reorderSwapWins(outer, inner) {
			continue
		}
		if swaps == nil {
			swaps = make(map[*ir.Apply]bool, len(candidates))
		}
		swaps[ap] = true
	}
	return swaps
}
