package ir

import "gograph/cypher/ast"

// with.go — WITH pipeline-boundary translation.
//
// A WITH clause introduces a scope boundary: variables not projected through
// the WITH are dropped from the downstream plan. The translation strategy is:
//
//  1. Inspect the projection list for aggregate function calls.
//  2. If aggregates are present, emit EagerAggregation(groupBy, aggs, child)
//     followed by a Projection for any remaining items (currently all items are
//     captured either as groupBy keys or as aggregate output names, so the
//     downstream Projection is omitted when there are no non-aggregate
//     pass-through items).
//  3. If no aggregates, emit a plain Projection.
//  4. A WHERE predicate on WITH wraps the result in a Selection.
//
// translateWith is called from translator.go.

// translateWith converts a WITH clause into a logical plan subtree.
func (t *translator) translateWith(w *ast.With, child LogicalPlan) (LogicalPlan, error) {
	groupBy, groupByExprs, aggs, hasAgg := detectAggregation(w.Projection)

	var plan LogicalPlan
	if hasAgg {
		plan = NewEagerAggregationWithExprs(groupBy, groupByExprs, aggs, child)
		// Emit a covering Projection only when there are non-aggregate items that
		// need renaming (alias not equal to the expression string). In practice the
		// EagerAggregation already exposes the correct output names, so the
		// Projection is needed only to preserve ordering and aliasing.
		items := projectionItems(w.Projection)
		if len(items) > 0 {
			plan = NewProjection(items, plan)
		}
	} else {
		items := projectionItems(w.Projection)
		plan = NewProjection(items, child)
	}

	// DISTINCT, SKIP, ORDER BY, LIMIT — mirror the RETURN translator at
	// translator.go so that `WITH x ORDER BY x LIMIT k` produces an actual
	// Sort+Limit pipeline rather than silently passing the full row stream.
	plan = applyProjectionTail(plan, w.Projection)

	if w.Where != nil {
		var err error
		plan, err = t.translateExistsPredicate(w.Where.Predicate, plan)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// applyProjectionTail wraps plan with the DISTINCT / ORDER BY / SKIP /
// LIMIT operators declared on proj. The canonical openCypher evaluation
// order is DISTINCT → ORDER BY → SKIP → LIMIT, so the plan tree is built
// from the inside out in exactly that order. ORDER BY fuses with LIMIT
// into Top only when SKIP is absent — when SKIP and LIMIT both appear,
// the Sort produces the full ordered stream and Skip/Limit operate on
// it independently.
func applyProjectionTail(plan LogicalPlan, proj *ast.Projection) LogicalPlan {
	if proj == nil {
		return plan
	}
	if proj.Distinct {
		plan = NewDistinct(plan)
	}
	if len(proj.OrderBy) > 0 {
		sortItems := make([]SortItem, len(proj.OrderBy))
		for i, s := range proj.OrderBy {
			sortItems[i] = SortItem{Expression: s.Expr.String(), Expr: s.Expr, Descending: s.Descending}
		}
		// Fuse Sort+Limit into Top only when no SKIP is present; with a
		// SKIP, Top would discard rows that the Skip should reveal.
		if proj.Limit != nil && proj.Skip == nil {
			if lim, err := intExpr(proj.Limit); err == nil {
				plan = NewTop(sortItems, lim, plan)
			} else {
				plan = NewSort(sortItems, plan)
				plan = NewLimit(0, plan)
			}
		} else {
			plan = NewSort(sortItems, plan)
		}
	}
	if proj.Skip != nil {
		sk, _ := intExpr(proj.Skip)
		plan = NewSkip(sk, plan)
	}
	// LIMIT alone (no ORDER BY) or LIMIT alongside SKIP needs an explicit
	// Limit wrapper. When ORDER BY+LIMIT fused into Top above, proj.Skip
	// is nil so we don't reach this branch.
	if proj.Limit != nil && (len(proj.OrderBy) == 0 || proj.Skip != nil) {
		lim, _ := intExpr(proj.Limit)
		plan = NewLimit(lim, plan)
	}
	return plan
}
