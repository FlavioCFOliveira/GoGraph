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
// N(label) rows) or [ir.AllNodesScan] (exactly the live node total) — or a
// plain Apply of such components (a nested Cartesian, whose exact cardinality is
// the product). Restricting to bare scans keeps every cost input an EXACT
// count-store quantity and keeps the swap reverse-hazard-free: no expand, no
// filter, no edge/path metadata to relocate. A relationship scan is not a leaf
// operator in this engine (`()-[r]->()` lowers to a scan + Expand), so an
// E(relType)-costed component never arises here.
//
// # The admissibility gate (design §1)
//
// A swap is admitted only when ALL hold, read from ONE count-store snapshot
// (the read-path build resolves them against the query's pinned snapshot, so the
// live node total and every label count are consistent with the query's graph):
//
//   - Both components' cardinalities are EstExact (node counts always are for
//     the live resolver; a nil/opaque resolver yields EstFallback). The
//     trustworthiness veto [planStaysDefault] keeps the written order otherwise.
//   - The candidate is order-safe: SuppressReorder(spine) == false (design §4).
//   - The swap is a strict improvement under the cost margin: the inner (would-be
//     driver) is strictly smaller than the outer. The Cartesian's product term is
//     symmetric, so the driver-scan term is the only differentiator; swapping to
//     the smaller driver is provably no worse and, for a re-executing nested loop,
//     strictly cheaper.
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

// isReorderComponent reports whether plan is a qualifying disjoint-reorder
// component: a bare node scan, or a plain Apply composing two such components
// with disjoint variables (a nested Cartesian). It is purely structural — no
// counts — so it is evaluated once at parse time.
func isReorderComponent(plan ir.LogicalPlan) bool {
	switch n := plan.(type) {
	case *ir.NodeByLabelScan, *ir.AllNodesScan:
		return true
	case *ir.Apply:
		return isReorderComponent(n.Outer) && isReorderComponent(n.Inner) &&
			reorderVarsDisjoint(n.Outer, n.Inner)
	default:
		return false
	}
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

// reorderComponentCardinality returns the EXACT row cardinality of a qualifying
// component as a provenance-tagged estimate, or (zero, false) when plan is not a
// component. A NodeByLabelScan is N(label) (estExact via the label index); an
// AllNodesScan is the live node total passed in (estExact); a nested Apply is
// the product of its arms, estExact iff both arms are. A nil/opaque label
// resolver makes labelCardinalityEstimate return estFallback, which the caller's
// [planStaysDefault] veto rejects.
func reorderComponentCardinality(plan ir.LogicalPlan, labelSrc labelResolverIface, totalNodes int64) (estimate, bool) {
	switch n := plan.(type) {
	case *ir.NodeByLabelScan:
		return labelCardinalityEstimate(labelSrc, n.Label), true
	case *ir.AllNodesScan:
		return estimate{rows: float64(totalNodes), source: estExact}, true
	case *ir.Apply:
		lo, ok1 := reorderComponentCardinality(n.Outer, labelSrc, totalNodes)
		hi, ok2 := reorderComponentCardinality(n.Inner, labelSrc, totalNodes)
		if !ok1 || !ok2 {
			return estimate{}, false
		}
		src := estExact
		if lo.source != estExact || hi.source != estExact {
			src = estFallback
		}
		return estimate{rows: lo.rows * hi.rows, source: src}, true
	default:
		return estimate{}, false
	}
}

// computeReorderSwaps applies the live cardinality gate to the memoised,
// order-safe candidates and returns the set of Apply nodes whose arms should be
// swapped at build time (smaller component drives). It returns nil when nothing
// qualifies. totalNodes is the exact live node count read once from the query's
// snapshot (for AllNodesScan components).
//
// The gate (design §1): both arms EstExact (else the trustworthiness veto keeps
// the written order), and a strict cost improvement under [joinReorderMargin].
func computeReorderSwaps(candidates []*ir.Apply, labelSrc labelResolverIface, totalNodes int64) map[*ir.Apply]bool {
	if len(candidates) == 0 {
		return nil
	}
	var swaps map[*ir.Apply]bool
	for _, ap := range candidates {
		estOuter, ok1 := reorderComponentCardinality(ap.Outer, labelSrc, totalNodes)
		estInner, ok2 := reorderComponentCardinality(ap.Inner, labelSrc, totalNodes)
		if !ok1 || !ok2 {
			continue
		}
		// §1 trustworthiness veto: any non-exact input keeps the written order.
		if planStaysDefault(estOuter, estInner) {
			continue
		}
		// Swap only on a strict improvement (smaller drives → fewer inner
		// re-inits), under the margin guard. Equal cardinalities keep the written
		// order, so a swap never merely reshuffles an even split.
		if estInner.rows*joinReorderMargin <= estOuter.rows && estInner.rows < estOuter.rows {
			if swaps == nil {
				swaps = make(map[*ir.Apply]bool, len(candidates))
			}
			swaps[ap] = true
		}
	}
	return swaps
}
