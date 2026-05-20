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
	groupBy, aggs, hasAgg := detectAggregation(w.Projection)

	var plan LogicalPlan
	if hasAgg {
		plan = NewEagerAggregation(groupBy, aggs, child)
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

	if w.Where != nil {
		var err error
		plan, err = t.translateExistsPredicate(w.Where.Predicate, plan)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}
