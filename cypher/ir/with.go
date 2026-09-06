package ir

import (
	"math"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
)

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
//
// Exception — aggregation: when the projection contains aggregates, the
// WHERE behaves as SQL HAVING and must run AFTER the EagerAggregation
// because it filters aggregated groups (`WITH a, count(*) AS c WHERE
// c > 1`). The aggregate-rewrite pass would substitute c with `count(*)`,
// which has no row-by-row evaluation upstream of the aggregator. In
// that case the Selection is wrapped on top of the projection instead.
func (t *translator) translateWith(w *ast.With, child LogicalPlan) (LogicalPlan, error) {
	_, _, _, hasAgg := detectAggregation(w.Projection)

	if w.Where != nil && !hasAgg {
		// When WITH is the first clause (no preceding reading clause) child is
		// nil — there is no scan leaf yet. Seed a single-row Argument so the
		// WHERE Selection has a valid, non-nil child. The Argument emits exactly
		// one empty row, which is the correct behaviour for a leading WITH: the
		// literal expressions in the projection are evaluated once per that row.
		if child == nil {
			child = NewArgument(nil)
		}
		pred := rewriteWithProjectionAliases(w.Where.Predicate, w.Projection)
		var err error
		child, err = t.translateExistsPredicate(pred, child)
		if err != nil {
			return nil, err
		}
	}

	// Hoist any PatternComprehension into its own RollUpApply layer so the
	// downstream aggregation / projection sees only synthetic __pc_N
	// variables — mirrors the RETURN path. rewrittenProj carries the same
	// substitution so detectAggregation classifies count(__pc_N) correctly.
	planAfterComp, regularItems, rewrittenProj, err := t.projectionsWithComprehensions(w.Projection, child)
	if err != nil {
		return nil, err
	}
	aggProj := w.Projection
	if rewrittenProj != nil {
		aggProj = rewrittenProj
	}

	groupBy, groupByExprs, aggs, _ := detectAggregation(aggProj)

	var plan LogicalPlan
	if hasAgg {
		plan = NewEagerAggregationWithExprs(groupBy, groupByExprs, aggs, planAfterComp)
		// Emit a covering Projection only when there are non-aggregate items that
		// need renaming (alias not equal to the expression string). In practice the
		// EagerAggregation already exposes the correct output names, so the
		// Projection is needed only to preserve ordering and aliasing.
		var items []ProjectionItem
		if len(regularItems) > 0 {
			items = regularItems
		} else {
			items = projectionItems(w.Projection, collectAllVars(planAfterComp))
		}
		// When the projection contains aggregates nested inside larger
		// expressions, rewrite the items so they reference the synthetic
		// __agg_N columns the EagerAggregation emits.
		if rewritten := rewriteProjectionForAggregation(aggProj); rewritten != nil {
			items = rewritten
		}
		// openCypher §3.6.4: ORDER BY on an aggregating WITH can reference
		// either the grouping-key alias or its source expression. Rewrite
		// the ORDER BY items so any subexpression whose String() equals a
		// projection item's source expression is replaced by a Variable
		// referencing the alias. Without this, `WITH a.name AS name,
		// count(*) AS cnt ORDER BY a.name + 'C'` fails because `a` is no
		// longer in scope after the aggregation (WithOrderBy2 [23]).
		rewriteOrderByForAggregation(w.Projection, items)
		if len(items) > 0 {
			plan = NewProjection(items, plan)
		}
	} else {
		var items []ProjectionItem
		if len(regularItems) > 0 {
			items = regularItems
		} else {
			items = projectionItems(w.Projection, collectAllVars(planAfterComp))
		}
		// openCypher allows ORDER BY (and SKIP/LIMIT) on a WITH to
		// reference variables that exist BEFORE this projection but
		// aren't explicitly projected. To preserve them through the
		// projection so the downstream Sort can read them, append
		// "hidden" projection items for any pre-projection variable
		// referenced by ORDER BY but not already in items. The final
		// projection above ProduceResults / the next WITH will discard
		// these extras because it only emits its declared columns.
		// Closes WithOrderBy4 [8] (`WITH a, sum; WITH a, mod ORDER BY
		// sum LIMIT 3` no longer drops `sum`).
		preVars := collectAllVars(planAfterComp)
		items = appendOrderByPassthrough(items, w.Projection, preVars, false)
		plan = NewProjection(items, planAfterComp)
	}

	// DISTINCT, SKIP, ORDER BY, LIMIT — mirror the RETURN translator at
	// translator.go so that `WITH x ORDER BY x LIMIT k` produces an actual
	// Sort+Limit pipeline rather than silently passing the full row stream.
	plan = applyProjectionTail(plan, w.Projection)

	// Aggregation HAVING-style filter: WHERE applies AFTER the
	// EagerAggregation+Projection because it filters aggregated groups
	// rather than pre-aggregation rows.
	if w.Where != nil && hasAgg {
		var err error
		plan, err = t.translateExistsPredicate(w.Where.Predicate, plan)
		if err != nil {
			return nil, err
		}
	}

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
func substVarRefs(e ast.Expression, subst map[string]ast.Expression) ast.Expression { // case-per-AST-node dispatch
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

// rewriteOrderByForAggregation rewrites every ORDER BY expression on the
// projection so any subexpression matching a projection item's source
// expression is replaced by a Variable referencing the item's alias.
// openCypher allows aggregating-WITH ORDER BY to reference the grouping
// expression OR its alias; after EagerAggregation only the alias is in
// scope, so the source-expression form must be rewritten to evaluate
// against the aggregated row.
func rewriteOrderByForAggregation(proj *ast.Projection, items []ProjectionItem) {
	if proj == nil || len(proj.OrderBy) == 0 {
		return
	}
	// Build a map from source-expression string → alias variable.
	exprToAlias := make(map[string]string, len(items))
	for _, it := range items {
		if it.Name == "" || it.Expression == "" || it.Expression == it.Name {
			continue
		}
		exprToAlias[it.Expression] = it.Name
	}
	if len(exprToAlias) == 0 {
		return
	}
	for _, s := range proj.OrderBy {
		if s == nil || s.Expr == nil {
			continue
		}
		s.Expr = substExprByString(s.Expr, exprToAlias)
	}
}

// substExprByString returns a copy of e in which any sub-expression whose
// String() equals a key in subst is replaced by a Variable referencing
// subst[s.String()]. Used by rewriteOrderByForAggregation to translate
// grouping-expression references in ORDER BY items into the alias variable
// they were projected as.
func substExprByString(e ast.Expression, subst map[string]string) ast.Expression { // case-per-AST-node dispatch
	if e == nil {
		return nil
	}
	if alias, ok := subst[e.String()]; ok {
		return &ast.Variable{Name: alias}
	}
	switch n := e.(type) {
	case *ast.BinaryOp:
		left := substExprByString(n.Left, subst)
		right := substExprByString(n.Right, subst)
		if left == n.Left && right == n.Right {
			return n
		}
		cp := *n
		cp.Left = left
		cp.Right = right
		return &cp
	case *ast.UnaryOp:
		op := substExprByString(n.Operand, subst)
		if op == n.Operand {
			return n
		}
		cp := *n
		cp.Operand = op
		return &cp
	case *ast.Property:
		rec := substExprByString(n.Receiver, subst)
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
			newArgs[i] = substExprByString(a, subst)
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
	case *ast.SubscriptExpr:
		expr := substExprByString(n.Expr, subst)
		idx := substExprByString(n.Index, subst)
		if expr == n.Expr && idx == n.Index {
			return n
		}
		cp := *n
		cp.Expr = expr
		cp.Index = idx
		return &cp
	case *ast.SliceExpr:
		ex := substExprByString(n.Expr, subst)
		from := substExprByString(n.From, subst)
		to := substExprByString(n.To, subst)
		if ex == n.Expr && from == n.From && to == n.To {
			return n
		}
		cp := *n
		cp.Expr = ex
		cp.From = from
		cp.To = to
		return &cp
	}
	return e
}

// appendOrderByPassthrough scans proj.OrderBy for Variable references that
// are in scope BEFORE this projection (preVars) but not already among the
// projection items. For each such variable it appends a passthrough
// ProjectionItem so the variable survives the projection's schema reset
// and the downstream Sort can resolve it. Aggregating WITHs and DISTINCT
// projections skip this augmentation because their output schema is
// strictly defined by the aggregation contract / DISTINCT contract.
func appendOrderByPassthrough(items []ProjectionItem, proj *ast.Projection, preVars []string, hoistKeys bool) []ProjectionItem {
	if proj == nil || len(proj.OrderBy) == 0 {
		return items
	}
	if proj.Distinct {
		return items
	}
	preSet := make(map[string]struct{}, len(preVars))
	for _, v := range preVars {
		preSet[v] = struct{}{}
	}
	itemNames := make(map[string]struct{}, len(items))
	for _, it := range items {
		if it.Name != "" {
			itemNames[it.Name] = struct{}{}
		}
		if it.Expression != "" {
			itemNames[it.Expression] = struct{}{}
		}
	}
	// Collect variable names referenced by ORDER BY but not yet in items.
	//
	// HOIST THE KEY INSTEAD OF THE ENTITY (rmp #2662). A key of the shape
	// `var.prop` is projected as its OWN hidden column, so the Sort reads the
	// scalar from a row slot and the entity never has to survive the projection
	// at all. Everything else keeps the entity passthrough below. See
	// [hoistableSortKeyItem] for the conditions and for why they are what they
	// are.
	//
	// hoisted is separate from itemNames on purpose: itemNames is what
	// collectOrderByVars consults to decide whether a VARIABLE still needs a
	// passthrough, and a hoisted key column is not a variable binding. Polluting
	// it would let a hidden key column suppress a passthrough that is genuinely
	// needed, because Cypher admits a backtick-quoted identifier whose spelling
	// is exactly a property path.
	hoisted := make(map[string]struct{}, len(proj.OrderBy))
	var added []string
	for _, s := range proj.OrderBy {
		if s == nil {
			continue
		}
		if it, ok := hoistableSortKeyItem(s.Expr, preSet, itemNames, hoisted, hoistKeys); ok {
			items = append(items, it)
			hoisted[it.Name] = struct{}{}
			continue
		}
		collectOrderByVars(s.Expr, preSet, itemNames, &added)
	}
	for _, v := range added {
		items = append(items, ProjectionItem{
			Name:       v,
			Expression: v,
			Expr:       &ast.Variable{Name: v},
			Hidden:     true, // ORDER-BY passthrough; excluded from output cols (#1805)
		})
		itemNames[v] = struct{}{}
	}
	return items
}

// hoistableSortKeyItem returns the hidden [ProjectionItem] that carries the
// VALUE of sort key e, and whether e may be hoisted into one at all.
//
// # Why (rmp #2662)
//
// The entity passthrough [appendOrderByPassthrough] adds is what forces the
// projection to ship the whole node. `MATCH (p:Person) RETURN p.firstName ORDER
// BY p.salary` projects a hidden `p`, and the projection's node-variable fast
// path turns that column into a full [expr.NodeValue] — labels and the ENTIRE
// property bag — once per row, purely so the sort-key evaluator above it can
// read one integer back out. Measured on the 120 000-node audit fixture at
// MemProfileRate=1, that path was 840 001 allocated objects, 7.00 per row, on a
// query that ships ten.
//
// Projecting the key itself removes the reason to carry the entity: the Sort
// resolves `p.salary` by direct schema lookup (case 1 of the physical builder's
// irSortKeys), so there is no per-row key evaluation either. It is the shape
// PostgreSQL has always planned — a non-output ORDER BY expression becomes a
// resjunk target-list entry, computed once per row with the rest of the
// projection and stripped before the rows reach the client — and the shape
// Neo4j's slotted runtime depends on, its comparators reading an ordered slot
// rather than evaluating an expression.
//
// # Why ONLY `var.prop`
//
// The hoist moves an evaluation from the sort operator into the projection, and
// the two do not treat failure alike: [exec.sortKeyValue] maps an evaluator
// error to NULL, whereas a projection item's error fails the query. Restricting
// the hoist to a property read off a bare variable keeps the change to the shape
// the defect is about, and every remaining shape keeps the entity passthrough
// and the behaviour it had. The residual error difference this shape can still
// produce — a property read on a non-entity, e.g. `UNWIND ['a'] AS s RETURN 1 AS
// x ORDER BY s.foo` — is neutralised in the physical builder, which gives every
// Hidden item the same error-to-NULL contract sortKeyValue applies.
//
// # The conditions
//
//   - e is `variable.key` with a BARE variable receiver. A receiver that is
//     itself an expression may not be row-local in the pre-projection scope.
//   - The receiver is in scope BEFORE this projection (preSet). Otherwise it is
//     an alias, which the post-projection row already carries and which
//     irSortKeys resolves by name today.
//   - The receiver is NOT one of the projection's own output names. `RETURN
//     p.firstName AS p ORDER BY p.salary` reads `p.salary` off the ALIAS, and
//     hoisting would silently re-point it at the pre-projection node.
//   - The rendered name collides with nothing: not with an item name or
//     expression string (the post-projection schema reset would let the later
//     entry win and re-point a real output column), and not with a
//     pre-projection variable that a passthrough may still add.
func hoistableSortKeyItem(e ast.Expression, preSet, itemNames, hoisted map[string]struct{}, hoistKeys bool) (ProjectionItem, bool) {
	if !hoistKeys {
		return ProjectionItem{}, false
	}
	if sortseam.KeyHoistDisabled() {
		// The differential arm: keep the entity passthrough so a test can compare
		// the two plans on ONE query in ONE binary. Read at translate time, so the
		// arm is fixed when the plan is cached; see [sortseam.SetKeyHoistDisabled].
		return ProjectionItem{}, false
	}
	prop, ok := e.(*ast.Property)
	if !ok {
		return ProjectionItem{}, false
	}
	recv, ok := prop.Receiver.(*ast.Variable)
	if !ok {
		return ProjectionItem{}, false
	}
	if _, inPre := preSet[recv.Name]; !inPre {
		return ProjectionItem{}, false
	}
	if _, shadowed := itemNames[recv.Name]; shadowed {
		return ProjectionItem{}, false
	}
	// The name is the SortItem.Expression the physical builder will look up:
	// applyProjectionTail stores s.Expr.String() and the projection's schema
	// reset registers ProjectionItem.Name, so the two must be the same string.
	name := prop.String()
	if _, taken := itemNames[name]; taken {
		return ProjectionItem{}, false
	}
	if _, taken := preSet[name]; taken {
		return ProjectionItem{}, false
	}
	if _, dup := hoisted[name]; dup {
		// A second ORDER BY term on the same key (`ORDER BY p.x, p.x DESC`)
		// reads the column the first one already projected.
		return ProjectionItem{}, false
	}
	return ProjectionItem{
		Expr:       prop,
		Name:       name,
		Expression: name,
		Hidden:     true, // ORDER-BY key column; excluded from output cols (#1805)
	}, true
}

// collectOrderByVars walks the expression and appends to *out every variable
// name that exists in preSet but not in itemNames, deduplicated. Helper for
// appendOrderByPassthrough.
func collectOrderByVars(e ast.Expression, preSet, itemNames map[string]struct{}, out *[]string) {
	if e == nil {
		return
	}
	switch n := e.(type) {
	case *ast.Variable:
		if _, inPre := preSet[n.Name]; !inPre {
			return
		}
		if _, exists := itemNames[n.Name]; exists {
			return
		}
		// Dedupe.
		for _, existing := range *out {
			if existing == n.Name {
				return
			}
		}
		*out = append(*out, n.Name)
	case *ast.Property:
		collectOrderByVars(n.Receiver, preSet, itemNames, out)
	case *ast.LabelPredicate:
		collectOrderByVars(n.Receiver, preSet, itemNames, out)
	case *ast.BinaryOp:
		collectOrderByVars(n.Left, preSet, itemNames, out)
		collectOrderByVars(n.Right, preSet, itemNames, out)
	case *ast.UnaryOp:
		collectOrderByVars(n.Operand, preSet, itemNames, out)
	case *ast.FunctionInvocation:
		for _, a := range n.Args {
			collectOrderByVars(a, preSet, itemNames, out)
		}
	case *ast.SubscriptExpr:
		collectOrderByVars(n.Expr, preSet, itemNames, out)
		collectOrderByVars(n.Index, preSet, itemNames, out)
	case *ast.SliceExpr:
		collectOrderByVars(n.Expr, preSet, itemNames, out)
		collectOrderByVars(n.From, preSet, itemNames, out)
		collectOrderByVars(n.To, preSet, itemNames, out)
	case *ast.CaseExpression:
		collectOrderByVars(n.Subject, preSet, itemNames, out)
		for _, alt := range n.Alternatives {
			collectOrderByVars(alt.Condition, preSet, itemNames, out)
			collectOrderByVars(alt.Consequent, preSet, itemNames, out)
		}
		collectOrderByVars(n.ElseExpr, preSet, itemNames, out)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			collectOrderByVars(el, preSet, itemNames, out)
		}
	case *ast.MapLiteral:
		for _, v := range n.Values {
			collectOrderByVars(v, preSet, itemNames, out)
		}
	}
}

// applyProjectionTail wraps plan with the DISTINCT / ORDER BY / SKIP /
// LIMIT operators declared on proj. The canonical openCypher evaluation
// order is DISTINCT → ORDER BY → SKIP → LIMIT, so the plan tree is built
// from the inside out in exactly that order.
//
// # The fusion (#2509)
//
// ORDER BY … SKIP s LIMIT k plans as Skip(s) over Top(s+k): the bounded operator
// retains only the rows the page can possibly need, and the Skip above it
// discards the leading s. That is Neo4j's shape
// (LogicalPlanProducer.planSkipAndLimit), chosen over folding the offset INTO the
// operator because it keeps EXPLAIN showing both clauses as operators.
//
// It fuses for LITERAL and PARAMETERISED bounds alike. The literal-only
// restriction that preceded this was backwards: StripLiterals clears the
// hoistable flag at SKIP/LIMIT, so LIMIT 10/20/30 occupy three plan-cache entries
// while LIMIT $m occupies one — the spelling the engine's own documentation
// recommends was exactly the spelling that lost the fusion.
//
// Two conditions refuse the fusion, both of them soundness rather than cost:
//
//   - a NON-DETERMINISTIC bound expression. The fused shape evaluates s twice —
//     once inside the Top bound and once in the Skip above it — so a SKIP
//     toInteger(rand()*10) would skip a different number of rows than it
//     reserved. [ExprIsDeterministic] is the gate.
//   - s+k OVERFLOWING int64 for literal bounds. Saturating would silently drop
//     the LIMIT, and Neo4j's Math.addExact would THROW where this engine's
//     pinned contract (internal/sim/surface_order.go, "SKIP past the end") says a
//     SKIP beyond the input returns zero rows and no error. Refusing to fuse
//     leaves Sort → Skip → Limit, which returns those zero rows.
//
// There is deliberately NO plan-time crossover guard on n against the input
// cardinality: neither reference engine attempts one, and this function has no
// statistics to attempt it with. [exec.Top] decides it at run time by
// accumulating to 2n before it builds a heap, as PostgreSQL does.
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
		if bound, ok := fusibleTopBound(proj); ok {
			plan = NewTopWithBound(sortItems, bound, plan)
			// The Skip is still emitted, above the Top, so the page starts
			// where the caller asked. The Limit is NOT: the Top bound already
			// carries it.
			if proj.Skip != nil {
				plan = newSkipFrom(proj.Skip, plan)
			}
			return plan
		}
		plan = NewSort(sortItems, plan)
	}
	if proj.Skip != nil {
		plan = newSkipFrom(proj.Skip, plan)
	}
	// LIMIT alone (no ORDER BY), or a LIMIT whose fusion was refused above.
	if proj.Limit != nil {
		plan = newLimitFrom(proj.Limit, plan)
	}
	return plan
}

// newSkipFrom builds a Skip from a SKIP expression, using the literal form when
// the expression is a literal integer and deferring resolution otherwise.
func newSkipFrom(e ast.Expression, child LogicalPlan) LogicalPlan {
	if v, err := intExpr(e); err == nil {
		return NewSkip(v, child)
	}
	// A parameter or another expression: defer resolution to the physical
	// builder so a float-typed parameter surfaces as the documented
	// InvalidArgumentType at runtime rather than at plan time.
	return NewSkipExpr(e, child)
}

// newLimitFrom is [newSkipFrom] for a LIMIT expression.
func newLimitFrom(e ast.Expression, child LogicalPlan) LogicalPlan {
	if v, err := intExpr(e); err == nil {
		return NewLimit(v, child)
	}
	return NewLimitExpr(e, child)
}

// fusibleTopBound reports the fused Top bound for proj's SKIP/LIMIT pair, and
// whether the fusion may be applied at all. It is called only when proj carries
// an ORDER BY.
func fusibleTopBound(proj *ast.Projection) (TopBound, bool) {
	if proj.Limit == nil {
		return TopBound{}, false // ORDER BY with no LIMIT is an unbounded Sort
	}
	if !ExprIsDeterministic(proj.Limit) || !ExprIsDeterministic(proj.Skip) {
		return TopBound{}, false
	}

	var b TopBound
	limLit, limIsLit := int64(0), false
	if v, err := intExpr(proj.Limit); err == nil {
		b.Limit, limLit, limIsLit = v, v, true
	} else {
		b.LimitExpr = proj.Limit
	}
	if proj.Skip == nil {
		return b, true
	}
	if v, err := intExpr(proj.Skip); err == nil {
		b.Offset = v
		if limIsLit && v > math.MaxInt64-limLit {
			// s+k does not fit. See the doc comment: fall back to
			// Sort → Skip → Limit rather than saturate or throw.
			return TopBound{}, false
		}
		return b, true
	}
	b.OffsetExpr = proj.Skip
	return b, true
}
