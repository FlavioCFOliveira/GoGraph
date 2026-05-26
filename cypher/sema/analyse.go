package sema

import (
	"sort"
	"strings"

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

// singleQuery processes the clauses of a SingleQuery in semantic order.
//
// The AST stores reading, WITH, and updating clauses in three separate
// slices and does not preserve source-level interleaving. The openCypher
// scope rules (WITH boundaries, UNWIND introduction, UpdatingClause
// references) depend on lexical order, so we recover ordering by sorting
// every clause by its [ast.Position]. Clauses with equal positions (e.g.
// programmatically constructed test ASTs that leave Pos zero) preserve
// their original slice order via [sort.SliceStable].
//
// The sorted walk gives correct results for queries such as
//
//	MATCH (n) WITH n CREATE (m) WITH m RETURN m
//
// where MATCH (ReadingClauses), the two WITHs (With), and CREATE
// (UpdatingClauses) interleave at the source level. Without sorting the
// analyser would walk every ReadingClause before any WITH, then every
// UpdatingClause, which silently drops variables that a later WITH would
// otherwise carry into scope.
func (a *analyser) singleQuery(q *ast.SingleQuery) {
	if a.scope == nil {
		a.scope = newScope()
	}

	clauses := orderClauses(q)
	for _, c := range clauses {
		a.dispatchClause(c)
	}
	if q.Return != nil {
		a.returnClause(q.Return)
	}
}

// orderClauses concatenates the three clause slices of q and returns them in
// source order, defined as ascending [ast.Position.Offset]. The sort is
// stable, so clauses with equal Offset (notably zero-Pos test fixtures)
// retain their slice insertion order.
//
// The returned slice's element type is the union interface [ast.Node]
// because Go does not allow a single concrete slice to mix
// [ast.ReadingClause], [ast.UpdatingClause], and *ast.With (each is a
// distinct sealed interface).
func orderClauses(q *ast.SingleQuery) []ast.Node {
	// When LeadingCountSet is true (parser-generated MultiPartQ queries), WITH
	// clauses are already embedded in q.ReadingClauses in document order, so
	// q.With must be excluded to avoid processing each WITH clause twice.
	withClauses := q.With
	if q.LeadingCountSet {
		withClauses = nil
	}
	total := len(q.ReadingClauses) + len(withClauses) + len(q.UpdatingClauses)
	if total == 0 {
		return nil
	}
	out := make([]ast.Node, 0, total)
	for _, c := range q.ReadingClauses {
		out = append(out, c)
	}
	for _, c := range withClauses {
		out = append(out, c)
	}
	for _, c := range q.UpdatingClauses {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return clausePos(out[i]).Offset < clausePos(out[j]).Offset
	})
	return out
}

// clausePos returns the [ast.Position] of a clause node, using a type
// switch over every concrete clause that may appear in
// [ast.SingleQuery.ReadingClauses], [ast.SingleQuery.UpdatingClauses], or
// [ast.SingleQuery.With]. Unknown nodes return the zero Position so they
// sort first; this is a defensive default that should never trigger in
// well-formed ASTs.
func clausePos(n ast.Node) ast.Position {
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
	case *ast.Return:
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

// dispatchClause routes a clause node from the source-ordered walk produced
// by [orderClauses] to the appropriate handler. The three clause families
// (reading / updating / WITH) are dispatched by concrete type rather than
// by interface so the existing handlers are reused verbatim.
func (a *analyser) dispatchClause(n ast.Node) {
	switch v := n.(type) {
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
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Reading clauses
// ─────────────────────────────────────────────────────────────────────────────
// Individual reading-clause handlers (matchClause, optionalMatchClause,
// unwindClause, callClause, withClause) are invoked by [analyser.dispatchClause]
// after [orderClauses] sorts every clause by source position.

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
	// `WITH *` (Projection.All) preserves every binding currently in scope
	// — it is a pass-through projection. We must not reset the scope, only
	// validate the WHERE predicate (and any explicitly listed Items).
	// The openCypher grammar does not allow mixing `*` and explicit items,
	// but we tolerate it defensively so a future grammar widening would not
	// silently drop bindings.
	if w.Projection.All {
		for _, item := range w.Projection.Items {
			a.checkExprForWith(item.Expr)
		}
		if w.Where != nil {
			a.whereClause(w.Where)
		}
		for _, s := range w.Projection.OrderBy {
			a.checkExpr(s.Expr)
		}
		return
	}

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

	// 2. WHERE and ORDER BY on WITH are both evaluated in a scope that
	//    includes BOTH the pre-WITH names AND the projection aliases.
	//    openCypher allows patterns such as
	//
	//        OPTIONAL MATCH (a)-[r:KNOWS]->(c)
	//        WITH c WHERE r IS NULL
	//
	//    where the WHERE filters by a pre-WITH variable, and similarly
	//
	//        MATCH (n) WITH n.name AS name ORDER BY n.age
	//
	//    is invalid because n.age references n which was NOT projected.
	//
	//    Wait — n IS in the pre-WITH scope (MATCH introduced it). The scope
	//    that ORDER BY sees is the same as WHERE: pre-WITH names plus the new
	//    projection aliases. We therefore introduce the alias names BEFORE the
	//    scope reset and validate both WHERE and ORDER BY against the merged
	//    scope. The reset that follows drops the pre-WITH names so subsequent
	//    clauses only see projected ones.
	for _, p := range projs {
		name := projectedName(p.expr, p.alias)
		if name == "" {
			continue
		}
		if _, exists := a.scope.LookupLocal(name); exists {
			// Alias collides with a pre-existing name (e.g. WITH n AS n):
			// no introduction is needed; the symbol is still in scope.
			continue
		}
		// We intentionally ignore redeclaration here — the post-reset block
		// below records the canonical introduction (and its error, if any).
		_ = a.scope.Define(name, p.pos, "any")
	}

	if w.Where != nil {
		a.whereClause(w.Where)
	}
	// ORDER BY sees the same merged scope as WHERE (pre-WITH names + aliases).
	// Any variable reference not present in this merged scope is undefined.
	for _, s := range w.Projection.OrderBy {
		a.checkExpr(s.Expr)
	}
	// InvalidAggregation: an ORDER BY item containing an aggregation
	// function is only legal when the projection itself contains an
	// aggregation. Otherwise the aggregation has no group to fold over.
	a.checkOrderByAggregation(w.Projection)

	// 3. Reset scope: only projected names survive.
	a.scope.reset()

	for _, p := range projs {
		name := projectedName(p.expr, p.alias)
		if name == "" {
			// Non-nameable projection (e.g. a literal): skip.
			continue
		}
		a.error(a.scope.Define(name, p.pos, inferProjectedType(p.expr)))
	}
}

// inferProjectedType returns a coarse static type for a WITH/RETURN
// projection expression so a subsequent pattern introduction
// (`MATCH (n)`, `MATCH (a)-[r]->(b)`) can detect a type conflict when
// the alias was previously bound to a non-graph-element value.
//
// Recognised types: "node", "relationship", "path", "value" (any
// non-graph literal / scalar / list / map) and "any" (unknown).
// Variable references propagate the existing scope type.
func inferProjectedType(e ast.Expression) string {
	switch v := e.(type) {
	case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral,
		*ast.BoolLiteral, *ast.ListLiteral, *ast.MapLiteral:
		_ = v
		return "value"
	case *ast.NullLiteral:
		// NULL is a wildcard; do not constrain downstream pattern use.
		return "any"
	}
	return "any"
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
// Individual updating-clause handlers are invoked by [analyser.dispatchClause]
// after [orderClauses] sorts every clause by source position.

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
		a.checkDeleteExpr(e)
	}
}

func (a *analyser) detachDeleteClause(d *ast.DetachDelete) {
	for _, e := range d.Expressions {
		a.checkExpr(e)
		a.checkDeleteExpr(e)
	}
}

// checkDeleteExpr verifies that a DELETE / DETACH DELETE expression is a
// node, relationship, or path. Direct non-graph literals and arithmetic
// expressions raise InvalidArgumentType at compile time. Variable
// receivers are not type-narrowed at this point (their static type may
// be "any") to avoid false positives on graph variables that came from
// a function call or other dynamic source.
func (a *analyser) checkDeleteExpr(e ast.Expression) {
	switch e.(type) {
	case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral,
		*ast.BoolLiteral, *ast.ListLiteral, *ast.MapLiteral,
		*ast.BinaryOp, *ast.UnaryOp:
		a.error(invalidBooleanOperandError("DELETE", "non-graph", positionOf(e)))
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
	// Path variable (p = (a)-[r]->(b)) — introduce into scope. Re-using a
	// previously-bound name as a path raises VariableTypeConflict unless the
	// existing binding is also a path (which it cannot be in practice — path
	// vars are unique per query — but the check is symmetric for safety).
	if pp.Variable != nil {
		a.checkTypeConflict(*pp.Variable, "path", pp.Pos)
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
	// If already defined, it is a re-use (bound node) — provided the
	// existing binding is also a node. Re-using a relationship or path
	// variable as a node is a VariableTypeConflict.
	if sym, ok := a.scope.Lookup(name); ok {
		if conflictsWith(sym.Type, "node") {
			a.error(redeclarationError(name, np.Pos))
		}
		return
	}
	a.error(a.scope.Define(name, np.Pos, "node"))
}

func (a *analyser) relPatternIntroduce(rp *ast.RelationshipPattern) {
	if rp.Variable == nil {
		return
	}
	name := *rp.Variable
	// Re-using an existing name as a relationship: must be a relationship
	// already, otherwise VariableTypeConflict.
	if sym, ok := a.scope.Lookup(name); ok {
		if conflictsWith(sym.Type, "relationship") {
			a.error(redeclarationError(name, rp.Pos))
		}
		return
	}
	a.error(a.scope.Define(name, rp.Pos, "relationship"))
}

// pathPatternRefCheck walks a path pattern in pure-reference mode: every
// named node and relationship variable must already be in scope, otherwise
// KindUndefinedVar is reported. Used for bare WHERE pattern predicates
// (existential checks) where openCypher forbids variable introduction.
func (a *analyser) pathPatternRefCheck(pp *ast.PathPattern) {
	if pp == nil {
		return
	}
	if pp.Variable != nil {
		if _, ok := a.scope.Lookup(*pp.Variable); !ok {
			a.error(undefinedVarError(*pp.Variable, pp.Pos))
		}
	}
	for el := pp.Head; el != nil; el = el.Next {
		if el.Node != nil && el.Node.Variable != nil {
			if _, ok := a.scope.Lookup(*el.Node.Variable); !ok {
				a.error(undefinedVarError(*el.Node.Variable, el.Node.Pos))
			}
		}
		if el.Relationship != nil && el.Relationship.Variable != nil {
			if _, ok := a.scope.Lookup(*el.Relationship.Variable); !ok {
				a.error(undefinedVarError(*el.Relationship.Variable, el.Relationship.Pos))
			}
		}
	}
}

// conflictsWith reports whether an existing symbol of kind have can be
// safely bound a second time as kind want. "any" tolerates either side
// (used for projection aliases and YIELD items where the static type is
// unknown). Identical kinds never conflict.
func conflictsWith(have, want string) bool {
	if have == want || have == "" || have == "any" || want == "any" {
		return false
	}
	return true
}

// checkTypeConflict records a redeclaration error when name is already in
// scope with a static type incompatible with kind. Used by introducers that
// otherwise unconditionally call [Scope.Define] (notably path patterns).
func (a *analyser) checkTypeConflict(name, kind string, pos ast.Position) {
	if sym, ok := a.scope.Lookup(name); ok {
		if conflictsWith(sym.Type, kind) {
			a.error(redeclarationError(name, pos))
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Projection check (RETURN / WITH source-expression validation)
// ─────────────────────────────────────────────────────────────────────────────

// projectionCheck validates that all expressions in a RETURN projection
// reference only variables that are in scope.
//
// ORDER BY, SKIP, and LIMIT see the projected aliases in addition to every
// pre-projection binding, so the alias names are introduced into the
// current scope before those clauses are validated. The scope mutation
// stays local to projectionCheck — it does not leak to subsequent clauses
// because RETURN is the terminal clause of a single query.
func (a *analyser) projectionCheck(proj *ast.Projection) {
	if proj.All {
		return // RETURN * — accept everything that is in scope
	}
	for _, item := range proj.Items {
		a.checkExpr(item.Expr)
	}
	// Introduce projected aliases (and bare-Variable projections) so that
	// ORDER BY / SKIP / LIMIT references resolve. Redeclaration errors are
	// suppressed here because the alias often shadows a pre-existing name
	// (e.g. `RETURN n.id AS n`).
	for _, item := range proj.Items {
		name := projectedName(item.Expr, item.Alias)
		if name == "" {
			continue
		}
		if _, exists := a.scope.LookupLocal(name); exists {
			continue
		}
		_ = a.scope.Define(name, item.Pos, "any")
	}
	for _, s := range proj.OrderBy {
		a.checkExpr(s.Expr)
	}
	a.checkOrderByAggregation(proj)
	if proj.Skip != nil {
		errsBefore := len(a.errs)
		a.checkExpr(proj.Skip)
		// Only flag the non-constant error when checkExpr did not already
		// report something for this expression (e.g. UndefinedVariable on
		// an unbound reference): avoids double-reporting the same site.
		if len(a.errs) == errsBefore && hasVariableReference(proj.Skip) {
			a.error(invalidBooleanOperandError("SKIP", "non-constant", positionOf(proj.Skip)))
		}
	}
	if proj.Limit != nil {
		errsBefore := len(a.errs)
		a.checkExpr(proj.Limit)
		if len(a.errs) == errsBefore && hasVariableReference(proj.Limit) {
			a.error(invalidBooleanOperandError("LIMIT", "non-constant", positionOf(proj.Limit)))
		}
	}
}

// hasVariableReference reports whether e (or any sub-expression) refers
// to a variable. Used to detect non-constant SKIP / LIMIT expressions
// per openCypher: both clauses must be constant at compile time.
// Parameters ($x) are NOT considered variable references — they bind at
// query-parameter time, before execution, and openCypher classifies
// them as constants for SKIP/LIMIT purposes.
func hasVariableReference(e ast.Expression) bool {
	if e == nil {
		return false
	}
	switch v := e.(type) {
	case *ast.Variable:
		return true
	case *ast.Property:
		return true
	case *ast.BinaryOp:
		return hasVariableReference(v.Left) || hasVariableReference(v.Right)
	case *ast.UnaryOp:
		return hasVariableReference(v.Operand)
	case *ast.FunctionInvocation:
		for _, a := range v.Args {
			if hasVariableReference(a) {
				return true
			}
		}
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			if hasVariableReference(el) {
				return true
			}
		}
	case *ast.MapLiteral:
		for _, val := range v.Values {
			if hasVariableReference(val) {
				return true
			}
		}
	}
	return false
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
		// Compile-time type check: property access on a direct
		// StringLiteral / ListLiteral / MapLiteral receiver is
		// statically invalid (RETURN 'str'.foo, RETURN [].foo, …).
		// IntLiteral and FloatLiteral receivers are intentionally NOT
		// flagged here — the parser reconstructs float literals like
		// `1.0` from an IntLiteral atom followed by a numeric "Name"
		// accessor, but very long floats may slip through that
		// reconstruction and reach sema as IntLiteral.someDigits, which
		// is a valid (round-trip-tolerant) float literal in the source
		// rather than a property access.
		switch v.Receiver.(type) {
		case *ast.StringLiteral, *ast.ListLiteral, *ast.MapLiteral, *ast.BoolLiteral:
			a.error(invalidBooleanOperandError(".", "non-graph", v.Pos))
		}

	case *ast.LabelPredicate:
		a.checkExpr(v.Receiver)

	case *ast.BinaryOp:
		if isLogicalOperator(v.Operator) {
			if kind, bad := nonBooleanLiteralKind(v.Left); bad {
				a.error(invalidBooleanOperandError(v.Operator, kind, v.Pos))
			}
			if kind, bad := nonBooleanLiteralKind(v.Right); bad {
				a.error(invalidBooleanOperandError(v.Operator, kind, v.Pos))
			}
		}
		// The IN operator requires a List on the right; a literal of any
		// other type is a static type error per openCypher.
		if strings.ToUpper(v.Operator) == "IN" {
			if kind, bad := nonListLiteralKind(v.Right); bad {
				a.error(invalidBooleanOperandError(v.Operator, kind, v.Pos))
			}
		}
		a.checkExpr(v.Left)
		a.checkExpr(v.Right)

	case *ast.UnaryOp:
		if isLogicalOperator(v.Operator) {
			if kind, bad := nonBooleanLiteralKind(v.Operand); bad {
				a.error(invalidBooleanOperandError(v.Operator, kind, v.Pos))
			}
		}
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
		// PathPattern in expression context (a bare pattern predicate in
		// WHERE, e.g. `WHERE (a)-[r]->(b)`). Per openCypher, a bare
		// pattern predicate may NOT introduce new variables — every node
		// and relationship variable must already be in scope. We only
		// check references; we never call Define.
		a.pathPatternRefCheck(v)

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

// isLogicalOperator returns true for the openCypher logical operators whose
// operands must be Boolean (or NULL). The comparison is case-insensitive
// because the parser may surface either casing.
func isLogicalOperator(op string) bool {
	switch strings.ToUpper(op) {
	case "AND", "OR", "XOR", "NOT":
		return true
	}
	return false
}

// nonBooleanLiteralKind classifies e as a literal expression of a known
// non-boolean type. It returns (kindString, true) for IntLiteral,
// FloatLiteral, StringLiteral, ListLiteral, and MapLiteral; ("", false) for
// BoolLiteral, NullLiteral, variables, parameters, function calls, and any
// other expression whose type is not statically constrained to non-boolean.
//
// The classification is conservative: only direct literals are flagged so
// that valid dynamic expressions like `x AND y` (where x/y may be booleans
// at runtime) remain unchecked. The TCK Boolean1-4 test family asserts the
// compile-time error only for literal operands; complex expressions like
// `(1+2) AND true` are out of scope here.
func nonBooleanLiteralKind(e ast.Expression) (string, bool) {
	switch e.(type) {
	case *ast.IntLiteral:
		return "Integer", true
	case *ast.FloatLiteral:
		return "Float", true
	case *ast.StringLiteral:
		return "String", true
	case *ast.ListLiteral:
		return "List", true
	case *ast.MapLiteral:
		return "Map", true
	}
	return "", false
}

// nonListLiteralKind classifies e as a literal expression of a known
// non-list type. It returns (kindString, true) for IntLiteral,
// FloatLiteral, StringLiteral, BoolLiteral, and MapLiteral. Returns
// ("", false) for ListLiteral, NullLiteral, variables, parameters,
// function calls, and any other expression whose type is not statically
// constrained to non-list.
func nonListLiteralKind(e ast.Expression) (string, bool) {
	switch e.(type) {
	case *ast.IntLiteral:
		return "Integer", true
	case *ast.FloatLiteral:
		return "Float", true
	case *ast.StringLiteral:
		return "String", true
	case *ast.BoolLiteral:
		return "Boolean", true
	case *ast.MapLiteral:
		return "Map", true
	}
	return "", false
}

// checkOrderByAggregation flags ORDER BY items that contain aggregation
// function calls when the surrounding projection does not aggregate
// itself. The openCypher rule: an aggregation in ORDER BY collapses
// rows to a group; the group must come from the projection.
func (a *analyser) checkOrderByAggregation(proj *ast.Projection) {
	if proj == nil || len(proj.OrderBy) == 0 {
		return
	}
	// If the projection itself contains an aggregation, ORDER BY may
	// reference aggregations freely (they fold over the same groups).
	for _, item := range proj.Items {
		if containsAggregation(item.Expr) {
			return
		}
	}
	for _, s := range proj.OrderBy {
		if containsAggregation(s.Expr) {
			a.error(invalidAggregationError(positionOf(s.Expr)))
			// Report once per ORDER BY item; do not flood with the
			// same error if the projection has none.
		}
	}
}

// positionOf returns the [ast.Position] of an expression by best effort.
// Expressions without a Pos field fall back to the zero Position.
func positionOf(e ast.Expression) ast.Position {
	switch v := e.(type) {
	case *ast.Variable:
		return v.Pos
	case *ast.Property:
		return v.Pos
	case *ast.FunctionInvocation:
		return v.Pos
	case *ast.BinaryOp:
		return v.Pos
	case *ast.UnaryOp:
		return v.Pos
	case *ast.IntLiteral:
		return v.Pos
	case *ast.FloatLiteral:
		return v.Pos
	case *ast.StringLiteral:
		return v.Pos
	case *ast.BoolLiteral:
		return v.Pos
	case *ast.NullLiteral:
		return v.Pos
	case *ast.ListLiteral:
		return v.Pos
	case *ast.MapLiteral:
		return v.Pos
	case *ast.LabelPredicate:
		return v.Pos
	case *ast.CaseExpression:
		return v.Pos
	case *ast.ListComprehension:
		return v.Pos
	case *ast.PatternComprehension:
		return v.Pos
	case *ast.MapProjection:
		return v.Pos
	case *ast.SubscriptExpr:
		return v.Pos
	case *ast.SliceExpr:
		return v.Pos
	case *ast.ExistsSubquery:
		return v.Pos
	case *ast.CountSubquery:
		return v.Pos
	}
	return ast.Position{}
}

// containsAggregation reports whether e (or any sub-expression) calls one
// of the openCypher aggregation functions. The classifier is a case-
// insensitive name match against a small canonical set; user-defined
// aggregations are out of scope for this static check.
func containsAggregation(e ast.Expression) bool {
	if e == nil {
		return false
	}
	switch v := e.(type) {
	case *ast.FunctionInvocation:
		name := strings.ToLower(v.Name)
		// FunctionInvocation may be namespaced (e.g. apoc.coll.sum); the
		// canonical aggregations are unqualified.
		if len(v.Namespace) == 0 {
			switch name {
			case "count", "sum", "avg", "min", "max", "collect",
				"stdev", "stdevp", "percentilecont", "percentiledisc":
				return true
			}
		}
		for _, arg := range v.Args {
			if containsAggregation(arg) {
				return true
			}
		}
	case *ast.BinaryOp:
		return containsAggregation(v.Left) || containsAggregation(v.Right)
	case *ast.UnaryOp:
		return containsAggregation(v.Operand)
	case *ast.Property:
		return containsAggregation(v.Receiver)
	case *ast.LabelPredicate:
		return containsAggregation(v.Receiver)
	case *ast.SubscriptExpr:
		return containsAggregation(v.Expr) || containsAggregation(v.Index)
	case *ast.SliceExpr:
		return containsAggregation(v.Expr) || containsAggregation(v.From) || containsAggregation(v.To)
	case *ast.CaseExpression:
		if containsAggregation(v.Subject) {
			return true
		}
		for _, alt := range v.Alternatives {
			if containsAggregation(alt.Condition) || containsAggregation(alt.Consequent) {
				return true
			}
		}
		return containsAggregation(v.ElseExpr)
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			if containsAggregation(el) {
				return true
			}
		}
	case *ast.MapLiteral:
		for _, val := range v.Values {
			if containsAggregation(val) {
				return true
			}
		}
	}
	return false
}

// checkFunctionArgTypes performs a coarse static type-check on a handful
// of graph-built-in functions whose argument kind is constrained by the
// openCypher spec. Only fires when the argument is a Variable whose scope
// type is definitively incompatible — Variables of type "any" and complex
// expressions (function calls, property access, etc.) remain unchecked.
//
// Checks:
//   - type(x)         requires x to be a relationship
//   - labels(x), keys(x) requires x to be a node (keys also accepts relationship)
//   - nodes(p), relationships(p) requires p to be a path
//
// The first failing argument surfaces InvalidArgumentType; subsequent
// arguments are not re-reported for the same invocation.
func (a *analyser) checkFunctionArgTypes(fn *ast.FunctionInvocation) {
	if fn == nil || len(fn.Args) == 0 || len(fn.Namespace) > 0 {
		return
	}
	name := strings.ToLower(fn.Name)
	type argTypeReq struct {
		argIdx int
		ok     map[string]bool
	}
	var req argTypeReq
	switch name {
	case "type":
		req = argTypeReq{argIdx: 0, ok: map[string]bool{"relationship": true, "any": true, "": true}}
	case "labels":
		req = argTypeReq{argIdx: 0, ok: map[string]bool{"node": true, "any": true, "": true}}
	case "keys":
		req = argTypeReq{argIdx: 0, ok: map[string]bool{"node": true, "relationship": true, "any": true, "": true}}
	case "nodes", "relationships":
		req = argTypeReq{argIdx: 0, ok: map[string]bool{"path": true, "any": true, "": true}}
	default:
		return
	}
	if req.argIdx >= len(fn.Args) {
		return
	}
	v, ok := fn.Args[req.argIdx].(*ast.Variable)
	if !ok {
		return
	}
	sym, exists := a.scope.Lookup(v.Name)
	if !exists {
		return
	}
	if req.ok[sym.Type] {
		return
	}
	a.error(invalidBooleanOperandError(name, sym.Type, v.Pos))
}
