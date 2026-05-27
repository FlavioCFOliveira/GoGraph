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

// TranslateSubquery translates a single-query AST (the body of an EXISTS / COUNT
// subquery) into a [LogicalPlan] rooted at an [Argument] leaf carrying the
// supplied correlation variables and ArgTag. The returned plan is suitable as
// the inner pipeline of a per-row subquery driver.
//
// outerVars is the list of variable names visible from the lexical outer
// scope at the subquery's call site; they are injected into the inner
// pipeline through the leading Argument so correlated patterns (e.g.
// (n)-->() where n is bound outside) observe the outer row's bindings at
// runtime.
//
// argTag must equal the tag the physical builder will register the seed
// [Argument] under in its argByTag map; the leading IR Argument carries this
// tag so the leaf operator resolves to the same exec.Argument instance.
//
// Concurrency: stateless; safe to call concurrently.
func TranslateSubquery(q *ast.SingleQuery, outerVars []string, argTag uint32) (LogicalPlan, error) {
	t := &translator{}
	arg := NewArgumentWithTag(outerVars, argTag)
	plan := LogicalPlan(arg)
	for _, rc := range q.ReadingClauses {
		var err error
		plan, err = t.readingClause(rc, plan)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
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

	// For parser-generated MultiPartQ queries, q.LeadingCountSet is true and
	// q.ReadingClauses already contains both the reading clauses and the WITH
	// clauses (as *ast.With nodes) in document order.  q.With is empty.
	// Processing all q.ReadingClauses through t.readingClause — which already
	// dispatches *ast.With via t.withClause — gives the correct evaluation order.
	//
	// For manually-constructed ast.SingleQuery objects (unit tests), q.LeadingCountSet
	// is false and q.With holds the WITH clauses.  Fall back to the legacy
	// "all ReadingClauses first, then q.With" ordering to preserve compat.
	if q.LeadingCountSet {
		// Document-order processing: interleave reading and updating clauses
		// by source position so that queries like CREATE … WITH … RETURN
		// place the CREATE before the WITH (otherwise the WITH runs with
		// no input and the subsequent CREATE re-references the now-bound
		// variable, dropping the actual node creation).
		clauses := make([]orderedClause, 0, len(q.ReadingClauses)+len(q.UpdatingClauses))
		for _, rc := range q.ReadingClauses {
			clauses = append(clauses, orderedClause{pos: positionFromNode(rc), kind: 0, rc: rc})
		}
		for _, uc := range q.UpdatingClauses {
			clauses = append(clauses, orderedClause{pos: positionFromNode(uc), kind: 1, uc: uc})
		}
		sortByPos(clauses)
		for _, c := range clauses {
			var err error
			if c.kind == 0 {
				plan, err = t.readingClause(c.rc, plan)
			} else {
				plan, err = t.updatingClause(c.uc, plan)
			}
			if err != nil {
				return nil, err
			}
		}
	} else {
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

// positionFromNode returns the Pos field of any ast.Node that carries one.
// It uses a small type switch over the concrete clause kinds the IR
// translator actually iterates over.
func positionFromNode(n ast.Node) ast.Position {
	switch v := n.(type) {
	case *ast.Match:
		return v.Pos
	case *ast.OptionalMatch:
		return v.Pos
	case *ast.Unwind:
		return v.Pos
	case *ast.With:
		return v.Pos
	case *ast.Call:
		return v.Pos
	case *ast.Create:
		return v.Pos
	case *ast.Merge:
		return v.Pos
	case *ast.Set:
		return v.Pos
	case *ast.Remove:
		return v.Pos
	case *ast.Delete:
		return v.Pos
	case *ast.DetachDelete:
		return v.Pos
	}
	return ast.Position{}
}

// orderedClause is a (reading or updating) clause paired with its source
// position so the singleQuery loop can interleave them by document order.
type orderedClause struct {
	pos  ast.Position
	kind int // 0=reading, 1=updating
	rc   ast.ReadingClause
	uc   ast.UpdatingClause
}

// sortByPos sorts the orderedClause slice by source-position offset using
// insertion sort (the slices are short — typically ≤6 clauses).
func sortByPos(s []orderedClause) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1].pos.Offset > s[j].pos.Offset {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

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
		items = projectionItems(proj, collectAllVars(planAfterComp))
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
			sortItems[i] = SortItem{Expression: s.Expr.String(), Expr: s.Expr, Descending: s.Descending}
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
// When proj.All is true (the openCypher `*` projection), every variable
// introduced by the child subtree is added to the projection before any
// explicit items, so `RETURN *` and `WITH *` forward all in-scope
// bindings. childVars supplies the in-scope variable list; pass nil when
// the caller does not have a child plan available.
func projectionItems(proj *ast.Projection, childVars []string) []ProjectionItem {
	if proj == nil {
		return nil
	}
	var items []ProjectionItem
	if proj.All {
		for _, v := range childVars {
			if v == "" {
				continue
			}
			items = append(items, ProjectionItem{
				Name:       v,
				Expression: v,
				Expr:       &ast.Variable{Name: v},
			})
		}
	}
	for _, it := range proj.Items {
		items = append(items, ProjectionItem{
			Name:       projectionColumnName(it),
			Expression: it.Expr.String(),
			Expr:       it.Expr,
		})
	}
	return items
}

// projectionColumnName returns the canonical openCypher column header
// for a single projection item. Priority:
//
//  1. Explicit AS alias.
//  2. Bare variable name (preserves source spelling, e.g. `n`).
//  3. Otherwise the precedence-aware textual form via exprToColumnName,
//     which avoids the over-parenthesised output of
//     [*ast.BinaryOp.String]. The TCK column-header convention is the
//     unparenthesised source form whenever the AST shape is
//     unambiguous from operator precedence.
func projectionColumnName(it *ast.ProjectionItem) string {
	if it.Alias != nil {
		return *it.Alias
	}
	if v, ok := it.Expr.(*ast.Variable); ok {
		return v.Name
	}
	return exprToColumnName(it.Expr)
}

// exprToColumnName renders an AST expression in the canonical TCK
// column-header form. The implementation walks BinaryOp / UnaryOp
// nodes with operator precedence so a child is only parenthesised
// when its operator binds less tightly than its parent. Non-arithmetic
// expressions fall through to their String() representation, with the
// exception of [*ast.Property] whose receiver is wrapped in parens
// when the receiver itself is not a bare variable (matching the TCK
// canonical form `(list[1]).existing`).
func exprToColumnName(e ast.Expression) string {
	switch n := e.(type) {
	case *ast.BinaryOp:
		left := exprToColumnNameWithParent(n.Left, binaryOpPrecedence(n.Operator), true)
		right := exprToColumnNameWithParent(n.Right, binaryOpPrecedence(n.Operator), false)
		return left + " " + n.Operator + " " + right
	case *ast.UnaryOp:
		// Unary operators have higher precedence than any binary; their
		// operand only needs parens when it is a BinaryOp.
		operand := exprToColumnName(n.Operand)
		if _, isBin := n.Operand.(*ast.BinaryOp); isBin {
			operand = "(" + operand + ")"
		}
		// IS NULL / IS NOT NULL are postfix; every other unary operator
		// (NOT, unary -, unary +) is prefix. Postfix operators render as
		// `<operand> <op>`; prefix as `<op><operand>` (NOT requires a
		// space, the arithmetic unaries do not — match the canonical
		// TCK header form).
		op := n.Operator
		if op == "IS NULL" || op == "IS NOT NULL" {
			return operand + " " + op
		}
		if op == "NOT" {
			return op + " " + operand
		}
		return op + operand
	case *ast.Property:
		// `n.x` keeps the bare-receiver spelling; expressions like
		// `(list[1]).x` or `(case … end).x` parenthesise the receiver.
		if _, isVar := n.Receiver.(*ast.Variable); isVar {
			return n.Receiver.String() + "." + n.Key
		}
		return "(" + exprToColumnName(n.Receiver) + ")." + n.Key
	default:
		return e.String()
	}
}

// exprToColumnNameWithParent renders a sub-expression with paren-
// guarding driven by the enclosing parent's precedence. isLeft
// disambiguates the right operand of a left-associative parent at
// equal precedence (e.g. `a - (b - c)` differs from `a - b - c`).
func exprToColumnNameWithParent(e ast.Expression, parentPrec int, isLeft bool) string {
	if bin, ok := e.(*ast.BinaryOp); ok {
		childPrec := binaryOpPrecedence(bin.Operator)
		needParen := childPrec < parentPrec || (childPrec == parentPrec && !isLeft && !operatorIsAssociative(bin.Operator))
		s := exprToColumnName(bin)
		if needParen {
			return "(" + s + ")"
		}
		return s
	}
	return exprToColumnName(e)
}

// binaryOpPrecedence returns the precedence level for a Cypher binary
// operator, with lower numbers binding less tightly. The numbers do
// not need to match any external spec — only the relative ordering
// matters for the parenthesisation decision.
func binaryOpPrecedence(op string) int {
	switch op {
	case "OR":
		return 1
	case "XOR":
		return 2
	case "AND":
		return 3
	case "=", "<>", "<", "<=", ">", ">=":
		return 4
	case "STARTS WITH", "ENDS WITH", "CONTAINS", "IN", "=~":
		return 5
	case "+", "-":
		return 6
	case "*", "/", "%":
		return 7
	case "^":
		return 8
	default:
		return 0 // unknown — always parenthesise
	}
}

// operatorIsAssociative reports whether op is associative — i.e. the
// right operand can elide parens at equal precedence. + and *, AND and
// OR are associative; -, /, ^ are not.
func operatorIsAssociative(op string) bool {
	switch op {
	case "+", "*", "AND", "OR", "XOR":
		return true
	}
	return false
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
