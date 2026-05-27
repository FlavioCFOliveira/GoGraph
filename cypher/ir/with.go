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
//
// The WHERE clause on a WITH is applied to the pre-WITH scope: openCypher
// 9 §5.1.5 specifies that the predicate sees both the variables bound by
// the preceding query part and any new aliases introduced by the
// projection (the aliases are equivalent to their source expressions).
// Implementation: filter the child plan first with the WHERE predicate,
// substituting any reference to a new projection alias with the alias's
// source expression. The aggregation and projection then run over the
// already-filtered rows. The DISTINCT / ORDER BY / SKIP / LIMIT tail
// applies AFTER the projection because openCypher evaluates them on the
// projected stream.
func (t *translator) translateWith(w *ast.With, child LogicalPlan) (LogicalPlan, error) {
	if w.Where != nil {
		pred := rewriteWithProjectionAliases(w.Where.Predicate, w.Projection)
		var err error
		child, err = t.translateExistsPredicate(pred, child)
		if err != nil {
			return nil, err
		}
	}

	groupBy, groupByExprs, aggs, hasAgg := detectAggregation(w.Projection)

	var plan LogicalPlan
	if hasAgg {
		plan = NewEagerAggregationWithExprs(groupBy, groupByExprs, aggs, child)
		// Emit a covering Projection only when there are non-aggregate items that
		// need renaming (alias not equal to the expression string). In practice the
		// EagerAggregation already exposes the correct output names, so the
		// Projection is needed only to preserve ordering and aliasing.
		items := projectionItems(w.Projection, collectAllVars(child))
		if len(items) > 0 {
			plan = NewProjection(items, plan)
		}
	} else {
		items := projectionItems(w.Projection, collectAllVars(child))
		plan = NewProjection(items, child)
	}

	// DISTINCT, SKIP, ORDER BY, LIMIT — mirror the RETURN translator at
	// translator.go so that `WITH x ORDER BY x LIMIT k` produces an actual
	// Sort+Limit pipeline rather than silently passing the full row stream.
	plan = applyProjectionTail(plan, w.Projection)

	return plan, nil
}

// rewriteWithProjectionAliases returns a copy of pred in which every
// *ast.Variable reference whose name matches a projection alias introduced
// by proj is replaced with the alias's source expression. This lets a
// WHERE-on-WITH predicate that uses a new alias (`WITH x.foo AS y …
// WHERE y > 0`) be applied to the pre-projection row stream where x is
// in scope and y is not yet defined.
//
// Aliases are detected as ProjectionItem entries whose .Alias is non-nil
// and whose .Expr is not itself a bare ast.Variable with the same name
// (a self-alias `WITH x AS x` is a no-op and ignored).
func rewriteWithProjectionAliases(pred ast.Expression, proj *ast.Projection) ast.Expression {
	if pred == nil || proj == nil {
		return pred
	}
	aliasMap := make(map[string]ast.Expression, len(proj.Items))
	for _, it := range proj.Items {
		if it == nil || it.Alias == nil || it.Expr == nil {
			continue
		}
		if v, isVar := it.Expr.(*ast.Variable); isVar && v.Name == *it.Alias {
			continue
		}
		aliasMap[*it.Alias] = it.Expr
	}
	if len(aliasMap) == 0 {
		return pred
	}
	return substVarRefs(pred, aliasMap)
}

// substVarRefs returns a copy of e in which any *ast.Variable whose name
// is a key in subst is replaced with the mapped expression. Non-leaf
// nodes are reconstructed only along the path that contains a substitution
// so unrelated subtrees keep their original pointers.
func substVarRefs(e ast.Expression, subst map[string]ast.Expression) ast.Expression { //nolint:gocyclo // case-per-AST-node dispatch
	if e == nil {
		return nil
	}
	switch n := e.(type) {
	case *ast.Variable:
		if repl, ok := subst[n.Name]; ok {
			return repl
		}
		return n
	case *ast.BinaryOp:
		left := substVarRefs(n.Left, subst)
		right := substVarRefs(n.Right, subst)
		if left == n.Left && right == n.Right {
			return n
		}
		cp := *n
		cp.Left = left
		cp.Right = right
		return &cp
	case *ast.UnaryOp:
		op := substVarRefs(n.Operand, subst)
		if op == n.Operand {
			return n
		}
		cp := *n
		cp.Operand = op
		return &cp
	case *ast.Property:
		rec := substVarRefs(n.Receiver, subst)
		if rec == n.Receiver {
			return n
		}
		cp := *n
		cp.Receiver = rec
		return &cp
	case *ast.FunctionInvocation:
		var changed bool
		newArgs := make([]ast.Expression, len(n.Args))
		for i, a := range n.Args {
			newArgs[i] = substVarRefs(a, subst)
			if newArgs[i] != a {
				changed = true
			}
		}
		if !changed {
			return n
		}
		cp := *n
		cp.Args = newArgs
		return &cp
	case *ast.LabelPredicate:
		rec := substVarRefs(n.Receiver, subst)
		if rec == n.Receiver {
			return n
		}
		cp := *n
		cp.Receiver = rec
		return &cp
	}
	return e
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
