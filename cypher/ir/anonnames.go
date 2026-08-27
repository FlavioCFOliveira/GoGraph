package ir

import (
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// anonSubqueryVarPrefix names an anonymous pattern entity that lives inside an
// EXISTS { } / COUNT { } subquery body.
//
// It is deliberately a DIFFERENT namespace from the plain `__anon_N` the
// translation walk mints (see [translator.freshAnonVar]), for two reasons:
//
//   - A name minted here can never collide with one the outer scope binds and
//     carries into the subquery's seed row. That is the collision rmp #2507 had
//     to dodge at translation time with [translator.reserveAnonVars]; here it is
//     impossible by construction.
//   - The translation walk's own counter is left completely undisturbed, so
//     every anonymous name OUTSIDE a subquery keeps the exact ordinal it has
//     today. Pattern renderings that already surface these names — most visibly
//     the Cartesian-product notification's representative variable — stay
//     byte-identical.
//
// [anonVarOrdinal] deliberately does not recognise this form, because the digits
// do not follow the prefix immediately. These names are therefore excluded from
// ordinal reservation, exactly like the physical builder's own synthetic schema
// keys (`__anon_rel_3`, `__anon_to_4`).
//
// The canonical value lives in [ast.SyntheticSubqueryVarPrefix], because the ast
// package renders patterns and must be able to tell a minted name from a
// user-written one in order to keep it out of a result-column name. Declaring the
// literal here as well would let the two drift, so this aliases it.
//
// It must remain a refinement of [anonVarPrefix]: [IsSyntheticVar] tests only
// that shorter prefix, so were the two to diverge it would stop recognising these
// names and the fast-path recognisers in degree_shape.go and labelled_hop_count.go
// would silently start refusing subquery shapes again (rmp #2508). Go cannot
// assert a string prefix at compile time, so TestAnonSubqueryPrefixIsRefinement
// asserts it instead.
const anonSubqueryVarPrefix = ast.SyntheticSubqueryVarPrefix

// anonSubqueryNamer mints the names for one query's subquery bodies. One namer
// serves the whole query, nested subqueries included, so every name it hands out
// is distinct.
type anonSubqueryNamer struct {
	n int
}

func (a *anonSubqueryNamer) mint() string {
	name := anonSubqueryVarPrefix + strconv.Itoa(a.n)
	a.n++
	return name
}

// NameSubqueryAnonymousEntities assigns a synthetic variable name to every
// anonymous node and relationship inside every EXISTS { } / COUNT { } subquery
// body reachable from q, and does the same for the pattern comprehensions and
// pattern predicates nested inside those bodies.
//
// WHY THIS EXISTS (rmp #2508). Only the OUTER plan is cached. A subquery's inner
// plan is re-translated on EVERY execution, over the SHARED cached AST, and the
// translation walk names anonymous entities by WRITING into that AST (the
// [translator.freshAnonVar] sites in match.go). Two concurrent first executions
// of the same query therefore raced on NodePattern.Variable and
// RelationshipPattern.Variable — and not benignly: each translator mints its own
// names, so one plan could reference a variable the other translator never
// bound, giving a wrong answer rather than a duplicated identical write.
//
// Naming ONCE here, before the AST is ever published to the plan cache, leaves
// nothing for the per-execution walk to write: it finds every Variable non-nil
// and takes no branch. This is the discipline both LPG reference engines follow.
// Neo4j names pattern elements in NameAllPatternElements, a step of its
// AstRewriting phase, which runs inside parsingPost and therefore BEFORE its AST
// cache; Memgraph fills its anonymous identifiers at the end of
// visitSingleQuery, at parse time. Neither re-derives synthetic names over a
// shared mutable tree.
//
// The nil-guards in the translation walk are deliberately KEPT rather than
// removed. They stay load-bearing for callers that translate a private, freshly
// parsed AST, and on the cached path they become an unreachable fallback.
//
// Concurrency: NOT safe against any concurrent access to q. It must run while q
// is still private to the plan-cache builder, which is the entire point of it.
func NameSubqueryAnonymousEntities(q ast.Query) {
	n := &anonSubqueryNamer{}
	walkTopLevelExpressions(q, func(e ast.Expression) {
		switch s := e.(type) {
		case *ast.ExistsSubquery:
			n.nameSubqueryBody(s.Pattern, s.Where, s.Query)
		case *ast.CountSubquery:
			n.nameSubqueryBody(s.Pattern, s.Where, s.Query)
		}
	})
}

// nameAnonymousPathEntities names exactly the anonymous entities of pp that the
// match translator addresses by name, applying the same rules the translation
// walk applies. It is the second statement of those rules; the first is in
// match.go, at the [translator.freshAnonVar] sites this mirrors. The regression
// test TestSubqueryConcurrentFirstExecution_2508 is what detects the two drifting
// apart: any entity this misses is minted per execution again, and races.
func nameAnonymousPathEntities(pp *ast.PathPattern, mint func() string) {
	if pp == nil || pp.Head == nil {
		return
	}
	ensure := func(p **string) {
		if *p == nil {
			v := mint()
			*p = &v
		}
	}

	if pp.Shortest != ast.ShortestNone {
		// matchShortestPath needs concrete FromVar/ToVar columns, so it names
		// BOTH endpoints and leaves the relationship anonymous. It does so only
		// for a well-formed single-relationship inner pattern and reports an
		// error on any other shape, so mirror that guard exactly rather than
		// naming a pattern it would reject.
		h := pp.Head
		if h.Node == nil || h.Next == nil || h.Next.Relationship == nil || h.Next.Node == nil {
			return
		}
		ensure(&h.Node.Variable)
		ensure(&h.Next.Node.Variable)
		return
	}

	// Ordinary path. The HEAD node is deliberately left anonymous: an anonymous
	// head is SCANNED, not addressed by name, and matchNodeScan leaves its
	// NodeVar "" for it. Naming it would change the plan — see the anchor-swap
	// mirror in cypher/anchor_swap_anonymous_test.go, which turns on the head
	// being the only element that can carry the empty name. Every SUBSEQUENT
	// node and every relationship is named, so a later hop can reference the
	// column the preceding Expand emits.
	for el := pp.Head.Next; el != nil; el = el.Next {
		if el.Relationship == nil || el.Node == nil {
			continue
		}
		ensure(&el.Node.Variable)
		ensure(&el.Relationship.Variable)
	}
}

// nameSubqueryBody names every anonymous pattern entity in one subquery, in both
// of its spellings: the pattern form (EXISTS { (a)-->(b) }, carried in pattern
// plus an optional inline where) and the full-query form
// (COUNT { MATCH … RETURN … }, carried in body).
//
// It recurses into nested subqueries itself rather than leaving them to the
// top-level walk, so "inside a subquery body" never has to be tracked as state:
// it is simply which function is running.
func (a *anonSubqueryNamer) nameSubqueryBody(pattern *ast.Pattern, where *ast.Where, body *ast.SingleQuery) {
	a.namePattern(pattern)
	if where != nil {
		a.nameExpr(where.Predicate)
	}
	a.nameSingleQuery(body)
}

func (a *anonSubqueryNamer) namePattern(pat *ast.Pattern) {
	if pat == nil {
		return
	}
	for _, pp := range pat.Paths {
		a.namePathPattern(pp)
	}
}

// namePathPattern names one path's own anonymous entities AND descends into the
// property maps its nodes and relationships carry inline, because a nested
// subquery can hide in one: `MATCH (m {age: COUNT { (n)-->() }})`. Missing that
// descent left the nested subquery unnamed, so its body was minted per execution
// again and raced — the exact defect rmp #2508 fixes, one level down. This
// mirrors [topLevelExprWalker.pathPattern], which walks the same two fields.
func (a *anonSubqueryNamer) namePathPattern(pp *ast.PathPattern) {
	if pp == nil {
		return
	}
	nameAnonymousPathEntities(pp, a.mint)
	for el := pp.Head; el != nil; el = el.Next {
		if el.Node != nil {
			a.nameExpr(el.Node.Properties)
		}
		if el.Relationship != nil {
			a.nameExpr(el.Relationship.Properties)
		}
	}
}

func (a *anonSubqueryNamer) nameSingleQuery(q *ast.SingleQuery) {
	if q == nil {
		return
	}
	for _, rc := range q.ReadingClauses {
		a.nameReadingClause(rc)
	}
	for _, uc := range q.UpdatingClauses {
		a.nameUpdatingClause(uc)
	}
	for _, w := range q.With {
		a.nameWith(w)
	}
	if q.Return != nil {
		a.nameProjection(q.Return.Projection)
	}
}

func (a *anonSubqueryNamer) nameReadingClause(rc ast.ReadingClause) {
	switch c := rc.(type) {
	case *ast.Match:
		a.namePattern(c.Pattern)
		a.nameWhere(c.Where)
	case *ast.OptionalMatch:
		a.namePattern(c.Pattern)
		a.nameWhere(c.Where)
	case *ast.With:
		a.nameWith(c)
	case *ast.Return:
		a.nameProjection(c.Projection)
	case *ast.Unwind:
		a.nameExpr(c.Expr)
	case *ast.Call:
		for _, arg := range c.Args {
			a.nameExpr(arg)
		}
		a.nameWhere(c.Where)
	}
}

// nameUpdatingClause descends into an updating clause's EXPRESSIONS only. The
// clause's own CREATE/MERGE pattern is left alone on purpose: writes name their
// anonymous nodes under different rules (translator.ensureNodeVar names the
// head too, because createNode must address it), so naming them here with the
// match rules would be wrong. Only the expressions can hide a subquery.
func (a *anonSubqueryNamer) nameUpdatingClause(uc ast.UpdatingClause) {
	switch c := uc.(type) {
	case *ast.Merge:
		for _, it := range c.OnCreate {
			a.nameSetItem(it)
		}
		for _, it := range c.OnMatch {
			a.nameSetItem(it)
		}
	case *ast.Delete:
		for _, e := range c.Expressions {
			a.nameExpr(e)
		}
	case *ast.DetachDelete:
		for _, e := range c.Expressions {
			a.nameExpr(e)
		}
	case *ast.Set:
		for _, it := range c.Items {
			a.nameSetItem(it)
		}
	case *ast.Foreach:
		a.nameExpr(c.Expr)
		for _, inner := range c.Body {
			a.nameUpdatingClause(inner)
		}
	}
}

func (a *anonSubqueryNamer) nameSetItem(it *ast.SetItem) {
	if it == nil {
		return
	}
	a.nameExpr(it.Target)
	a.nameExpr(it.Value)
}

func (a *anonSubqueryNamer) nameWith(w *ast.With) {
	if w == nil {
		return
	}
	a.nameProjection(w.Projection)
	a.nameWhere(w.Where)
}

func (a *anonSubqueryNamer) nameWhere(w *ast.Where) {
	if w == nil {
		return
	}
	a.nameExpr(w.Predicate)
}

func (a *anonSubqueryNamer) nameProjection(p *ast.Projection) {
	if p == nil {
		return
	}
	for _, it := range p.Items {
		if it != nil {
			a.nameExpr(it.Expr)
		}
	}
	for _, s := range p.OrderBy {
		if s != nil {
			a.nameExpr(s.Expr)
		}
	}
	a.nameExpr(p.Skip)
	a.nameExpr(p.Limit)
}

// nameExpr walks one expression tree, naming the patterns it carries. Every
// [ast.Expression] implementation that can hold a sub-expression or a pattern
// has a case here; the leaf literals fall through the switch. Nested subqueries
// recurse through nameSubqueryBody, so a subquery inside a subquery is named
// too.
func (a *anonSubqueryNamer) nameExpr(e ast.Expression) {
	switch v := e.(type) {
	case nil:
		return

	// Patterns reachable as expressions inside a subquery body: a pattern
	// predicate is a bare PathPattern, and a pattern comprehension carries one.
	case *ast.PathPattern:
		a.namePathPattern(v)
	case *ast.PatternComprehension:
		a.namePathPattern(v.Pattern)
		a.nameExpr(v.Predicate)
		a.nameExpr(v.Projection)

	// Nested subqueries.
	case *ast.ExistsSubquery:
		a.nameSubqueryBody(v.Pattern, v.Where, v.Query)
	case *ast.CountSubquery:
		a.nameSubqueryBody(v.Pattern, v.Where, v.Query)

	// Composite expressions.
	case *ast.BinaryOp:
		a.nameExpr(v.Left)
		a.nameExpr(v.Right)
	case *ast.UnaryOp:
		a.nameExpr(v.Operand)
	case *ast.CaseExpression:
		a.nameExpr(v.Subject)
		a.nameExpr(v.ElseExpr)
		for _, alt := range v.Alternatives {
			if alt != nil {
				a.nameExpr(alt.Condition)
				a.nameExpr(alt.Consequent)
			}
		}
	case *ast.FunctionInvocation:
		for _, arg := range v.Args {
			a.nameExpr(arg)
		}
	case *ast.ListComprehension:
		a.nameExpr(v.Source)
		a.nameExpr(v.Predicate)
		a.nameExpr(v.Projection)
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			a.nameExpr(el)
		}
	case *ast.MapLiteral:
		for _, val := range v.Values {
			a.nameExpr(val)
		}
	case *ast.MapProjection:
		a.nameExpr(v.Subject)
		for _, it := range v.Items {
			if it != nil {
				a.nameExpr(it.Value)
			}
		}
	case *ast.Property:
		a.nameExpr(v.Receiver)
	case *ast.ReduceExpr:
		a.nameExpr(v.Init)
		a.nameExpr(v.Source)
		a.nameExpr(v.Projection)
	case *ast.SliceExpr:
		a.nameExpr(v.Expr)
		a.nameExpr(v.From)
		a.nameExpr(v.To)
	case *ast.SubscriptExpr:
		a.nameExpr(v.Expr)
		a.nameExpr(v.Index)
	case *ast.LabelPredicate:
		a.nameExpr(v.Receiver)
	}
}

// walkTopLevelExpressions visits every expression of q that is NOT inside an
// EXISTS { } / COUNT { } subquery body. It stops at a subquery rather than
// descending, because [anonSubqueryNamer.nameSubqueryBody] owns everything below
// that point and recurses on its own.
func walkTopLevelExpressions(q ast.Query, fn func(ast.Expression)) {
	w := &topLevelExprWalker{fn: fn}
	switch n := q.(type) {
	case *ast.SingleQuery:
		w.singleQuery(n)
	case *ast.MultiQuery:
		for _, part := range n.Parts {
			w.singleQuery(part)
		}
	}
}

type topLevelExprWalker struct {
	fn func(ast.Expression)
}

func (w *topLevelExprWalker) singleQuery(q *ast.SingleQuery) {
	if q == nil {
		return
	}
	for _, rc := range q.ReadingClauses {
		w.readingClause(rc)
	}
	for _, uc := range q.UpdatingClauses {
		w.updatingClause(uc)
	}
	for _, with := range q.With {
		w.with(with)
	}
	if q.Return != nil {
		w.projection(q.Return.Projection)
	}
}

func (w *topLevelExprWalker) readingClause(rc ast.ReadingClause) {
	switch c := rc.(type) {
	case *ast.Match:
		w.pattern(c.Pattern)
		w.where(c.Where)
	case *ast.OptionalMatch:
		w.pattern(c.Pattern)
		w.where(c.Where)
	case *ast.With:
		w.with(c)
	case *ast.Return:
		w.projection(c.Projection)
	case *ast.Unwind:
		w.expr(c.Expr)
	case *ast.Call:
		for _, arg := range c.Args {
			w.expr(arg)
		}
		w.where(c.Where)
	}
}

func (w *topLevelExprWalker) updatingClause(uc ast.UpdatingClause) {
	switch c := uc.(type) {
	case *ast.Create:
		w.pattern(c.Pattern)
	case *ast.Merge:
		w.pathPattern(c.Pattern)
		for _, it := range c.OnCreate {
			w.setItem(it)
		}
		for _, it := range c.OnMatch {
			w.setItem(it)
		}
	case *ast.Delete:
		for _, e := range c.Expressions {
			w.expr(e)
		}
	case *ast.DetachDelete:
		for _, e := range c.Expressions {
			w.expr(e)
		}
	case *ast.Set:
		for _, it := range c.Items {
			w.setItem(it)
		}
	case *ast.Foreach:
		w.expr(c.Expr)
		for _, inner := range c.Body {
			w.updatingClause(inner)
		}
	}
}

func (w *topLevelExprWalker) setItem(it *ast.SetItem) {
	if it == nil {
		return
	}
	w.expr(it.Target)
	w.expr(it.Value)
}

func (w *topLevelExprWalker) with(with *ast.With) {
	if with == nil {
		return
	}
	w.projection(with.Projection)
	w.where(with.Where)
}

func (w *topLevelExprWalker) where(wh *ast.Where) {
	if wh == nil {
		return
	}
	w.expr(wh.Predicate)
}

func (w *topLevelExprWalker) projection(p *ast.Projection) {
	if p == nil {
		return
	}
	for _, it := range p.Items {
		if it != nil {
			w.expr(it.Expr)
		}
	}
	for _, s := range p.OrderBy {
		if s != nil {
			w.expr(s.Expr)
		}
	}
	w.expr(p.Skip)
	w.expr(p.Limit)
}

func (w *topLevelExprWalker) pattern(pat *ast.Pattern) {
	if pat == nil {
		return
	}
	for _, pp := range pat.Paths {
		w.pathPattern(pp)
	}
}

// pathPattern descends into the predicates a pattern can carry inline, because a
// subquery can hide inside one. The pattern's own node and relationship
// variables are not the walker's concern.
func (w *topLevelExprWalker) pathPattern(pp *ast.PathPattern) {
	if pp == nil {
		return
	}
	for el := pp.Head; el != nil; el = el.Next {
		if el.Node != nil {
			w.expr(el.Node.Properties)
		}
		if el.Relationship != nil {
			w.expr(el.Relationship.Properties)
		}
	}
}

// expr walks one expression tree, reporting every node to w.fn, and deliberately
// does NOT descend into a subquery body — the caller's namer owns that subtree.
func (w *topLevelExprWalker) expr(e ast.Expression) {
	if e == nil {
		return
	}
	w.fn(e)
	switch v := e.(type) {
	case *ast.ExistsSubquery, *ast.CountSubquery:
		// Reported, not descended into: nameSubqueryBody recurses on its own.
		return
	case *ast.PathPattern:
		w.pathPattern(v)
	case *ast.PatternComprehension:
		w.pathPattern(v.Pattern)
		w.expr(v.Predicate)
		w.expr(v.Projection)
	case *ast.BinaryOp:
		w.expr(v.Left)
		w.expr(v.Right)
	case *ast.UnaryOp:
		w.expr(v.Operand)
	case *ast.CaseExpression:
		w.expr(v.Subject)
		w.expr(v.ElseExpr)
		for _, alt := range v.Alternatives {
			if alt != nil {
				w.expr(alt.Condition)
				w.expr(alt.Consequent)
			}
		}
	case *ast.FunctionInvocation:
		for _, arg := range v.Args {
			w.expr(arg)
		}
	case *ast.ListComprehension:
		w.expr(v.Source)
		w.expr(v.Predicate)
		w.expr(v.Projection)
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			w.expr(el)
		}
	case *ast.MapLiteral:
		for _, val := range v.Values {
			w.expr(val)
		}
	case *ast.MapProjection:
		w.expr(v.Subject)
		for _, it := range v.Items {
			if it != nil {
				w.expr(it.Value)
			}
		}
	case *ast.Property:
		w.expr(v.Receiver)
	case *ast.ReduceExpr:
		w.expr(v.Init)
		w.expr(v.Source)
		w.expr(v.Projection)
	case *ast.SliceExpr:
		w.expr(v.Expr)
		w.expr(v.From)
		w.expr(v.To)
	case *ast.SubscriptExpr:
		w.expr(v.Expr)
		w.expr(v.Index)
	case *ast.LabelPredicate:
		w.expr(v.Receiver)
	}
}
