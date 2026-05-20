package sema

import (
	"gograph/cypher/ast"
)

// Analyse runs the scope-analysis pass on q and returns all scope violations
// found. An empty (or nil) slice means the query is scope-clean.
//
// Rules enforced:
//   - MATCH / OPTIONAL MATCH: NodePattern and RelationshipPattern variables are
//     introduced into the current scope. Duplicate variable names within the
//     same scope are an error.
//   - WHERE: references must be defined in the current scope; no new variables.
//   - UNWIND … AS x: introduces x into the current scope.
//   - WITH: acts as a scope boundary — after WITH only the projected names
//     survive. AS aliases create new names; bare variable references must be
//     in scope before the WITH.
//   - RETURN: each projected expression must reference only defined variables.
//   - CREATE / MERGE: pattern variables may be new (introduction) or
//     previously defined (re-use). Re-use of an already-defined variable in
//     CREATE is permitted (bound-node reuse); introducing a duplicate in the
//     same scope is an error.
//   - SET / REMOVE / DELETE: references must be in scope.
//   - CALL … YIELD: each yielded item introduces a new variable.
//   - List comprehension / pattern comprehension: variable binding is local to
//     the comprehension; using it outside is a scope leak.
//   - EXISTS { } / COUNT { } with a full subquery: analysed in an isolated
//     child scope; outer variables are visible inside but inner variables do
//     not leak out.
func Analyse(q ast.Query) []ScopeError {
	a := &analyser{}
	a.query(q)
	return a.errs
}

// analyser holds the mutable state accumulated during the single-pass walk.
type analyser struct {
	scope *Scope
	errs  []ScopeError
}

func (a *analyser) error(e *ScopeError) {
	if e != nil {
		a.errs = append(a.errs, *e)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Query dispatch
// ─────────────────────────────────────────────────────────────────────────────

func (a *analyser) query(q ast.Query) {
	switch v := q.(type) {
	case *ast.SingleQuery:
		a.singleQuery(v)
	case *ast.MultiQuery:
		a.multiQuery(v)
	}
}

func (a *analyser) multiQuery(mq *ast.MultiQuery) {
	// Each branch of a UNION is analysed in an independent scope; they do not
	// share variable bindings.
	for _, part := range mq.Parts {
		sub := &analyser{}
		sub.singleQuery(part)
		a.errs = append(a.errs, sub.errs...)
	}
}

// singleQuery processes the clauses of a SingleQuery in semantic order:
// reading clauses → WITH clauses → updating clauses → RETURN.
//
// NOTE: The AST stores reading, WITH, and updating clauses in separate slices.
// The actual interleaving order depends on how the parser filled them.  We walk
// the three slices in their canonical order; this matches the openCypher spec
// for the common case of: MATCH … [WITH …] [CREATE/SET/…] [RETURN …].
func (a *analyser) singleQuery(q *ast.SingleQuery) {
	if a.scope == nil {
		a.scope = newScope()
	}

	for _, rc := range q.ReadingClauses {
		a.readingClause(rc)
	}
	for _, w := range q.With {
		a.withClause(w)
	}
	for _, uc := range q.UpdatingClauses {
		a.updatingClause(uc)
	}
	if q.Return != nil {
		a.returnClause(q.Return)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Reading clauses
// ─────────────────────────────────────────────────────────────────────────────

func (a *analyser) readingClause(rc ast.ReadingClause) {
	switch v := rc.(type) {
	case *ast.Match:
		a.matchClause(v)
	case *ast.OptionalMatch:
		a.optionalMatchClause(v)
	case *ast.Unwind:
		a.unwindClause(v)
	case *ast.With:
		a.withClause(v)
	case *ast.Call:
		a.callClause(v)
	case *ast.Return:
		a.returnClause(v)
	}
}

func (a *analyser) matchClause(m *ast.Match) {
	a.patternIntroduce(m.Pattern)
	if m.Where != nil {
		a.whereClause(m.Where)
	}
}

func (a *analyser) optionalMatchClause(m *ast.OptionalMatch) {
	a.patternIntroduce(m.Pattern)
	if m.Where != nil {
		a.whereClause(m.Where)
	}
}

func (a *analyser) unwindClause(u *ast.Unwind) {
	// The source expression is evaluated in the current scope.
	a.checkExpr(u.Expr)
	// The AS variable is introduced into the current scope.
	a.error(a.scope.Define(u.Variable, u.Pos, "any"))
}

func (a *analyser) withClause(w *ast.With) {
	// 1. Evaluate each projected expression against the pre-WITH scope.
	//    (AS aliases are not yet in scope.)
	type projection struct {
		expr  ast.Expression
		alias *string
		pos   ast.Position
	}
	projs := make([]projection, 0, len(w.Projection.Items))
	for _, item := range w.Projection.Items {
		a.checkExprForWith(item.Expr)
		projs = append(projs, projection{item.Expr, item.Alias, item.Pos})
	}

	// 2. WHERE on WITH is also evaluated in the pre-WITH scope, but after
	//    the projections are determined (and their aliases are visible).
	//    openCypher spec: WHERE after WITH sees the projected names.
	//    We build the new scope first, then check WHERE.

	// 3. Reset scope: only projected names survive.
	a.scope.reset()

	for _, p := range projs {
		name := projectedName(p.expr, p.alias)
		if name == "" {
			// Non-nameable projection (e.g. a literal): skip.
			continue
		}
		a.error(a.scope.Define(name, p.pos, "any"))
	}

	if w.Where != nil {
		a.whereClause(w.Where)
	}
}

// projectedName returns the variable name that a WITH/RETURN projection item
// introduces into the new scope:
//   - If there is an AS alias, the alias wins.
//   - Otherwise, if the expression is a bare Variable, its name is used.
//   - Otherwise returns "" (no name is introduced).
func projectedName(expr ast.Expression, alias *string) string {
	if alias != nil {
		return *alias
	}
	if v, ok := expr.(*ast.Variable); ok {
		return v.Name
	}
	return ""
}

func (a *analyser) callClause(c *ast.Call) {
	// Arguments are evaluated in the current scope.
	for _, arg := range c.Args {
		a.checkExpr(arg)
	}
	// YIELD items introduce new variables.
	for _, item := range c.Yield {
		name := item.Name
		if item.Alias != nil {
			name = *item.Alias
		}
		a.error(a.scope.Define(name, item.Pos, "any"))
	}
	if c.Where != nil {
		a.whereClause(c.Where)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Updating clauses
// ─────────────────────────────────────────────────────────────────────────────

func (a *analyser) updatingClause(uc ast.UpdatingClause) {
	switch v := uc.(type) {
	case *ast.Create:
		a.createClause(v)
	case *ast.Merge:
		a.mergeClause(v)
	case *ast.Set:
		a.setClause(v)
	case *ast.Remove:
		a.removeClause(v)
	case *ast.Delete:
		a.deleteClause(v)
	case *ast.DetachDelete:
		a.detachDeleteClause(v)
	case *ast.Call:
		a.callClause(v)
	}
}

func (a *analyser) createClause(c *ast.Create) {
	// CREATE introduces new variables from the pattern.
	a.patternIntroduce(c.Pattern)
}

func (a *analyser) mergeClause(m *ast.Merge) {
	// MERGE may introduce new variables or reuse existing ones.
	a.pathPatternIntroduce(m.Pattern)
	// ON CREATE / ON MATCH SET items reference existing variables.
	for _, si := range m.OnCreate {
		a.checkExpr(si.Target)
		if si.Value != nil {
			a.checkExpr(si.Value)
		}
	}
	for _, si := range m.OnMatch {
		a.checkExpr(si.Target)
		if si.Value != nil {
			a.checkExpr(si.Value)
		}
	}
}

func (a *analyser) setClause(s *ast.Set) {
	for _, item := range s.Items {
		a.checkExpr(item.Target)
		if item.Value != nil {
			a.checkExpr(item.Value)
		}
	}
}

func (a *analyser) removeClause(r *ast.Remove) {
	for _, item := range r.Items {
		a.checkExpr(item.Target)
	}
}

func (a *analyser) deleteClause(d *ast.Delete) {
	for _, e := range d.Expressions {
		a.checkExpr(e)
	}
}

func (a *analyser) detachDeleteClause(d *ast.DetachDelete) {
	for _, e := range d.Expressions {
		a.checkExpr(e)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Return clause
// ─────────────────────────────────────────────────────────────────────────────

func (a *analyser) returnClause(r *ast.Return) {
	a.projectionCheck(r.Projection)
}

// ─────────────────────────────────────────────────────────────────────────────
// WHERE
// ─────────────────────────────────────────────────────────────────────────────

func (a *analyser) whereClause(w *ast.Where) {
	a.checkExpr(w.Predicate)
}

// ─────────────────────────────────────────────────────────────────────────────
// Pattern introduction helpers
// ─────────────────────────────────────────────────────────────────────────────

// patternIntroduce walks all path patterns and introduces any named variables
// into the current scope.
func (a *analyser) patternIntroduce(pat *ast.Pattern) {
	if pat == nil {
		return
	}
	for _, pp := range pat.Paths {
		a.pathPatternIntroduce(pp)
	}
}

func (a *analyser) pathPatternIntroduce(pp *ast.PathPattern) {
	if pp == nil {
		return
	}
	// Path variable (p = (a)-[r]->(b)) — introduce into scope.
	if pp.Variable != nil {
		a.error(a.scope.Define(*pp.Variable, pp.Pos, "path"))
	}
	el := pp.Head
	for el != nil {
		if el.Node != nil {
			a.nodePatternIntroduce(el.Node)
		}
		if el.Relationship != nil {
			a.relPatternIntroduce(el.Relationship)
		}
		el = el.Next
	}
}

func (a *analyser) nodePatternIntroduce(np *ast.NodePattern) {
	if np.Variable == nil {
		return
	}
	name := *np.Variable
	// If already defined, it is a re-use (bound node), not a redeclaration.
	if _, ok := a.scope.Lookup(name); ok {
		return
	}
	a.error(a.scope.Define(name, np.Pos, "node"))
}

func (a *analyser) relPatternIntroduce(rp *ast.RelationshipPattern) {
	if rp.Variable == nil {
		return
	}
	name := *rp.Variable
	// If already defined, it is a re-use (bound relationship), not a redeclaration.
	if _, ok := a.scope.Lookup(name); ok {
		return
	}
	a.error(a.scope.Define(name, rp.Pos, "relationship"))
}

// ─────────────────────────────────────────────────────────────────────────────
// Projection check (RETURN / WITH source-expression validation)
// ─────────────────────────────────────────────────────────────────────────────

// projectionCheck validates that all expressions in a RETURN projection
// reference only variables that are in scope.
func (a *analyser) projectionCheck(proj *ast.Projection) {
	if proj.All {
		return // RETURN * — accept everything that is in scope
	}
	for _, item := range proj.Items {
		a.checkExpr(item.Expr)
	}
	for _, s := range proj.OrderBy {
		a.checkExpr(s.Expr)
	}
	if proj.Skip != nil {
		a.checkExpr(proj.Skip)
	}
	if proj.Limit != nil {
		a.checkExpr(proj.Limit)
	}
}

// checkExprForWith validates projection source expressions in WITH.
// It is identical to checkExpr; the separate name makes the call-site intent
// explicit when reading the WITH logic.
func (a *analyser) checkExprForWith(e ast.Expression) {
	a.checkExpr(e)
}

// ─────────────────────────────────────────────────────────────────────────────
// Expression checker
// ─────────────────────────────────────────────────────────────────────────────

// checkExpr recursively validates that every Variable reference in e resolves
// to a symbol in the current scope chain.
//
//nolint:gocyclo // One branch per concrete Expression type; complexity is structural.
func (a *analyser) checkExpr(e ast.Expression) {
	if e == nil {
		return
	}
	switch v := e.(type) {
	case *ast.Variable:
		if _, ok := a.scope.Lookup(v.Name); !ok {
			a.error(undefinedVarError(v.Name, v.Pos))
		}

	case *ast.Property:
		a.checkExpr(v.Receiver)

	case *ast.BinaryOp:
		a.checkExpr(v.Left)
		a.checkExpr(v.Right)

	case *ast.UnaryOp:
		a.checkExpr(v.Operand)

	case *ast.FunctionInvocation:
		for _, arg := range v.Args {
			a.checkExpr(arg)
		}

	case *ast.CaseExpression:
		a.checkExpr(v.Subject)
		for _, alt := range v.Alternatives {
			a.checkExpr(alt.Condition)
			a.checkExpr(alt.Consequent)
		}
		a.checkExpr(v.ElseExpr)

	case *ast.ListLiteral:
		for _, elem := range v.Elements {
			a.checkExpr(elem)
		}

	case *ast.MapLiteral:
		for _, val := range v.Values {
			a.checkExpr(val)
		}

	case *ast.SubscriptExpr:
		a.checkExpr(v.Expr)
		a.checkExpr(v.Index)

	case *ast.SliceExpr:
		a.checkExpr(v.Expr)
		a.checkExpr(v.From)
		a.checkExpr(v.To)

	case *ast.ListComprehension:
		// Source is evaluated in the outer scope.
		a.checkExpr(v.Source)
		// The loop variable is local to the comprehension.
		inner := a.scope.Child()
		saved := a.scope
		a.scope = inner
		if err := a.scope.Define(v.Variable, v.Pos, "any"); err != nil {
			a.error(err)
		}
		a.checkExpr(v.Predicate)
		a.checkExpr(v.Projection)
		a.scope = saved

	case *ast.PatternComprehension:
		// Pattern comprehension: optional path variable + pattern + predicate + projection.
		inner := a.scope.Child()
		saved := a.scope
		a.scope = inner
		if v.Variable != nil {
			if err := a.scope.Define(*v.Variable, v.Pos, "path"); err != nil {
				a.error(err)
			}
		}
		a.pathPatternIntroduce(v.Pattern)
		a.checkExpr(v.Predicate)
		a.checkExpr(v.Projection)
		a.scope = saved

	case *ast.MapProjection:
		a.checkExpr(v.Subject)
		for _, item := range v.Items {
			if !item.IsAll && item.Value != nil {
				a.checkExpr(item.Value)
			}
		}

	case *ast.ExistsSubquery:
		a.existsSubquery(v)

	case *ast.CountSubquery:
		a.countSubquery(v)

	case *ast.PathPattern:
		// PathPattern in expression context (e.g. shortestPath): introduce
		// variables but only check them — they are pattern-bound here.
		a.pathPatternIntroduce(v)

	// Literals and parameters carry no variable references.
	case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral,
		*ast.BoolLiteral, *ast.NullLiteral, *ast.Parameter:
		// nothing

	default:
		// Unknown expression type — no action; do not panic.
	}
}

// existsSubquery analyses an EXISTS { … } expression.  The subquery sees all
// outer-scope variables but may not leak its own variables outwards.
func (a *analyser) existsSubquery(e *ast.ExistsSubquery) {
	sub := &analyser{scope: a.scope.Child()}
	if e.Pattern != nil {
		sub.patternIntroduce(e.Pattern)
	} else if e.Query != nil {
		sub.singleQuery(e.Query)
	}
	a.errs = append(a.errs, sub.errs...)
}

// countSubquery analyses a COUNT { … } expression identically to EXISTS.
func (a *analyser) countSubquery(c *ast.CountSubquery) {
	sub := &analyser{scope: a.scope.Child()}
	if c.Pattern != nil {
		sub.patternIntroduce(c.Pattern)
	} else if c.Query != nil {
		sub.singleQuery(c.Query)
	}
	a.errs = append(a.errs, sub.errs...)
}
