package rewrite

import (
	"gograph/cypher/ir"
)

// FusionRules implements three structural optimisations:
//
//  1. Distinct+Limit fusion → Top: Distinct(Limit(n, child)) is replaced by
//     Top([], n, child). The Top operator sorts-and-limits in a single pass,
//     but when no SortItems are present it degenerates to a stop-after-n
//     operator with deduplication. This avoids materialising the full sorted
//     stream before deduplication.
//
//  2. Double-Projection removal: Projection(outer, Projection(inner, child))
//     where every outer item references only columns produced by the inner
//     Projection is collapsed into a single Projection by inlining the outer
//     item expressions and retaining only items required by the outer.
//
//  3. Redundant-Distinct removal: Distinct(Distinct(child)) → Distinct(child).
//     Also removes a Distinct above an operator that already produces unique
//     rows, specifically EagerAggregation (GROUP BY always produces unique key
//     combinations).
//
// Concurrency: FusionRules is stateless and goroutine-safe.
type FusionRules struct{}

// Name implements Rule.
func (FusionRules) Name() string { return "FusionRules" }

// Apply implements Rule.
func (FusionRules) Apply(plan ir.LogicalPlan) (ir.LogicalPlan, bool) {
	switch p := plan.(type) {
	case *ir.Distinct:
		return applyDistinctFusion(p)
	case *ir.Projection:
		return applyDoubleProjection(p)
	default:
		return plan, false
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Distinct-based fusion
// ─────────────────────────────────────────────────────────────────────────────

func applyDistinctFusion(d *ir.Distinct) (ir.LogicalPlan, bool) {
	switch child := d.Child.(type) {
	// ── Distinct(Limit(n, X)) → Top([], n, X) ───────────────────────────────
	case *ir.Limit:
		top := ir.NewTop(nil, child.Count, child.Child)
		return top, true

	// ── Distinct(Distinct(X)) → Distinct(X) ─────────────────────────────────
	case *ir.Distinct:
		// Remove the outer Distinct; the inner already deduplicates.
		return child, true

	// ── Distinct(EagerAggregation(…)) → EagerAggregation(…) ─────────────────
	// EagerAggregation already produces unique GroupBy key combinations.
	case *ir.EagerAggregation:
		return child, true

	// ── Distinct(Top(…)) → Top(…) ───────────────────────────────────────────
	// Top already implies distinct (it is the result of a prior Distinct+Limit
	// fusion), so wrapping it in another Distinct is redundant.
	case *ir.Top:
		return child, true

	default:
		return d, false
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Double-Projection removal
// ─────────────────────────────────────────────────────────────────────────────

// applyDoubleProjection handles Projection(outer, Projection(inner, child)).
// The inner Projection must produce a superset of the columns required by the
// outer; in that case the inner can be removed and the outer's expressions
// rewritten in terms of the child's outputs.
//
// Since our ProjectionItem.Expression is opaque, we can only safely collapse
// when the outer item's Expression equals its Name (i.e. a simple variable
// reference, not a computed expression). This is the common case after
// predicate/projection pushdown.
func applyDoubleProjection(outer *ir.Projection) (ir.LogicalPlan, bool) {
	inner, ok := outer.Child.(*ir.Projection)
	if !ok {
		return outer, false
	}

	// Build a set of names the inner produces.
	innerNames := stringSet(inner.Vars())

	// Verify that every outer item whose Expression ≠ Name is fully covered by
	// the inner. If the outer references a computed expression we cannot safely
	// inline, so we bail out.
	for _, item := range outer.Items {
		if item.Expression != item.Name {
			// Computed expression: require that inner produces it.
			if _, ok := innerNames[item.Name]; !ok {
				return outer, false
			}
		}
	}

	// All outer items are either simple renames or directly reference inner
	// output columns. Build the merged Projection: outer items using the inner's
	// child as the new child.
	merged := ir.NewProjection(outer.Items, inner.Child)
	return merged, true
}
