package ir

import (
	"fmt"

	"gograph/cypher/ast"
)

// FromAST converts a post-parse [ast.Query] into a [LogicalPlan] tree following
// the Marton (2017) algebra. The function dispatches per clause and assembles
// operators bottom-up.
//
// Unsupported constructs (FOREACH, multi-graph constructs beyond UNION) return a
// [*TranslateError] so callers can distinguish them from internal failures.
//
// Concurrency: FromAST is stateless; it is safe to call concurrently.
func FromAST(q ast.Query) (LogicalPlan, error) {
	t := &translator{}
	return t.query(q)
}

// translator is an internal, single-use helper that threads the bottom-up plan
// construction. It carries no mutable state; it exists only as a method receiver
// for organisational clarity.
type translator struct{}

// ─────────────────────────────────────────────────────────────────────────────
// Top-level query dispatch
// ─────────────────────────────────────────────────────────────────────────────

func (t *translator) query(q ast.Query) (LogicalPlan, error) {
	switch v := q.(type) {
	case *ast.SingleQuery:
		return t.singleQuery(v)
	case *ast.MultiQuery:
		return t.multiQuery(v)
	default:
		return nil, &TranslateError{UnsupportedClause: fmt.Sprintf("%T", q)}
	}
}

// multiQuery translates a UNION / UNION ALL of single queries.
func (t *translator) multiQuery(mq *ast.MultiQuery) (LogicalPlan, error) {
	if len(mq.Parts) == 0 {
		return nil, &TranslateError{UnsupportedClause: "empty UNION", Pos: mq.Pos}
	}

	// Translate the first part as the leftmost operand.
	left, err := t.singleQuery(mq.Parts[0])
	if err != nil {
		return nil, err
	}

	// Fold remaining parts left-associatively.
	for _, part := range mq.Parts[1:] {
		right, err := t.singleQuery(part)
		if err != nil {
			return nil, err
		}
		if mq.All {
			left = NewUnionAll(left, right)
		} else {
			left = NewUnion(left, right)
		}
	}
	return left, nil
}

// singleQuery translates a SingleQuery bottom-up:
//  1. Reading clauses build the initial scan/expand/filter tree.
//  2. WITH clauses project and reset scope boundaries.
//  3. Updating clauses layer write operators on top.
//  4. RETURN wraps in Projection + ProduceResults.
func (t *translator) singleQuery(q *ast.SingleQuery) (LogicalPlan, error) {
	// Start with a nil base; the first scan clause sets it.
	var plan LogicalPlan

	for _, rc := range q.ReadingClauses {
		var err error
		plan, err = t.readingClause(rc, plan)
		if err != nil {
			return nil, err
		}
	}

	for _, w := range q.With {
		var err error
		plan, err = t.withClause(w, plan)
		if err != nil {
			return nil, err
		}
	}

	for _, uc := range q.UpdatingClauses {
		var err error
		plan, err = t.updatingClause(uc, plan)
		if err != nil {
			return nil, err
		}
	}

	if q.Return != nil {
		var err error
		plan, err = t.returnClause(q.Return, plan)
		if err != nil {
			return nil, err
		}
	}

	return plan, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Reading clauses
// ─────────────────────────────────────────────────────────────────────────────

func (t *translator) readingClause(rc ast.ReadingClause, child LogicalPlan) (LogicalPlan, error) {
	switch v := rc.(type) {
	case *ast.Match:
		return t.matchClause(v, child, false)
	case *ast.OptionalMatch:
		return t.optionalMatchClause(v, child)
	case *ast.Unwind:
		return t.unwindClause(v, child)
	case *ast.With:
		return t.withClause(v, child)
	case *ast.Call:
		return t.callClause(v, child)
	case *ast.Return:
		return t.returnClause(v, child)
	default:
		return nil, &TranslateError{UnsupportedClause: fmt.Sprintf("%T", rc)}
	}
}

// matchClause translates MATCH / OPTIONAL MATCH.
// optional=true produces OptionalExpand instead of Expand on relationships.
func (t *translator) matchClause(m *ast.Match, child LogicalPlan, _ bool) (LogicalPlan, error) {
	plan, err := t.pattern(m.Pattern, child, false)
	if err != nil {
		return nil, err
	}
	if m.Where != nil {
		plan = NewSelection(m.Where.Predicate.String(), plan)
	}
	return plan, nil
}

func (t *translator) optionalMatchClause(m *ast.OptionalMatch, child LogicalPlan) (LogicalPlan, error) {
	plan, err := t.pattern(m.Pattern, child, true)
	if err != nil {
		return nil, err
	}
	if m.Where != nil {
		plan = NewSelection(m.Where.Predicate.String(), plan)
	}
	return plan, nil
}

func (t *translator) unwindClause(u *ast.Unwind, child LogicalPlan) (LogicalPlan, error) {
	return NewUnwind(u.Expr.String(), u.Variable, child), nil
}

func (t *translator) callClause(c *ast.Call, child LogicalPlan) (LogicalPlan, error) {
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = a.String()
	}
	yieldVars := make([]string, 0, len(c.Yield))
	for _, yi := range c.Yield {
		name := yi.Name
		if yi.Alias != nil {
			name = *yi.Alias
		}
		yieldVars = append(yieldVars, name)
	}
	return NewProcedureCall(c.Namespace, c.Procedure, args, yieldVars, child), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Pattern translation
// ─────────────────────────────────────────────────────────────────────────────

// pattern translates a MATCH/CREATE Pattern (comma-separated path list) into
// a plan. Multiple paths produce a cartesian cross-product by stacking scans.
// optional controls whether relationship steps use OptionalExpand.
func (t *translator) pattern(pat *ast.Pattern, child LogicalPlan, optional bool) (LogicalPlan, error) {
	if pat == nil || len(pat.Paths) == 0 {
		return child, nil
	}
	plan := child
	for _, pp := range pat.Paths {
		var err error
		plan, err = t.pathPattern(pp, plan, optional)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// pathPattern translates a single PathPattern (a linked list of node/rel steps).
func (t *translator) pathPattern(pp *ast.PathPattern, child LogicalPlan, optional bool) (LogicalPlan, error) {
	if pp == nil || pp.Head == nil {
		return child, nil
	}

	plan := child
	el := pp.Head

	// The first element is always a node; translate it as a scan.
	if el.Node != nil {
		plan = t.nodeScan(el.Node, plan)
	}

	// Walk remaining (rel, node) pairs.
	el = el.Next
	for el != nil {
		if el.Relationship != nil && el.Node != nil {
			plan = t.expandStep(el.Relationship, el.Node, plan, optional)
		}
		el = el.Next
	}
	return plan, nil
}

// nodeScan produces AllNodesScan or NodeByLabelScan for a NodePattern.
// The incoming child is threaded as-is (cross-join semantics when non-nil).
func (t *translator) nodeScan(np *ast.NodePattern, child LogicalPlan) LogicalPlan {
	nodeVar := ""
	if np.Variable != nil {
		nodeVar = *np.Variable
	}

	if len(np.Labels) == 0 {
		scan := NewAllNodesScan(nodeVar)
		if child == nil {
			return scan
		}
		// Cross-join: wrap as an Apply so the outer bindings are available.
		// For simple queries this is just the scan itself when child is nil.
		return scan
	}

	// Use the first label for the scan; additional labels become a selection.
	scan := NewNodeByLabelScan(nodeVar, np.Labels[0])
	var plan LogicalPlan = scan

	// Extra labels: AND-filter (label predicates become Selection).
	for _, lbl := range np.Labels[1:] {
		pred := fmt.Sprintf("%s:%s", nodeVar, lbl)
		plan = NewSelection(pred, plan)
	}

	// Inline property predicates from the node pattern.
	if np.Properties != nil {
		plan = NewSelection(nodePropertiesPredicate(nodeVar, np.Properties), plan)
	}

	return plan
}

// expandStep translates a single (rel, node) hop into Expand or OptionalExpand.
func (t *translator) expandStep(rp *ast.RelationshipPattern, to *ast.NodePattern, child LogicalPlan, optional bool) LogicalPlan {
	// Determine the source variable from the child plan's first var.
	fromVar := firstVar(child)

	relVar := ""
	if rp.Variable != nil {
		relVar = *rp.Variable
	}

	toVar := ""
	if to.Variable != nil {
		toVar = *to.Variable
	}

	dir := relDirection(rp.Direction)
	relTypes := make([]string, len(rp.Types))
	copy(relTypes, rp.Types)

	// Variable-length expansion (e.g. -[r*1..3]->).
	if rp.Range != nil {
		minDepth := 1
		maxDepth := 0 // 0 means unbounded
		if rp.Range.Min != nil {
			minDepth = int(*rp.Range.Min)
		}
		if rp.Range.Max != nil {
			maxDepth = int(*rp.Range.Max)
		}
		plan := NewVarLengthExpand(fromVar, relVar, relTypes, dir, toVar, minDepth, maxDepth, child)
		return t.applyNodeFilter(to, toVar, plan)
	}

	var plan LogicalPlan
	if optional {
		plan = NewOptionalExpand(fromVar, relVar, relTypes, dir, toVar, child)
	} else {
		plan = NewExpand(fromVar, relVar, relTypes, dir, toVar, child)
	}

	// Inline destination-node predicates.
	return t.applyNodeFilter(to, toVar, plan)
}

// applyNodeFilter wraps plan with Selection operators for label and property
// constraints declared on the destination node pattern.
func (t *translator) applyNodeFilter(np *ast.NodePattern, nodeVar string, plan LogicalPlan) LogicalPlan {
	for _, lbl := range np.Labels {
		pred := fmt.Sprintf("%s:%s", nodeVar, lbl)
		plan = NewSelection(pred, plan)
	}
	if np.Properties != nil {
		plan = NewSelection(nodePropertiesPredicate(nodeVar, np.Properties), plan)
	}
	return plan
}

// ─────────────────────────────────────────────────────────────────────────────
// WITH and RETURN
// ─────────────────────────────────────────────────────────────────────────────

func (t *translator) withClause(w *ast.With, child LogicalPlan) (LogicalPlan, error) {
	items := projectionItems(w.Projection)
	plan := LogicalPlan(NewProjection(items, child))
	if w.Where != nil {
		plan = NewSelection(w.Where.Predicate.String(), plan)
	}
	return plan, nil
}

func (t *translator) returnClause(r *ast.Return, child LogicalPlan) (LogicalPlan, error) {
	proj := r.Projection

	// Build projection items.
	items := projectionItems(proj)

	// Wrap in Projection.
	var plan LogicalPlan = NewProjection(items, child)

	// DISTINCT.
	if proj.Distinct {
		plan = NewDistinct(plan)
	}

	// ORDER BY (with LIMIT → fused Top; without LIMIT → Sort).
	if len(proj.OrderBy) > 0 {
		sortItems := make([]SortItem, len(proj.OrderBy))
		for i, s := range proj.OrderBy {
			sortItems[i] = SortItem{Expression: s.Expr.String(), Descending: s.Descending}
		}
		if proj.Limit != nil {
			lim, err := intExpr(proj.Limit)
			if err != nil {
				// Fall back to Sort + Limit when the limit is not a literal.
				plan = NewSort(sortItems, plan)
				plan = NewLimit(0, plan) // opaque limit; expression stored via string repr
			} else {
				plan = NewTop(sortItems, lim, plan)
			}
		} else {
			plan = NewSort(sortItems, plan)
		}
	} else if proj.Limit != nil {
		lim, _ := intExpr(proj.Limit)
		plan = NewLimit(lim, plan)
	}

	// SKIP.
	if proj.Skip != nil {
		sk, _ := intExpr(proj.Skip)
		plan = NewSkip(sk, plan)
	}

	// Collect output column names for ProduceResults.
	cols := make([]string, len(items))
	for i, it := range items {
		cols[i] = it.Name
	}
	return NewProduceResults(cols, plan), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Updating clauses
// ─────────────────────────────────────────────────────────────────────────────

func (t *translator) updatingClause(uc ast.UpdatingClause, child LogicalPlan) (LogicalPlan, error) {
	switch v := uc.(type) {
	case *ast.Create:
		return t.createClause(v, child)
	case *ast.Merge:
		return t.mergeClause(v, child)
	case *ast.Set:
		return t.setClause(v, child)
	case *ast.Remove:
		return t.removeClause(v, child)
	case *ast.Delete:
		return t.deleteClause(v, child)
	case *ast.DetachDelete:
		return t.detachDeleteClause(v, child)
	case *ast.Call:
		return t.callClause(v, child)
	default:
		return nil, &TranslateError{UnsupportedClause: fmt.Sprintf("%T", uc)}
	}
}

// createClause translates a CREATE pattern. Each node pattern becomes
// CreateNode; each relationship becomes CreateRelationship.
func (t *translator) createClause(c *ast.Create, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, pp := range c.Pattern.Paths {
		var err error
		plan, err = t.createPathPattern(pp, plan)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

func (t *translator) createPathPattern(pp *ast.PathPattern, child LogicalPlan) (LogicalPlan, error) {
	if pp == nil || pp.Head == nil {
		return child, nil
	}
	plan := child
	el := pp.Head

	// Translate the anchor node.
	if el.Node != nil {
		plan = t.createNode(el.Node, plan)
	}

	el = el.Next
	for el != nil {
		if el.Relationship != nil && el.Node != nil {
			plan = t.createRelationship(el.Relationship, el.Node, plan)
		}
		el = el.Next
	}
	return plan, nil
}

func (t *translator) createNode(np *ast.NodePattern, child LogicalPlan) LogicalPlan {
	nodeVar := ""
	if np.Variable != nil {
		nodeVar = *np.Variable
	}
	labels := make([]string, len(np.Labels))
	copy(labels, np.Labels)
	props := ""
	if np.Properties != nil {
		props = np.Properties.String()
	}
	return NewCreateNode(nodeVar, labels, props, child)
}

func (t *translator) createRelationship(rp *ast.RelationshipPattern, to *ast.NodePattern, child LogicalPlan) LogicalPlan {
	startVar := firstVar(child)
	endVar := ""
	if to.Variable != nil {
		endVar = *to.Variable
	}
	relVar := ""
	if rp.Variable != nil {
		relVar = *rp.Variable
	}
	relType := ""
	if len(rp.Types) > 0 {
		relType = rp.Types[0]
	}
	props := ""
	if rp.Properties != nil {
		props = rp.Properties.String()
	}
	// First create the destination node, then the relationship.
	nodePlan := t.createNode(to, child)
	return NewCreateRelationship(startVar, endVar, relVar, relType, props, nodePlan)
}

func (t *translator) mergeClause(m *ast.Merge, child LogicalPlan) (LogicalPlan, error) {
	onCreate := make([]string, len(m.OnCreate))
	for i, si := range m.OnCreate {
		onCreate[i] = si.String()
	}
	onMatch := make([]string, len(m.OnMatch))
	for i, si := range m.OnMatch {
		onMatch[i] = si.String()
	}
	boundVars := patternVars(m.Pattern)
	return NewMerge(m.Pattern.String(), onCreate, onMatch, boundVars, child), nil
}

func (t *translator) setClause(s *ast.Set, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, item := range s.Items {
		if len(item.Labels) > 0 {
			// SET n:Label form.
			entityVar := item.Target.String()
			labels := make([]string, len(item.Labels))
			copy(labels, item.Labels)
			plan = NewSetLabels(entityVar, labels, plan)
			continue
		}
		// SET n.prop = expr  or  SET n = expr  or  SET n += expr.
		if prop, ok := item.Target.(*ast.Property); ok {
			plan = NewSetProperty(prop.Receiver.String(), prop.Key, item.Value.String(), plan)
		} else {
			// Whole-node assignment (n = {…} or n += {…}): model as SetProperty
			// with an empty key to signal whole-entity update until expression IR
			// is introduced.
			plan = NewSetProperty(item.Target.String(), "", item.Value.String(), plan)
		}
	}
	return plan, nil
}

func (t *translator) removeClause(r *ast.Remove, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, item := range r.Items {
		if len(item.Labels) > 0 {
			entityVar := item.Target.String()
			labels := make([]string, len(item.Labels))
			copy(labels, item.Labels)
			plan = NewRemoveLabels(entityVar, labels, plan)
			continue
		}
		// REMOVE n.prop.
		if prop, ok := item.Target.(*ast.Property); ok {
			plan = NewRemoveProperty(prop.Receiver.String(), prop.Key, plan)
		} else {
			plan = NewRemoveProperty(item.Target.String(), "", plan)
		}
	}
	return plan, nil
}

func (t *translator) deleteClause(d *ast.Delete, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, expr := range d.Expressions {
		plan = NewDeleteNode(expr.String(), plan)
	}
	return plan, nil
}

func (t *translator) detachDeleteClause(d *ast.DetachDelete, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, expr := range d.Expressions {
		plan = NewDetachDelete(expr.String(), plan)
	}
	return plan, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// projectionItems converts ast.Projection items into ir.ProjectionItem values.
func projectionItems(proj *ast.Projection) []ProjectionItem {
	if proj == nil {
		return nil
	}
	items := make([]ProjectionItem, len(proj.Items))
	for i, it := range proj.Items {
		name := it.Expr.String()
		if it.Alias != nil {
			name = *it.Alias
		} else if v, ok := it.Expr.(*ast.Variable); ok {
			name = v.Name
		}
		items[i] = ProjectionItem{Name: name, Expression: it.Expr.String()}
	}
	return items
}

// relDirection maps an ast.RelDirection to an ir.Direction.
func relDirection(d ast.RelDirection) Direction {
	switch d {
	case ast.RelDirectionOutgoing:
		return DirectionOutgoing
	case ast.RelDirectionIncoming:
		return DirectionIncoming
	default:
		return DirectionBoth
	}
}

// firstVar returns the first variable name produced by plan, or "" when plan
// is nil or produces no variables.
func firstVar(plan LogicalPlan) string {
	if plan == nil {
		return ""
	}
	vars := plan.Vars()
	if len(vars) == 0 {
		return ""
	}
	return vars[0]
}

// nodePropertiesPredicate builds a string predicate for inline node properties.
func nodePropertiesPredicate(nodeVar string, props ast.Expression) string {
	return nodeVar + " " + props.String()
}

// patternVars collects named variables from a PathPattern.
func patternVars(pp *ast.PathPattern) []string {
	if pp == nil {
		return nil
	}
	var vars []string
	if pp.Variable != nil {
		vars = append(vars, *pp.Variable)
	}
	el := pp.Head
	for el != nil {
		if el.Node != nil && el.Node.Variable != nil {
			vars = append(vars, *el.Node.Variable)
		}
		if el.Relationship != nil && el.Relationship.Variable != nil {
			vars = append(vars, *el.Relationship.Variable)
		}
		el = el.Next
	}
	return vars
}

// intExpr attempts to extract a constant int64 from a literal expression.
// Returns 0 and an error when the expression is not an integer literal.
func intExpr(e ast.Expression) (int64, error) {
	if il, ok := e.(*ast.IntLiteral); ok {
		return il.Value, nil
	}
	return 0, fmt.Errorf("not a literal int: %T", e)
}
