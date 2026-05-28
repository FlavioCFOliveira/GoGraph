package sema

import (
	"sort"
	"strings"

	"gograph/cypher/ast"
)

// IsKnownFunction, when non-nil, is consulted by [Analyse] to decide
// whether a scalar function-call expression refers to a known function.
// The argument is the lower-cased qualified name (namespace components
// joined to the function name with '.', e.g. "duration.between").
//
// The hook is intentionally a package-level variable rather than an
// argument so existing call sites do not need to change. It is set by
// cypher/api.init() to a closure that consults the engine's function
// registry; sema fails closed (no UnknownFunction reports) when the
// hook is nil, preserving the pre-hook behaviour.
//
//nolint:gochecknoglobals // hook for cross-package wiring; set once at init
var IsKnownFunction func(qualifiedLowerName string) bool

// knownAggregates is the closed set of aggregate function names that
// sema accepts even when [IsKnownFunction] returns false. The set
// mirrors cypher/ir/aggregation.aggFunctions but is duplicated here to
// avoid an import cycle (sema is upstream of ir).
var knownAggregates = map[string]bool{
	"count":          true,
	"sum":            true,
	"avg":            true,
	"min":            true,
	"max":            true,
	"collect":        true,
	"stdev":          true,
	"stdevp":         true,
	"percentilecont": true,
	"percentiledisc": true,
}

// knownQuantifiers is the set of names that look like function calls
// in the parser but are actually openCypher quantifier predicates over
// a list (`all(x IN xs WHERE p)`, `any(...)`, `none(...)`,
// `single(...)`) or existential subqueries (`exists { ... }`). They
// must not be flagged as unknown by the function-name check.
var knownQuantifiers = map[string]bool{
	"all":    true,
	"any":    true,
	"none":   true,
	"single": true,
	"exists": true,
}

// isKnownFunctionName reports whether a function-call name is acceptable
// at sema time. Aggregates and the IsKnownFunction hook are consulted
// in that order; an unset hook short-circuits to "known" so sema does
// not report false positives when the engine wiring is incomplete.
func isKnownFunctionName(qualifiedLower string) bool {
	if knownAggregates[qualifiedLower] {
		return true
	}
	if knownQuantifiers[qualifiedLower] {
		return true
	}
	if IsKnownFunction == nil {
		return true
	}
	return IsKnownFunction(qualifiedLower)
}

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
	a.checkPatternParameterProps(m.Pattern)
}

func (a *analyser) optionalMatchClause(m *ast.OptionalMatch) {
	a.patternIntroduce(m.Pattern)
	if m.Where != nil {
		a.whereClause(m.Where)
	}
	a.checkPatternParameterProps(m.Pattern)
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
		a.checkProjectionSkipLimit("SKIP", w.Projection.Skip)
		a.checkProjectionSkipLimit("LIMIT", w.Projection.Limit)
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
		// openCypher 9 §5.1.2: every WITH item must be either a bare
		// Variable (whose name becomes the projected name) or aliased
		// via AS. A complex expression without an alias has no
		// downstream name and must be rejected at compile time.
		if item.Alias == nil {
			if _, isVar := item.Expr.(*ast.Variable); !isVar {
				a.error(noExpressionAliasError(item.Pos))
			}
		}
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
	// ORDER BY sees the merged scope (pre-WITH names + aliases).
	for _, s := range w.Projection.OrderBy {
		a.checkExpr(s.Expr)
	}
	// When the projection aggregates, the post-WITH row only contains
	// projected aliases, so an ORDER BY reference to a non-projected
	// pre-WITH variable is undefined per openCypher 9 §3.3.5. Flag
	// those cases without disturbing the merged-scope check above.
	projAggregates := false
	for _, p := range projs {
		if containsAggregation(p.expr) {
			projAggregates = true
			break
		}
	}
	if projAggregates {
		projected := map[string]struct{}{}
		projectedExprs := map[string]struct{}{}
		for _, p := range projs {
			if name := projectedName(p.expr, p.alias); name != "" {
				projected[name] = struct{}{}
			}
			if p.expr != nil {
				projectedExprs[p.expr.String()] = struct{}{}
			}
		}
		for _, s := range w.Projection.OrderBy {
			// In an aggregated projection, the post-WITH row only carries
			// the projected aliases plus the values of projected aggregate
			// calls. The ORDER BY may compose them freely with literals and
			// scalar function calls, but a reference to a pre-WITH variable
			// that is not itself projected and does not sit inside a
			// projected aggregate is undefined per openCypher 9 §3.3.5.
			for _, v := range collectFreeVarsOutsideProjectedAggs(s.Expr, projectedExprs) {
				if _, ok := projected[v]; ok {
					continue
				}
				a.error(undefinedVarError(v, positionOf(s.Expr)))
				break
			}
		}
	}
	// InvalidAggregation: an ORDER BY item containing an aggregation
	// function is only legal when the projection itself contains an
	// aggregation. Otherwise the aggregation has no group to fold over.
	a.checkOrderByAggregation(w.Projection)
	a.checkAmbiguousAggregation(w.Projection)

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
	a.checkProjectionSkipLimit("SKIP", w.Projection.Skip)
	a.checkProjectionSkipLimit("LIMIT", w.Projection.Limit)
}

// inferListElementType returns a coarse element-type tag for a ListLiteral
// when the list is homogeneously typed across a primitive literal kind, or
// "" when the list is empty, mixed, or carries non-literal elements. Used by
// the quantifier predicate check to surface
// `none(x IN ['a'] WHERE x % 2 = 0)`-style type mismatches at compile time.
func inferListElementType(e ast.Expression) string {
	ll, ok := e.(*ast.ListLiteral)
	if !ok || ll == nil || len(ll.Elements) == 0 {
		return ""
	}
	var kind string
	for _, el := range ll.Elements {
		var k string
		switch el.(type) {
		case *ast.IntLiteral:
			k = "integer"
		case *ast.FloatLiteral:
			k = "float"
		case *ast.StringLiteral:
			k = "string"
		case *ast.BoolLiteral:
			k = "boolean"
		case *ast.NullLiteral:
			// Null does not constrain the element kind.
			continue
		default:
			return ""
		}
		if kind == "" {
			kind = k
		} else if kind != k {
			// Mix integer/float as "number".
			if (kind == "integer" && k == "float") || (kind == "float" && k == "integer") {
				kind = "number"
				continue
			}
			return ""
		}
	}
	if kind == "integer" || kind == "float" {
		return "number"
	}
	return kind
}

// checkQuantifierPredicateTypes validates that the predicate of a
// quantifier list comprehension is compatible with the source list's
// element type. The check is intentionally narrow — it only fires when
// the source is a homogeneously-typed ListLiteral and the predicate uses
// an arithmetic operator (% / / * + -) on the bound variable. The
// arithmetic family requires Number; encountering it with a String /
// Boolean element kind raises InvalidArgumentType per openCypher
// TCK Quantifier1-4.
func (a *analyser) checkQuantifierPredicateTypes(lc *ast.ListComprehension, pos ast.Position) {
	if lc == nil || lc.Predicate == nil {
		return
	}
	elemKind := inferListElementType(lc.Source)
	if elemKind == "" || elemKind == "number" {
		// Unknown or numeric: arithmetic is fine; no check fires.
		return
	}
	// elemKind ∈ {"string", "boolean"} — arithmetic on the bound var is
	// statically invalid.
	if quantifierPredicateUsesArithOn(lc.Predicate, lc.Variable) {
		a.error(invalidBooleanOperandError(".", "non-number", pos))
	}
}

// quantifierPredicateUsesArithOn reports whether e contains a binary
// arithmetic operator (% / / * + -) where one operand references the
// loop-bound variable named varName. The walker descends into BinaryOp,
// UnaryOp, FunctionInvocation, list/map literals, and case expressions so
// the typical TCK predicates `x % 2 = 0`, `x + 1 > 0`, `x * 2 = 4` are all
// caught. String concatenation via "+" is intentionally NOT excluded — if
// the list element kind is "string" we still flag `x + 'suffix'` because
// the TCK examples only exercise arithmetic; a future refinement could
// permit the string-concat case.
func quantifierPredicateUsesArithOn(e ast.Expression, varName string) bool { //nolint:gocyclo // structural walker
	if e == nil {
		return false
	}
	switch n := e.(type) {
	case *ast.BinaryOp:
		switch n.Operator {
		case "%", "/", "*", "+", "-", "^":
			if isVarRef(n.Left, varName) || isVarRef(n.Right, varName) {
				return true
			}
		}
		return quantifierPredicateUsesArithOn(n.Left, varName) ||
			quantifierPredicateUsesArithOn(n.Right, varName)
	case *ast.UnaryOp:
		return quantifierPredicateUsesArithOn(n.Operand, varName)
	case *ast.FunctionInvocation:
		for _, arg := range n.Args {
			if quantifierPredicateUsesArithOn(arg, varName) {
				return true
			}
		}
	case *ast.SubscriptExpr:
		return quantifierPredicateUsesArithOn(n.Expr, varName) ||
			quantifierPredicateUsesArithOn(n.Index, varName)
	case *ast.SliceExpr:
		return quantifierPredicateUsesArithOn(n.Expr, varName) ||
			quantifierPredicateUsesArithOn(n.From, varName) ||
			quantifierPredicateUsesArithOn(n.To, varName)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			if quantifierPredicateUsesArithOn(el, varName) {
				return true
			}
		}
	case *ast.MapLiteral:
		for _, val := range n.Values {
			if quantifierPredicateUsesArithOn(val, varName) {
				return true
			}
		}
	case *ast.CaseExpression:
		if quantifierPredicateUsesArithOn(n.Subject, varName) ||
			quantifierPredicateUsesArithOn(n.ElseExpr, varName) {
			return true
		}
		for _, alt := range n.Alternatives {
			if quantifierPredicateUsesArithOn(alt.Condition, varName) ||
				quantifierPredicateUsesArithOn(alt.Consequent, varName) {
				return true
			}
		}
	}
	return false
}

// isVarRef reports whether e is a Variable referencing the named binding.
func isVarRef(e ast.Expression, name string) bool {
	v, ok := e.(*ast.Variable)
	return ok && v.Name == name
}

// inferProjectedType returns a coarse static type for a WITH/RETURN
// projection expression so a subsequent pattern introduction
// (`MATCH (n)`, `MATCH (a)-[r]->(b)`) can detect a type conflict when
// the alias was previously bound to a non-graph-element value.
//
// Recognised types: "node", "relationship", "path", "map" (literal map
// expression, supports property access at runtime), "list", "scalar"
// (string / int / float / bool — no property access), "value" (general
// non-graph literal — used by [conflictsWith] as a catch-all that does
// not conflict with anything), and "any" (unknown). Variable references
// would propagate the existing scope type but this function currently
// inspects only the immediate AST node and returns "any" for them.
func inferProjectedType(e ast.Expression) string {
	switch e.(type) {
	case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral, *ast.BoolLiteral:
		return "scalar"
	case *ast.ListLiteral:
		return "list"
	case *ast.MapLiteral:
		return "map"
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
	// openCypher 9 §3.7: aggregation function calls inside procedure CALL
	// argument expressions are illegal — the procedure dispatcher has no
	// row group over which to fold the aggregator.
	for _, arg := range c.Args {
		if containsAggregation(arg) {
			a.error(invalidAggregationError(positionOf(arg)))
		}
	}
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
	// Detect bound-node augmentation BEFORE patternIntroduce defines the new
	// occurrences in the scope. A node pattern whose variable is already in
	// scope AND declares new labels or properties is rejected as
	// VariableAlreadyBound.
	a.checkCreateNoRebind(c.Pattern)
	// CREATE introduces new variables from the pattern.
	a.patternIntroduce(c.Pattern)
	// CREATE-specific relationship checks: every relationship pattern
	// must have exactly one type and an explicit direction. Zero types,
	// union types, variable-length and undirected relationships are all
	// rejected.
	a.checkCreateRelationshipTypes(c.Pattern, true)
	// Validate any property expressions on node or relationship patterns —
	// in particular flag undefined-variable references (e.g.
	// CREATE (b {name: missing}) where `missing` is not in scope).
	a.checkPatternPropertyExprs(c.Pattern)
}

// checkMergeNoRebind is the MERGE counterpart of checkCreateNoRebind. Two
// situations are rejected as VariableAlreadyBound per openCypher 9 §3.5.2:
//
//  1. A standalone bound node as the whole pattern (MERGE (a) where a is
//     already bound is a no-op the spec forbids).
//  2. A node pattern that references an already-bound variable while
//     adding new label predicates or a property map. The bound entity
//     already has its labels and properties set; MERGE may not impose
//     additional ones.
//
// Reusing a bound variable as an endpoint of a relationship in MERGE without
// new attributes is legal — that is how MERGE attaches new relationships to
// existing nodes.
func (a *analyser) checkMergeNoRebind(pp *ast.PathPattern) {
	if pp == nil {
		return
	}
	standalone := pp.Head != nil && pp.Head.Next == nil && pp.Head.Node != nil
	if standalone {
		np := pp.Head.Node
		if np.Variable == nil {
			return
		}
		name := *np.Variable
		if _, alreadyInScope := a.scope.Lookup(name); alreadyInScope {
			a.error(variableAlreadyBoundError(name, np.Pos))
			return
		}
	}
	// Walk every node pattern in the path; flag any bound-variable
	// re-use that adds new labels or properties. Track within-pattern
	// occurrences so the second `(a:Bar)` in `(a)-[r:KNOWS]->(a:Bar)`
	// also trips the check.
	seen := map[string]struct{}{}
	el := pp.Head
	for el != nil {
		if np := el.Node; np != nil && np.Variable != nil {
			name := *np.Variable
			_, alreadyInScope := a.scope.Lookup(name)
			_, alreadyInPattern := seen[name]
			bound := alreadyInScope || alreadyInPattern
			hasNewAttrs := len(np.Labels) > 0 || np.Properties != nil
			if bound && hasNewAttrs {
				a.error(variableAlreadyBoundError(name, np.Pos))
			}
			seen[name] = struct{}{}
		}
		el = el.Next
	}
}

// checkPatternPropertyExprs walks every node and relationship pattern in pat
// and runs the standard expression checker over any inline property map. The
// pattern-variable scope is unchanged — this only surfaces UndefinedVariable
// and the other diagnostics produced by [checkExpr] for property values.
func (a *analyser) checkPatternPropertyExprs(pat *ast.Pattern) {
	if pat == nil {
		return
	}
	for _, pp := range pat.Paths {
		el := pp.Head
		for el != nil {
			if np := el.Node; np != nil && np.Properties != nil {
				a.checkExpr(np.Properties)
			}
			if rp := el.Relationship; rp != nil && rp.Properties != nil {
				a.checkExpr(rp.Properties)
			}
			el = el.Next
		}
	}
}

// checkCreateNoRebind walks every node pattern in pat and reports a
// KindVariableAlreadyBound error in two situations:
//
//  1. A node pattern whose variable is already bound (by an earlier
//     clause or by an earlier occurrence in this same pattern) carries
//     new labels or properties. The bound entity already has its label
//     and property set; CREATE may not augment them.
//  2. A single-node path pattern (no relationships in the path) refers
//     to an already bound variable, with or without new attributes —
//     this is a no-op that openCypher 9 §3.5.1 still rejects as
//     "Cannot create a node that is already bound".
//
// Reusing a bound variable as an endpoint of a relationship in CREATE
// is legal, provided no new labels or properties are declared on that
// node — that is how CREATE attaches new relationships to existing
// nodes.
func (a *analyser) checkCreateNoRebind(pat *ast.Pattern) {
	if pat == nil {
		return
	}
	seen := map[string]struct{}{}
	for _, pp := range pat.Paths {
		// A path is "standalone-node" when its head has no Next link —
		// i.e. the entire path is just a single node with no relationship.
		standalone := pp.Head != nil && pp.Head.Next == nil && pp.Head.Node != nil
		el := pp.Head
		for el != nil {
			if np := el.Node; np != nil && np.Variable != nil {
				name := *np.Variable
				_, alreadyInScope := a.scope.Lookup(name)
				_, alreadyInPattern := seen[name]
				bound := alreadyInScope || alreadyInPattern
				hasNewAttrs := len(np.Labels) > 0 || np.Properties != nil
				if bound && (hasNewAttrs || standalone) {
					a.error(variableAlreadyBoundError(name, np.Pos))
				}
				seen[name] = struct{}{}
			}
			el = el.Next
		}
	}
}

func (a *analyser) mergeClause(m *ast.Merge) {
	// MERGE follows the same bound-node-augmentation rule as CREATE: a
	// standalone bound node in the MERGE pattern is illegal (openCypher 9
	// §3.5.2 forbids MERGE on a variable that is already bound).
	a.checkMergeNoRebind(m.Pattern)
	// MERGE may introduce new variables or reuse existing ones.
	a.pathPatternIntroduce(m.Pattern)
	// MERGE relationship-type rule: similar to CREATE but undirected
	// relationships are allowed (MERGE matches either direction, and
	// creates outgoing when no match exists).
	a.checkPathPatternRelTypes(m.Pattern, false)
	a.checkPathPatternParameterProps(m.Pattern)
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
		*ast.BinaryOp, *ast.UnaryOp, *ast.LabelPredicate:
		// LabelPredicate captures the `n:Foo` shape — `DELETE n:Foo` is
		// asking the engine to delete a label, which is not a valid
		// DELETE target. The proper form is `REMOVE n:Foo`.
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
	// openCypher 9 §3.4.5: aggregations are forbidden in WHERE because
	// the filter applies per row, before any grouping; allowing
	// count/sum/… here would be ambiguous (the aggregator has no group
	// to fold over).
	if containsAggregation(w.Predicate) {
		a.error(invalidAggregationError(positionOf(w.Predicate)))
	}
	// A bare Variable predicate must reference a Boolean-compatible value.
	// When the scope-symbol type proves the reference is a node,
	// relationship, or path the predicate cannot coerce to Boolean and
	// openCypher requires InvalidArgumentType at compile time
	// (Pattern1 [11] `WHERE (n)` where n is bound to a node).
	if v, ok := w.Predicate.(*ast.Variable); ok {
		if sym, ok := a.scope.Lookup(v.Name); ok {
			switch sym.Type {
			case "node", "relationship", "path":
				a.error(invalidBooleanOperandError("WHERE", sym.Type+"-variable", v.Pos))
			}
		}
	}
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
	// relSeen tracks named relationship variables already introduced
	// inside this path. openCypher 9 §3.3.1.2 forbids re-using the same
	// relationship name in a single path (the relationship-uniqueness
	// constraint); we report it here so the error surfaces before
	// type-conflict checks in relPatternIntroduce.
	relSeen := make(map[string]bool)
	el := pp.Head
	for el != nil {
		if el.Node != nil {
			a.nodePatternIntroduce(el.Node)
		}
		if el.Relationship != nil {
			if rv := el.Relationship.Variable; rv != nil {
				if relSeen[*rv] {
					a.error(relationshipUniquenessError(*rv, el.Relationship.Pos))
				}
				relSeen[*rv] = true
			}
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

// validateRelRange flags negative lower or upper bounds on a variable-length
// relationship pattern (`-[r:T*-2..3]->`). Both bounds must be non-negative
// per openCypher 9 §3.2.6, and Min must not exceed Max when both are
// present. Reports the violation as InvalidRelationshipPattern.
func (a *analyser) validateRelRange(rp *ast.RelationshipPattern) {
	if rp == nil || rp.Range == nil {
		return
	}
	if rp.Range.Min != nil && *rp.Range.Min < 0 {
		a.error(invalidBooleanOperandError("range", "negative-lower-bound", rp.Pos))
	}
	if rp.Range.Max != nil && *rp.Range.Max < 0 {
		a.error(invalidBooleanOperandError("range", "negative-upper-bound", rp.Pos))
	}
	// Min > Max is a real-but-rare authoring mistake; we intentionally do
	// NOT flag it here because the parser's normalize-then-abs pipeline
	// (see cypher/parser/normalize.go) collapses originally-negative range
	// bounds back to positive numbers, so a user-written `*-2..1` reaches
	// sema as `*2..1` and would trip a false positive. A separate parser-
	// level fix that preserves the originally-negative sign is required
	// before this check can be re-enabled.
}

func (a *analyser) relPatternIntroduce(rp *ast.RelationshipPattern) {
	a.validateRelRange(rp)
	if rp.Variable == nil {
		return
	}
	name := *rp.Variable
	// Re-using an existing name as a relationship: must be a relationship
	// already, otherwise VariableTypeConflict. Exception: a variable-length
	// relationship pattern (`[rs*]`) accepts a previously-bound "value" or
	// "list" alias as a list-of-relationships constraint — openCypher
	// allows `WITH [r1, r2] AS rs MATCH (a)-[rs*]->(b)` so the var-length
	// match is restricted to the supplied relationship list.
	if sym, ok := a.scope.Lookup(name); ok {
		if rp.Range != nil && (sym.Type == "value" || sym.Type == "list") {
			return
		}
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
		// RETURN * requires at least one in-scope variable, walking
		// through parent scopes (a subquery RETURN * still sees outer
		// bindings via the parent chain). openCypher 9 §3.3.2 rejects
		// star projection with a truly empty scope as
		// NoVariablesInScope.
		if !scopeHasAnyName(a.scope) && len(proj.Items) == 0 {
			a.error(noVariablesInScopeError(proj.Pos))
		}
		return
	}
	// Reject duplicate output column names (e.g. RETURN 1 AS a, 2 AS a).
	// openCypher 9 §3.3.3 raises ColumnNameConflict at compile time for
	// any projection whose output column list is not unique.
	seenCols := map[string]struct{}{}
	for _, item := range proj.Items {
		name := projectedName(item.Expr, item.Alias)
		if name == "" {
			continue
		}
		if _, dup := seenCols[name]; dup {
			a.error(columnNameConflictError(name, item.Pos))
		}
		seenCols[name] = struct{}{}
	}
	for _, item := range proj.Items {
		// An aggregation function used inside a list / pattern comprehension's
		// projection or predicate has no group to fold over (the comprehension
		// iterates elements lazily) — reject at compile time per openCypher 9
		// §3.7.7 InvalidAggregation.
		if aggInsideComprehension(item.Expr) {
			a.error(invalidAggregationError(positionOf(item.Expr)))
		}
		// openCypher rejects non-deterministic function calls (rand,
		// randomUUID) inside an aggregation argument because their value
		// changes per call, leaving the aggregation result undefined.
		// Surfaces as SyntaxError(NonConstantExpression) per Return6 [15].
		if pos, ok := nondetInsideAggArg(item.Expr); ok {
			a.error(invalidBooleanOperandError("aggregation-argument", "non-constant", pos))
		}
		a.checkExpr(item.Expr)
	}
	a.checkAmbiguousAggregation(proj)
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
	// DISTINCT or aggregation collapses the row identity so that ORDER BY can
	// only reference the projected columns. openCypher 9 §3.3.5 raises
	// UndefinedVariable for an ORDER BY item that references a pre-projection
	// variable that is no longer accessible after the projection.
	projAggregates := false
	for _, item := range proj.Items {
		if containsAggregation(item.Expr) {
			projAggregates = true
			break
		}
	}
	if proj.Distinct || projAggregates {
		projected := map[string]struct{}{}
		projectedExprs := map[string]struct{}{}
		for _, item := range proj.Items {
			if name := projectedName(item.Expr, item.Alias); name != "" {
				projected[name] = struct{}{}
			}
			if item.Expr != nil {
				projectedExprs[item.Expr.String()] = struct{}{}
			}
		}
		for _, s := range proj.OrderBy {
			if projAggregates {
				for _, v := range collectFreeVarsOutsideProjectedAggs(s.Expr, projectedExprs) {
					if _, ok := projected[v]; ok {
						continue
					}
					a.error(undefinedVarError(v, positionOf(s.Expr)))
					break
				}
				continue
			}
			// DISTINCT (no aggregation): the row identity is collapsed to
			// the projected columns. ORDER BY may compose new values from
			// projected sub-expressions, so allow a partial sub-expression
			// match. Variables that are not in projected and never appear
			// as a top-level projected expression are undefined.
			if s.Expr != nil && exprMatchesAnyProjection(s.Expr, projectedExprs) {
				continue
			}
			for _, v := range collectVariables(s.Expr) {
				if _, ok := projected[v]; ok {
					continue
				}
				a.error(undefinedVarError(v, positionOf(s.Expr)))
				break
			}
		}
	}
	a.checkOrderByAggregation(proj)
	a.checkProjectionSkipLimit("SKIP", proj.Skip)
	a.checkProjectionSkipLimit("LIMIT", proj.Limit)
}

// checkProjectionSkipLimit validates the SKIP and LIMIT expressions of
// a projection at compile time. It composes three checks:
//   - generic checkExpr (variable scoping, sub-expression types);
//   - non-constant expressions referencing variables (openCypher 9 §3.6
//     requires SKIP/LIMIT to be constant);
//   - literal-type rules: negative IntLiteral → NegativeIntegerArgument;
//     FloatLiteral → InvalidArgumentType.
//
// Parameters ($x) are deliberately accepted at compile time; they are
// validated at runtime when their values are known.
func (a *analyser) checkProjectionSkipLimit(clause string, e ast.Expression) {
	if e == nil {
		return
	}
	errsBefore := len(a.errs)
	a.checkExpr(e)
	if len(a.errs) == errsBefore && hasVariableReference(e) {
		a.error(invalidBooleanOperandError(clause, "non-constant", positionOf(e)))
	}
	switch v := e.(type) {
	case *ast.IntLiteral:
		if v.Value < 0 {
			a.error(negativeIntegerArgumentError(clause, v.Value, v.Pos))
		}
	case *ast.FloatLiteral:
		a.error(invalidIntegerArgumentError(clause, "FLOAT", v.Pos))
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
		// StringLiteral / ListLiteral / BoolLiteral receiver is
		// statically invalid (`RETURN 'str'.foo`, `RETURN [].foo`,
		// `RETURN true.foo`). MapLiteral is excluded because maps DO
		// admit property access (`RETURN {k: 1}.k`). IntLiteral and
		// FloatLiteral receivers are intentionally NOT flagged here —
		// the parser reconstructs float literals like `1.0` from an
		// IntLiteral atom followed by a numeric "Name" accessor, but
		// very long floats may slip through that reconstruction and
		// reach sema as IntLiteral.someDigits, which is a valid
		// (round-trip-tolerant) float literal in the source rather
		// than a property access.
		switch v.Receiver.(type) {
		case *ast.StringLiteral, *ast.ListLiteral, *ast.BoolLiteral:
			a.error(invalidBooleanOperandError(".", "non-graph", v.Pos))
		}
		// Transitive check: when the receiver is a Variable whose
		// scope symbol carries a static type that cannot have
		// properties (scalar / list / path), reject the access at
		// compile time. Path variables (`p = (a)-[*]->(b)`) carry the
		// path itself, not a map; openCypher requires `p.prop` to
		// raise InvalidArgumentType at compile time (MatchWhere1
		// [14]). Map / node / relationship / any all admit property
		// access (or might at runtime, for "any"), so they pass
		// through.
		if vref, isVar := v.Receiver.(*ast.Variable); isVar {
			if sym, ok := a.scope.Lookup(vref.Name); ok {
				switch sym.Type {
				case "scalar", "list", "path":
					a.error(invalidBooleanOperandError(".", "non-graph", v.Pos))
				}
			}
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
		qualified := v.Name
		if len(v.Namespace) > 0 {
			qualified = strings.Join(append(append([]string{}, v.Namespace...), v.Name), ".")
		}
		if !isKnownFunctionName(strings.ToLower(qualified)) {
			a.error(unknownFunctionError(qualified, v.Pos))
		}
		a.checkFunctionArgTypes(v)
		// Quantifier predicate type check: `any/none/all/single(x IN
		// homogeneous-literal-list WHERE …)` must use the bound variable
		// in operations compatible with the inferred element type.
		// Arithmetic on a String / Boolean / Null list element is the
		// canonical openCypher TCK InvalidArgumentType (Quantifier1-4).
		if len(v.Namespace) == 0 && knownQuantifiers[strings.ToLower(v.Name)] && len(v.Args) == 1 {
			if lc, ok := v.Args[0].(*ast.ListComprehension); ok && lc != nil {
				a.checkQuantifierPredicateTypes(lc, v.Pos)
			}
		}
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
		// A bare node pattern such as `WHERE (n)` is not a valid
		// predicate — openCypher requires at least one relationship in
		// an expression-position path pattern (existential check). Flag
		// it as InvalidArgumentType so Pattern1 [11] surfaces the
		// canonical compile-time error.
		if v.Head != nil && v.Head.Next == nil && v.Head.Relationship == nil {
			a.error(invalidBooleanOperandError("WHERE", "bare-node-pattern", v.Pos))
		}

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

// checkAmbiguousAggregation enforces openCypher 9 §5.3.3: when a
// projection item contains an aggregating sub-expression nested inside
// a larger expression (e.g. `me.age + count(you.age)`), every Variable
// or Property reference that appears OUTSIDE the aggregate call must
// either (a) match a standalone "simple" projection item in the same
// projection (so it is a true grouping key) or (b) be a constant /
// literal / parameter. References that fail the check surface as
// SyntaxError(AmbiguousAggregationExpression).
//
// Pure bare-aggregate items (`count(x)`) skip this check — they have no
// non-aggregate body to scrutinise. Non-aggregating items in an
// aggregating projection ARE the grouping keys and are unconstrained
// here (their own checkExpr enforces scope). The check fires only when
// the projection actually contains at least one aggregate.
func (a *analyser) checkAmbiguousAggregation(proj *ast.Projection) {
	if proj == nil {
		return
	}
	hasAgg := false
	for _, item := range proj.Items {
		if containsAggregation(item.Expr) {
			hasAgg = true
			break
		}
	}
	if !hasAgg {
		return
	}
	// Build the set of "grouping keys". A projection item that is a
	// bare Variable adds the name; a projection item whose Expr is a
	// Property `recv.key` adds the full canonical string. Compound
	// projection items (BinaryOp arithmetic, function calls) are NOT
	// added — per openCypher 9 §5.3.3 the per-leaf rule requires
	// individual references inside an aggregating sibling to each
	// resolve, and a compound projection does not authorise
	// substituting its leaves.
	keys := map[string]struct{}{}
	for _, item := range proj.Items {
		if containsAggregation(item.Expr) {
			continue
		}
		switch n := item.Expr.(type) {
		case *ast.Variable:
			keys[n.Name] = struct{}{}
		case *ast.Property:
			keys[n.String()] = struct{}{}
			if vv, isVar := n.Receiver.(*ast.Variable); isVar {
				keys[vv.Name] = struct{}{}
			}
		}
	}
	for _, item := range proj.Items {
		if !containsAggregation(item.Expr) {
			continue
		}
		// Bare aggregate at top level — no surrounding non-agg body.
		if _, ok := extractAggregation(item.Expr); ok && exprIsBareCall(item.Expr) {
			continue
		}
		// Walk; report the first non-grouping Variable / Property
		// reference that appears outside an aggregate sub-call.
		if pos, name, bad := findUngroupedNonAggRef(item.Expr, keys); bad {
			a.error(ambiguousAggregationError(name, pos))
		}
	}
}

// extractAggregation returns the FunctionInvocation if e is a direct
// aggregate call, otherwise false. Mirrors the IR helper but stays in
// sema so we do not depend on cypher/ir from cypher/sema.
func extractAggregation(e ast.Expression) (*ast.FunctionInvocation, bool) {
	fn, ok := e.(*ast.FunctionInvocation)
	if !ok {
		return nil, false
	}
	if len(fn.Namespace) > 0 {
		return nil, false
	}
	switch strings.ToLower(fn.Name) {
	case "count", "sum", "avg", "min", "max", "collect",
		"stdev", "stdevp", "percentilecont", "percentiledisc":
		return fn, true
	}
	return nil, false
}

// exprIsBareCall reports whether e is a single FunctionInvocation
// (not wrapped in any arithmetic/property/cast).
func exprIsBareCall(e ast.Expression) bool {
	_, ok := e.(*ast.FunctionInvocation)
	return ok
}

// findUngroupedNonAggRef walks e and looks for any Variable or Property
// leaf that (a) does NOT sit inside an aggregate function call and (b)
// does NOT match a grouping key. The first such leaf is returned with
// its position and surface name. Returns bad=false when every non-
// aggregate leaf matches a grouping key.
//
// Per openCypher 9 §5.3.3, the "grouping key" match is per leaf: a
// compound projection item like `me.age + you.age AS grp` is NOT
// sufficient to authorise `me.age + you.age + count(*)` as an
// aggregating sibling, because the rule requires me.age and you.age
// individually to be standalone projection items (or me / you to be
// standalone-projected Variables that authorise their property
// children). The keyExprs set still short-circuits when the whole
// expression-under-walk matches a key, but compound matches DO NOT
// stop descent — leaves are always checked.
func findUngroupedNonAggRef(e ast.Expression, groupingKeys map[string]struct{}) (ast.Position, string, bool) { //nolint:gocyclo
	if e == nil {
		return ast.Position{}, "", false
	}
	switch n := e.(type) {
	case *ast.Variable:
		if _, ok := groupingKeys[n.Name]; ok {
			return ast.Position{}, "", false
		}
		return n.Pos, n.Name, true
	case *ast.Property:
		// `recv.key` matches when the full property string is a key
		// OR when the receiver Variable name is a key (a grouping
		// key on the bare entity authorises all its property
		// children — a node grouped per `n` has a fixed `n.prop` per
		// group).
		if _, ok := groupingKeys[n.String()]; ok {
			return ast.Position{}, "", false
		}
		if vv, isVar := n.Receiver.(*ast.Variable); isVar {
			if _, ok := groupingKeys[vv.Name]; ok {
				return ast.Position{}, "", false
			}
			return vv.Pos, vv.Name, true
		}
		return findUngroupedNonAggRef(n.Receiver, groupingKeys)
	case *ast.BinaryOp:
		if pos, name, bad := findUngroupedNonAggRef(n.Left, groupingKeys); bad {
			return pos, name, true
		}
		return findUngroupedNonAggRef(n.Right, groupingKeys)
	case *ast.UnaryOp:
		return findUngroupedNonAggRef(n.Operand, groupingKeys)
	case *ast.FunctionInvocation:
		if _, ok := extractAggregation(n); ok {
			// Stop descending into aggregate arguments — references
			// inside the aggregate are unrestricted.
			return ast.Position{}, "", false
		}
		for _, arg := range n.Args {
			if pos, name, bad := findUngroupedNonAggRef(arg, groupingKeys); bad {
				return pos, name, true
			}
		}
	case *ast.ListLiteral:
		for _, elem := range n.Elements {
			if pos, name, bad := findUngroupedNonAggRef(elem, groupingKeys); bad {
				return pos, name, true
			}
		}
	case *ast.MapLiteral:
		for _, val := range n.Values {
			if pos, name, bad := findUngroupedNonAggRef(val, groupingKeys); bad {
				return pos, name, true
			}
		}
	case *ast.CaseExpression:
		if pos, name, bad := findUngroupedNonAggRef(n.Subject, groupingKeys); bad {
			return pos, name, true
		}
		for _, alt := range n.Alternatives {
			if pos, name, bad := findUngroupedNonAggRef(alt.Condition, groupingKeys); bad {
				return pos, name, true
			}
			if pos, name, bad := findUngroupedNonAggRef(alt.Consequent, groupingKeys); bad {
				return pos, name, true
			}
		}
		return findUngroupedNonAggRef(n.ElseExpr, groupingKeys)
	}
	return ast.Position{}, "", false
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

// exprMatchesAnyProjection reports whether e or any of its sub-expressions
// (Property receivers, BinaryOp operands, FunctionInvocation arguments, …)
// has a String() that appears in the projected set. Used by the
// aggregating-WITH ORDER BY check to allow expressions that compose new
// values from projected sub-expressions.
func exprMatchesAnyProjection(e ast.Expression, projected map[string]struct{}) bool {
	if e == nil {
		return false
	}
	if _, ok := projected[e.String()]; ok {
		return true
	}
	switch v := e.(type) {
	case *ast.Property:
		return exprMatchesAnyProjection(v.Receiver, projected)
	case *ast.BinaryOp:
		return exprMatchesAnyProjection(v.Left, projected) || exprMatchesAnyProjection(v.Right, projected)
	case *ast.UnaryOp:
		return exprMatchesAnyProjection(v.Operand, projected)
	case *ast.SubscriptExpr:
		return exprMatchesAnyProjection(v.Expr, projected) || exprMatchesAnyProjection(v.Index, projected)
	case *ast.SliceExpr:
		return exprMatchesAnyProjection(v.Expr, projected) ||
			exprMatchesAnyProjection(v.From, projected) ||
			exprMatchesAnyProjection(v.To, projected)
	case *ast.FunctionInvocation:
		for _, arg := range v.Args {
			if exprMatchesAnyProjection(arg, projected) {
				return true
			}
		}
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			if exprMatchesAnyProjection(el, projected) {
				return true
			}
		}
	case *ast.MapLiteral:
		for _, val := range v.Values {
			if exprMatchesAnyProjection(val, projected) {
				return true
			}
		}
	case *ast.CaseExpression:
		if exprMatchesAnyProjection(v.Subject, projected) {
			return true
		}
		for _, alt := range v.Alternatives {
			if exprMatchesAnyProjection(alt.Condition, projected) ||
				exprMatchesAnyProjection(alt.Consequent, projected) {
				return true
			}
		}
		return exprMatchesAnyProjection(v.ElseExpr, projected)
	}
	return false
}

// collectVariables walks e and returns every Variable name referenced,
// deduplicated. Used by the ORDER-BY-after-aggregation check to find
// references that fall outside the post-WITH projected-alias scope.
// collectFreeVarsOutsideProjectedAggs returns the variable names referenced
// in e that fall outside any aggregation function call whose surface text is
// in projectedExprs. Used by the aggregated-projection ORDER BY check so an
// expression like `me.age + count(you.age)` (where `count(you.age)` is
// projected but `me.age` is not) reports `me` as undefined while still
// permitting expressions composed entirely of projected aggregates and
// literals.
func collectFreeVarsOutsideProjectedAggs(e ast.Expression, projectedExprs map[string]struct{}) []string {
	seen := map[string]struct{}{}
	var out []string
	var walk func(x ast.Expression)
	walk = func(x ast.Expression) {
		if x == nil {
			return
		}
		// If the sub-expression's surface form matches a projected
		// expression (e.g. a projected aggregate call), every variable
		// inside is consumed by the projection — stop the descent.
		if _, projected := projectedExprs[x.String()]; projected {
			return
		}
		switch v := x.(type) {
		case *ast.Variable:
			if _, dup := seen[v.Name]; !dup {
				seen[v.Name] = struct{}{}
				out = append(out, v.Name)
			}
		case *ast.Property:
			walk(v.Receiver)
		case *ast.LabelPredicate:
			walk(v.Receiver)
		case *ast.BinaryOp:
			walk(v.Left)
			walk(v.Right)
		case *ast.UnaryOp:
			walk(v.Operand)
		case *ast.FunctionInvocation:
			for _, arg := range v.Args {
				walk(arg)
			}
		case *ast.SubscriptExpr:
			walk(v.Expr)
			walk(v.Index)
		case *ast.SliceExpr:
			walk(v.Expr)
			walk(v.From)
			walk(v.To)
		case *ast.ListLiteral:
			for _, el := range v.Elements {
				walk(el)
			}
		case *ast.MapLiteral:
			for _, val := range v.Values {
				walk(val)
			}
		case *ast.CaseExpression:
			walk(v.Subject)
			for _, alt := range v.Alternatives {
				walk(alt.Condition)
				walk(alt.Consequent)
			}
			walk(v.ElseExpr)
		case *ast.ListComprehension:
			walk(v.Source)
			walk(v.Predicate)
			walk(v.Projection)
		case *ast.PatternComprehension:
			walk(v.Predicate)
			walk(v.Projection)
		}
	}
	walk(e)
	return out
}

func collectVariables(e ast.Expression) []string {
	seen := map[string]struct{}{}
	var out []string
	var walk func(x ast.Expression)
	walk = func(x ast.Expression) {
		if x == nil {
			return
		}
		switch v := x.(type) {
		case *ast.Variable:
			if _, dup := seen[v.Name]; !dup {
				seen[v.Name] = struct{}{}
				out = append(out, v.Name)
			}
		case *ast.Property:
			walk(v.Receiver)
		case *ast.LabelPredicate:
			walk(v.Receiver)
		case *ast.BinaryOp:
			walk(v.Left)
			walk(v.Right)
		case *ast.UnaryOp:
			walk(v.Operand)
		case *ast.FunctionInvocation:
			for _, arg := range v.Args {
				walk(arg)
			}
		case *ast.SubscriptExpr:
			walk(v.Expr)
			walk(v.Index)
		case *ast.SliceExpr:
			walk(v.Expr)
			walk(v.From)
			walk(v.To)
		case *ast.ListLiteral:
			for _, el := range v.Elements {
				walk(el)
			}
		case *ast.MapLiteral:
			for _, val := range v.Values {
				walk(val)
			}
		case *ast.CaseExpression:
			walk(v.Subject)
			for _, alt := range v.Alternatives {
				walk(alt.Condition)
				walk(alt.Consequent)
			}
			walk(v.ElseExpr)
		case *ast.ListComprehension:
			walk(v.Source)
			walk(v.Predicate)
			walk(v.Projection)
		case *ast.PatternComprehension:
			walk(v.Predicate)
			walk(v.Projection)
		}
	}
	walk(e)
	return out
}

// aggInsideComprehension reports whether e contains a list or pattern
// comprehension whose projection or predicate calls an aggregation
// function. The aggregation has no group to fold over inside a
// comprehension, so the situation is rejected as InvalidAggregation.
func aggInsideComprehension(e ast.Expression) bool {
	if e == nil {
		return false
	}
	switch v := e.(type) {
	case *ast.ListComprehension:
		if containsAggregation(v.Projection) || containsAggregation(v.Predicate) {
			return true
		}
		return aggInsideComprehension(v.Source)
	case *ast.PatternComprehension:
		if containsAggregation(v.Projection) || containsAggregation(v.Predicate) {
			return true
		}
		return false
	case *ast.BinaryOp:
		return aggInsideComprehension(v.Left) || aggInsideComprehension(v.Right)
	case *ast.UnaryOp:
		return aggInsideComprehension(v.Operand)
	case *ast.Property:
		return aggInsideComprehension(v.Receiver)
	case *ast.SubscriptExpr:
		return aggInsideComprehension(v.Expr) || aggInsideComprehension(v.Index)
	case *ast.SliceExpr:
		return aggInsideComprehension(v.Expr) || aggInsideComprehension(v.From) || aggInsideComprehension(v.To)
	case *ast.FunctionInvocation:
		for _, arg := range v.Args {
			if aggInsideComprehension(arg) {
				return true
			}
		}
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			if aggInsideComprehension(el) {
				return true
			}
		}
	case *ast.MapLiteral:
		for _, val := range v.Values {
			if aggInsideComprehension(val) {
				return true
			}
		}
	case *ast.CaseExpression:
		if aggInsideComprehension(v.Subject) {
			return true
		}
		for _, alt := range v.Alternatives {
			if aggInsideComprehension(alt.Condition) || aggInsideComprehension(alt.Consequent) {
				return true
			}
		}
		return aggInsideComprehension(v.ElseExpr)
	}
	return false
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
	case *ast.ListComprehension:
		return containsAggregation(v.Source) ||
			containsAggregation(v.Predicate) ||
			containsAggregation(v.Projection)
	case *ast.PatternComprehension:
		return containsAggregation(v.Predicate) ||
			containsAggregation(v.Projection)
	}
	return false
}

// nondetInsideAggArg reports whether e contains an aggregation call whose
// argument transitively references a non-deterministic function (rand,
// randomUUID). openCypher classifies these as SyntaxError(NonConstantExpression)
// because their value varies per call, leaving aggregation results undefined.
// Returns the position of the offending non-deterministic call when found.
func nondetInsideAggArg(e ast.Expression) (ast.Position, bool) {
	if e == nil {
		return ast.Position{}, false
	}
	switch v := e.(type) {
	case *ast.FunctionInvocation:
		if len(v.Namespace) == 0 {
			switch strings.ToLower(v.Name) {
			case "count", "sum", "avg", "min", "max", "collect",
				"stdev", "stdevp", "percentilecont", "percentiledisc":
				for _, arg := range v.Args {
					if pos, ok := containsNonDetCall(arg); ok {
						return pos, true
					}
				}
			}
		}
		for _, arg := range v.Args {
			if pos, ok := nondetInsideAggArg(arg); ok {
				return pos, true
			}
		}
	case *ast.BinaryOp:
		if pos, ok := nondetInsideAggArg(v.Left); ok {
			return pos, true
		}
		return nondetInsideAggArg(v.Right)
	case *ast.UnaryOp:
		return nondetInsideAggArg(v.Operand)
	case *ast.Property:
		return nondetInsideAggArg(v.Receiver)
	case *ast.LabelPredicate:
		return nondetInsideAggArg(v.Receiver)
	case *ast.SubscriptExpr:
		if pos, ok := nondetInsideAggArg(v.Expr); ok {
			return pos, true
		}
		return nondetInsideAggArg(v.Index)
	case *ast.SliceExpr:
		if pos, ok := nondetInsideAggArg(v.Expr); ok {
			return pos, true
		}
		if pos, ok := nondetInsideAggArg(v.From); ok {
			return pos, true
		}
		return nondetInsideAggArg(v.To)
	case *ast.CaseExpression:
		if pos, ok := nondetInsideAggArg(v.Subject); ok {
			return pos, true
		}
		for _, alt := range v.Alternatives {
			if pos, ok := nondetInsideAggArg(alt.Condition); ok {
				return pos, true
			}
			if pos, ok := nondetInsideAggArg(alt.Consequent); ok {
				return pos, true
			}
		}
		return nondetInsideAggArg(v.ElseExpr)
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			if pos, ok := nondetInsideAggArg(el); ok {
				return pos, true
			}
		}
	case *ast.MapLiteral:
		for _, val := range v.Values {
			if pos, ok := nondetInsideAggArg(val); ok {
				return pos, true
			}
		}
	}
	return ast.Position{}, false
}

// containsNonDetCall reports whether e contains a call to one of the
// non-deterministic openCypher functions (rand, randomUUID). The classifier
// is case-insensitive and unqualified-only.
func containsNonDetCall(e ast.Expression) (ast.Position, bool) {
	if e == nil {
		return ast.Position{}, false
	}
	switch v := e.(type) {
	case *ast.FunctionInvocation:
		if len(v.Namespace) == 0 {
			switch strings.ToLower(v.Name) {
			case "rand", "randomuuid":
				return v.Pos, true
			}
		}
		for _, arg := range v.Args {
			if pos, ok := containsNonDetCall(arg); ok {
				return pos, true
			}
		}
	case *ast.BinaryOp:
		if pos, ok := containsNonDetCall(v.Left); ok {
			return pos, true
		}
		return containsNonDetCall(v.Right)
	case *ast.UnaryOp:
		return containsNonDetCall(v.Operand)
	case *ast.Property:
		return containsNonDetCall(v.Receiver)
	case *ast.LabelPredicate:
		return containsNonDetCall(v.Receiver)
	case *ast.SubscriptExpr:
		if pos, ok := containsNonDetCall(v.Expr); ok {
			return pos, true
		}
		return containsNonDetCall(v.Index)
	case *ast.SliceExpr:
		if pos, ok := containsNonDetCall(v.Expr); ok {
			return pos, true
		}
		if pos, ok := containsNonDetCall(v.From); ok {
			return pos, true
		}
		return containsNonDetCall(v.To)
	case *ast.CaseExpression:
		if pos, ok := containsNonDetCall(v.Subject); ok {
			return pos, true
		}
		for _, alt := range v.Alternatives {
			if pos, ok := containsNonDetCall(alt.Condition); ok {
				return pos, true
			}
			if pos, ok := containsNonDetCall(alt.Consequent); ok {
				return pos, true
			}
		}
		return containsNonDetCall(v.ElseExpr)
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			if pos, ok := containsNonDetCall(el); ok {
				return pos, true
			}
		}
	case *ast.MapLiteral:
		for _, val := range v.Values {
			if pos, ok := containsNonDetCall(val); ok {
				return pos, true
			}
		}
	}
	return ast.Position{}, false
}

// checkFunctionArgTypes performs a coarse static type-check on a handful
// of graph-built-in functions whose argument kind is constrained by the
// openCypher spec. The check fires only when the argument is a Variable
// whose scope symbol type is one of the kinds we can prove is definitely
// invalid for this function ("reject set"). Variables typed "any" /
// "value" / "" and complex expressions (function calls, parameters,
// literals, property access, …) fall through unchecked so we never
// flag a legitimate use.
//
// Reject-set per function (everything else is permitted):
//   - type(x):                rejects node, path        (only relationships have a type)
//   - labels(x):              rejects relationship, path (only nodes carry labels)
//   - keys(x):                rejects path              (nodes / relationships / maps all have keys)
//   - nodes(p), relationships(p): rejects node, relationship (must be a path)
//   - length(x):              rejects node, relationship (length is path-only here)
//   - size(x):                rejects node, relationship, path (size is for strings / lists)
//
// The first failing argument surfaces InvalidArgumentType; subsequent
// arguments are not re-reported for the same invocation.
func (a *analyser) checkFunctionArgTypes(fn *ast.FunctionInvocation) {
	if fn == nil || len(fn.Args) == 0 || len(fn.Namespace) > 0 {
		return
	}
	name := strings.ToLower(fn.Name)
	var reject map[string]bool
	switch name {
	case "type":
		reject = map[string]bool{"node": true, "path": true}
	case "labels":
		reject = map[string]bool{"relationship": true, "path": true}
	case "keys":
		reject = map[string]bool{"path": true}
	case "nodes", "relationships":
		reject = map[string]bool{"node": true, "relationship": true}
	case "length":
		reject = map[string]bool{"node": true, "relationship": true}
	case "size":
		reject = map[string]bool{"node": true, "relationship": true, "path": true}
	default:
		return
	}
	v, ok := fn.Args[0].(*ast.Variable)
	if !ok {
		return
	}
	sym, exists := a.scope.Lookup(v.Name)
	if !exists {
		return
	}
	if !reject[sym.Type] {
		return
	}
	a.error(invalidBooleanOperandError(name, sym.Type, v.Pos))
}

// checkCreateRelationshipTypes flags every relationship pattern in pat
// that does NOT declare exactly one type and an explicit direction
// (CREATE) or that violates the simpler MERGE rules. requireDirection
// is true for CREATE (which forbids undirected relationships) and
// false for MERGE (which accepts them, matching either direction at
// runtime).
func (a *analyser) checkCreateRelationshipTypes(pat *ast.Pattern, requireDirection bool) {
	if pat == nil {
		return
	}
	for _, pp := range pat.Paths {
		a.checkPathPatternRelTypes(pp, requireDirection)
	}
}

// checkPathPatternRelTypes is the per-path implementation used by both
// CREATE and MERGE. It reports:
//   - SyntaxError(NoSingleRelationshipType) for relationships without
//     exactly one type label (zero types or union types).
//   - SyntaxError(CreatingVarLength) for variable-length relationships
//     (CREATE/MERGE require fixed-length per openCypher).
//   - SyntaxError(RequiresDirectedRelationship) for undirected
//     relationships when requireDirection is true (CREATE only).
func (a *analyser) checkPathPatternRelTypes(pp *ast.PathPattern, requireDirection bool) {
	if pp == nil {
		return
	}
	for el := pp.Head; el != nil; el = el.Next {
		if el.Relationship == nil {
			continue
		}
		if el.Relationship.Range != nil {
			a.error(invalidBooleanOperandError("relationship", "variable-length", el.Relationship.Pos))
		}
		if len(el.Relationship.Types) != 1 {
			a.error(invalidBooleanOperandError("relationship", "must have exactly one type", el.Relationship.Pos))
		}
		if requireDirection && el.Relationship.Direction == ast.RelDirectionNone {
			a.error(invalidBooleanOperandError("relationship", "must be directed", el.Relationship.Pos))
		}
	}
}

// checkPatternParameterProps flags every node or relationship pattern
// in pat whose Properties expression is a *ast.Parameter — the form
// `MATCH (n $param)` / `MATCH ()-[r $param]->()` is rejected by
// openCypher as InvalidParameterUse. The legal alternative is to bind
// the parameter via a literal map: `MATCH (n {key: $param})`.
func (a *analyser) checkPatternParameterProps(pat *ast.Pattern) {
	if pat == nil {
		return
	}
	for _, pp := range pat.Paths {
		a.checkPathPatternParameterProps(pp)
	}
}

// checkPathPatternParameterProps is the per-path counterpart of
// [checkPatternParameterProps]. Walks each node/relationship in the
// path and rejects a bare *ast.Parameter receiver.
func (a *analyser) checkPathPatternParameterProps(pp *ast.PathPattern) {
	if pp == nil {
		return
	}
	for el := pp.Head; el != nil; el = el.Next {
		if el.Node != nil && el.Node.Properties != nil {
			if _, isParam := el.Node.Properties.(*ast.Parameter); isParam {
				a.error(invalidBooleanOperandError("node properties", "parameter as full predicate", el.Node.Pos))
			}
		}
		if el.Relationship != nil && el.Relationship.Properties != nil {
			if _, isParam := el.Relationship.Properties.(*ast.Parameter); isParam {
				a.error(invalidBooleanOperandError("relationship properties", "parameter as full predicate", el.Relationship.Pos))
			}
		}
	}
}
