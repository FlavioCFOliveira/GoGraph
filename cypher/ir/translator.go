package ir

import (
	"fmt"
	"strconv"

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
// construction. It carries an anonCounter for generating unique internal variable
// names for anonymous nodes in CREATE patterns (e.g. CREATE ()-[:R]->()).
type translator struct {
	anonCounter int // monotonic counter for synthetic anonymous-node vars
}

// freshAnonVar returns a unique internal variable name for an anonymous node
// created in a CREATE clause. The name is prefixed with "__anon_" to avoid
// collisions with user-visible variable names.
func (t *translator) freshAnonVar() string {
	n := t.anonCounter
	t.anonCounter++
	return "__anon_" + strconv.Itoa(n)
}

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

// matchClause translates MATCH. Delegates to translateMatch in match.go.
// optional=true produces OptionalExpand instead of Expand on relationships.
func (t *translator) matchClause(m *ast.Match, child LogicalPlan, optional bool) (LogicalPlan, error) {
	return t.translateMatch(m, child, optional)
}

func (t *translator) optionalMatchClause(m *ast.OptionalMatch, child LogicalPlan) (LogicalPlan, error) {
	return t.translateOptionalMatch(m, child)
}

func (t *translator) unwindClause(u *ast.Unwind, child LogicalPlan) (LogicalPlan, error) {
	return NewUnwindExpr(u.Expr.String(), u.Expr, u.Variable, child), nil
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
// WITH and RETURN
// ─────────────────────────────────────────────────────────────────────────────

func (t *translator) withClause(w *ast.With, child LogicalPlan) (LogicalPlan, error) {
	return t.translateWith(w, child)
}

func (t *translator) returnClause(r *ast.Return, child LogicalPlan) (LogicalPlan, error) {
	proj := r.Projection

	// Handle pattern comprehensions first: each comprehension item becomes a
	// RollUpApply layer. The remaining (non-comprehension) items are returned
	// as regularItems.
	planAfterComp, regularItems, err := t.projectionsWithComprehensions(proj, child)
	if err != nil {
		return nil, err
	}

	// When all items were comprehensions, use an empty projection items list.
	var items []ProjectionItem
	if len(regularItems) > 0 {
		items = regularItems
	} else {
		items = projectionItems(proj)
	}

	// Detect aggregate functions among non-comprehension items. When present,
	// wrap in EagerAggregation first, then Projection.
	var plan LogicalPlan
	groupBy, groupByExprs, aggs, hasAgg := detectAggregation(proj)
	if hasAgg {
		plan = NewEagerAggregationWithExprs(groupBy, groupByExprs, aggs, planAfterComp)
		plan = NewProjection(items, plan)
	} else {
		plan = NewProjection(items, planAfterComp)
	}

	// DISTINCT.
	if proj.Distinct {
		plan = NewDistinct(plan)
	}

	// SKIP must be applied before LIMIT so that the offset is taken from the
	// full result stream, not from an already-truncated one. The IR is built
	// bottom-up, so we apply SKIP first (inner) and LIMIT on top (outer).
	if proj.Skip != nil {
		sk, _ := intExpr(proj.Skip)
		plan = NewSkip(sk, plan)
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
	// Write clause implementations are in writes.go.
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
		items[i] = ProjectionItem{Name: name, Expression: it.Expr.String(), Expr: it.Expr}
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
//
// The parser emits *ast.IntLiteral for integer literals that appear inside
// arithmetic expressions, but emits *ast.Variable when the integer appears
// directly in a LIMIT or SKIP position (where the grammar funnels the token
// through the general Symbol rule). Both representations are handled here.
func intExpr(e ast.Expression) (int64, error) {
	if il, ok := e.(*ast.IntLiteral); ok {
		return il.Value, nil
	}
	// The Cypher grammar routes integer literals in LIMIT/SKIP through the
	// Symbol → Variable path when they are the sole atom of the expression.
	// Parse the variable name as an integer in that case.
	if v, ok := e.(*ast.Variable); ok {
		n, err := strconv.ParseInt(v.Name, 10, 64)
		if err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("not a literal int: %T", e)
}
