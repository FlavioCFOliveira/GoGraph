package parser

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/antlr4-go/antlr/v4"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser/gen"
)

// visitor converts an antlr parse tree into the typed AST defined in
// github.com/FlavioCFOliveira/GoGraph/cypher/ast.  It embeds gen.BaseCypherParserVisitor for default
// no-op implementations of any Visit* method not explicitly overridden.
//
// All Visit* methods return interface{}; callers use the as* helpers to
// unwrap the typed values.  Errors are propagated by returning a *SemaError.
type visitor struct {
	gen.BaseCypherParserVisitor
}

// newVisitor allocates a zero-value visitor ready to use.
func newVisitor() *visitor { return &visitor{} }

// positionOf extracts the start source position from any parse-tree node.
func positionOf(ctx antlr.ParserRuleContext) ast.Position {
	if ctx == nil {
		return ast.Position{}
	}
	start := ctx.GetStart()
	if start == nil {
		return ast.Position{}
	}
	return ast.Position{
		Line:   uint32(start.GetLine()),
		Column: uint32(start.GetColumn()),
		Offset: uint32(start.GetStart()),
	}
}

// endPositionOf extracts the end source position from any parse-tree node.
// The returned position points to the first byte past the last token's text.
// For single-token rules the end position equals the start position plus the
// token length (on the same line).
func endPositionOf(ctx antlr.ParserRuleContext) ast.Position {
	if ctx == nil {
		return ast.Position{}
	}
	stop := ctx.GetStop()
	if stop == nil {
		// Fall back to start when stop is unavailable.
		return positionOf(ctx)
	}
	tokenLen := uint32(len(stop.GetText()))
	return ast.Position{
		Line:   uint32(stop.GetLine()),
		Column: uint32(stop.GetColumn()) + tokenLen,
		Offset: uint32(stop.GetStop()) + 1,
	}
}

// visit dispatches to the concrete Accept method of the tree node.
func (v *visitor) visit(tree antlr.ParseTree) interface{} {
	if tree == nil {
		return nil
	}
	return tree.Accept(v)
}

// asExpr casts a visitor result to ast.Expression. Returns *SemaError on type
// mismatch.
func asExpr(val interface{}) (ast.Expression, error) {
	if val == nil {
		return nil, nil
	}
	if err, ok := val.(*SemaError); ok {
		return nil, err
	}
	if e, ok := val.(ast.Expression); ok {
		return e, nil
	}
	return nil, fmt.Errorf("internal: expected Expression, got %T", val)
}

// firstError returns the first *SemaError found in results, or nil.
func firstError(results ...interface{}) *SemaError {
	for _, r := range results {
		if e, ok := r.(*SemaError); ok {
			return e
		}
	}
	return nil
}

// unsupported returns a *SemaError for grammar rules that are intentionally
// out of scope.
func unsupported(ctx antlr.ParserRuleContext, rule, msg string) *SemaError {
	return &SemaError{Rule: rule, Pos: positionOf(ctx), Message: msg}
}

// projItems is the package-level transfer type returned by VisitProjectionItems.
// Using a package-level type ensures that the type assertion in visitProjectionBody
// resolves correctly regardless of the call site.
type projItems struct {
	all   bool
	items []*ast.ProjectionItem
}

// mergeAction is the package-level transfer type returned by VisitMergeAction.
type mergeAction struct {
	onCreate bool
	items    []*ast.SetItem
}

// -------------------------------------------------------------------------
// Script / Query dispatch
// -------------------------------------------------------------------------

// VisitScript is the entry point. The script rule wraps a single query.
func (v *visitor) VisitScript(ctx *gen.ScriptContext) interface{} {
	return v.visit(ctx.Query())
}

// VisitQuery dispatches to regularQuery or standaloneCall.
func (v *visitor) VisitQuery(ctx *gen.QueryContext) interface{} {
	if rq := ctx.RegularQuery(); rq != nil {
		return v.visit(rq)
	}
	if sc := ctx.StandaloneCall(); sc != nil {
		return v.visit(sc)
	}
	return unsupported(ctx, "query", "empty query")
}

// VisitRegularQuery handles UNION of singleQuery.
func (v *visitor) VisitRegularQuery(ctx *gen.RegularQueryContext) interface{} {
	sq := ctx.SingleQuery()
	first, ok := v.visit(sq).(*ast.SingleQuery)
	if !ok {
		return v.visit(sq)
	}

	unions := ctx.AllUnionSt()
	if len(unions) == 0 {
		return first
	}

	parts := []*ast.SingleQuery{first}
	var sawAll, sawDistinct bool
	for _, u := range unions {
		ur := v.visit(u)
		union, ok := ur.(*ast.Union)
		if !ok {
			return ur // propagate error
		}
		if union.All {
			sawAll = true
		} else {
			sawDistinct = true
		}
		parts = append(parts, union.Query)
	}
	if sawAll && sawDistinct {
		// openCypher 9 §3.3.2 forbids mixing UNION and UNION ALL in the
		// same query — the deduplication semantics are incompatible.
		return &SemaError{
			Rule:    "regularQuery",
			Pos:     positionOf(ctx),
			Message: "InvalidClauseComposition: cannot mix UNION and UNION ALL in the same query",
		}
	}
	// Cross-branch column-name agreement: every branch of a UNION must
	// project the same number of columns with the same names (order
	// matters). Mismatches must surface as a compile-time SyntaxError
	// (DifferentColumnsInUnion).
	if err := checkUnionColumns(parts, ctx); err != nil {
		return err
	}
	return &ast.MultiQuery{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Parts: parts, All: sawAll}
}

// checkUnionColumns enforces that every branch of a UNION projects the
// same explicit aliases in the same order. Only aliases (item.Alias != nil)
// participate in the check: anonymous projection items keep their
// expression text as the column name, and openCypher implementations
// vary on whether two anonymous columns with different expression text
// can UNION. The TCK only exercises the aliased / column-count
// mismatch cases, which we cover with this conservative rule.
func checkUnionColumns(parts []*ast.SingleQuery, ctx antlr.ParserRuleContext) error {
	if len(parts) < 2 {
		return nil
	}
	first, firstHasAlias := unionBranchColumns(parts[0])
	for i := 1; i < len(parts); i++ {
		cols, hasAlias := unionBranchColumns(parts[i])
		// Column count must always match.
		if len(cols) != len(first) {
			return &SemaError{
				Rule:    "regularQuery",
				Pos:     positionOf(ctx),
				Message: "DifferentColumnsInUnion: all UNION branches must project the same number of columns",
			}
		}
		// Alias-name agreement only when both sides have explicit aliases.
		if firstHasAlias && hasAlias && !sameColumnList(first, cols) {
			return &SemaError{
				Rule:    "regularQuery",
				Pos:     positionOf(ctx),
				Message: "DifferentColumnsInUnion: all UNION branches must project the same columns in the same order",
			}
		}
	}
	return nil
}

// unionBranchColumns returns (column names, hasAlias) for the trailing
// RETURN of a UNION branch. hasAlias is true iff every projection item
// carries an explicit AS alias.
func unionBranchColumns(sq *ast.SingleQuery) ([]string, bool) {
	if sq == nil || sq.Return == nil || sq.Return.Projection == nil {
		return nil, false
	}
	items := sq.Return.Projection.Items
	out := make([]string, 0, len(items))
	allAliased := len(items) > 0
	for _, item := range items {
		if item.Alias != nil {
			out = append(out, *item.Alias)
			continue
		}
		allAliased = false
		if item.Expr != nil {
			out = append(out, item.Expr.String())
		}
	}
	return out, allAliased
}

// sameColumnList reports whether a and b have identical names in identical
// positions.
func sameColumnList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// VisitUnionSt handles a UNION [ALL] singleQuery clause.
func (v *visitor) VisitUnionSt(ctx *gen.UnionStContext) interface{} {
	sq := ctx.SingleQuery()
	if sq == nil {
		return unsupported(ctx, "unionSt", "missing single query in UNION")
	}
	sqResult := v.visit(sq)
	part, ok := sqResult.(*ast.SingleQuery)
	if !ok {
		return sqResult
	}
	return &ast.Union{
		Pos:    positionOf(ctx),
		EndPos: endPositionOf(ctx),
		All:    ctx.ALL() != nil,
		Query:  part,
	}
}

// VisitSingleQuery selects singlePartQ or multiPartQ.
func (v *visitor) VisitSingleQuery(ctx *gen.SingleQueryContext) interface{} {
	if spq := ctx.SinglePartQ(); spq != nil {
		return v.visit(spq)
	}
	if mpq := ctx.MultiPartQ(); mpq != nil {
		return v.visit(mpq)
	}
	return unsupported(ctx, "singleQuery", "empty single query")
}

// VisitSinglePartQ handles the simple (no WITH) query form.
func (v *visitor) VisitSinglePartQ(ctx *gen.SinglePartQContext) interface{} {
	q := &ast.SingleQuery{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}

	for _, rs := range ctx.AllReadingStatement() {
		r := v.visit(rs)
		if err := firstError(r); err != nil {
			return err
		}
		if c, ok := r.(ast.ReadingClause); ok {
			q.ReadingClauses = append(q.ReadingClauses, c)
		}
	}
	for _, us := range ctx.AllUpdatingStatement() {
		u := v.visit(us)
		if err := firstError(u); err != nil {
			return err
		}
		if c, ok := u.(ast.UpdatingClause); ok {
			q.UpdatingClauses = append(q.UpdatingClauses, c)
		}
	}
	if ret := ctx.ReturnSt(); ret != nil {
		r := v.visit(ret)
		if err := firstError(r); err != nil {
			return err
		}
		if rv, ok := r.(*ast.Return); ok {
			q.Return = rv
		}
	}
	return q
}

// VisitMultiPartQ handles queries with one or more WITH-prefixed parts.
//
// The ANTLR grammar for multiPartQ is:
//
//	(ReadingStatement* UpdatingStatement* WithSt)+ SinglePartQ
//
// Children appear in document order: ReadingStatements and UpdatingStatements
// from each (…WithSt) cycle, followed by the terminal SinglePartQ.
//
// To preserve the correct interleaving between reading clauses and WITH
// clauses, this visitor iterates GetChildren() in document order and emits
// each child into q.ReadingClauses (reading clauses and *ast.With nodes
// alike) or q.UpdatingClauses.  q.With is intentionally left empty; the IR
// translator recognises q.LeadingCountSet=true and processes all
// ReadingClauses via t.readingClause, which already dispatches *ast.With
// through t.withClause — giving the correct evaluation order.
//
// Backward compatibility: manually-constructed ast.SingleQuery objects (used
// in unit tests) do NOT set LeadingCountSet, so the translator falls back to
// the legacy "all ReadingClauses first, then q.With" ordering.
func (v *visitor) VisitMultiPartQ(ctx *gen.MultiPartQContext) interface{} {
	q := &ast.SingleQuery{
		Pos:             positionOf(ctx),
		EndPos:          endPositionOf(ctx),
		LeadingCountSet: true, // signal to translator to use document-order processing
	}

	// Iterate children in document order so that reading clauses and WITH
	// clauses are appended to q.ReadingClauses in the sequence they appear in
	// the source query.
	for _, child := range ctx.GetChildren() {
		switch c := child.(type) {
		case gen.IReadingStatementContext:
			r := v.visit(c)
			if err := firstError(r); err != nil {
				return err
			}
			if rc, ok := r.(ast.ReadingClause); ok {
				q.ReadingClauses = append(q.ReadingClauses, rc)
			}

		case gen.IUpdatingStatementContext:
			u := v.visit(c)
			if err := firstError(u); err != nil {
				return err
			}
			if uc, ok := u.(ast.UpdatingClause); ok {
				q.UpdatingClauses = append(q.UpdatingClauses, uc)
			}

		case gen.IWithStContext:
			// Append the WITH clause as a reading clause (ast.With implements
			// ast.ReadingClause) so the translator sees it in document order.
			// Also append to q.With for backward compatibility with parser tests
			// that inspect that field directly.
			w := v.visit(c)
			if err := firstError(w); err != nil {
				return err
			}
			if wv, ok := w.(*ast.With); ok {
				q.ReadingClauses = append(q.ReadingClauses, wv)
				q.With = append(q.With, wv) // kept for external inspection; translator ignores when LeadingCountSet=true
			}

		case gen.ISinglePartQContext:
			// Terminal singlePartQ carries the final RETURN and any trailing
			// reading/updating clauses that follow the last WITH.
			spqResult := v.visit(c)
			if err := firstError(spqResult); err != nil {
				return err
			}
			if inner, ok := spqResult.(*ast.SingleQuery); ok {
				q.ReadingClauses = append(q.ReadingClauses, inner.ReadingClauses...)
				q.UpdatingClauses = append(q.UpdatingClauses, inner.UpdatingClauses...)
				// inner.With is always empty for parser-generated SinglePartQ nodes;
				// do NOT merge it into q.ReadingClauses to avoid duplicates.
				q.Return = inner.Return
			}
		}
	}
	return q
}

// -------------------------------------------------------------------------
// Standalone CALL
// -------------------------------------------------------------------------

// VisitStandaloneCall handles CALL proc(args) YIELD items.
func (v *visitor) VisitStandaloneCall(ctx *gen.StandaloneCallContext) interface{} {
	c := v.buildCallFromInvocationName(ctx, ctx.InvocationName())

	// Optional argument list.
	if pec := ctx.ParenExpressionChain(); pec != nil {
		args, err := v.visitExpressionChain(pec.ExpressionChain())
		if err != nil {
			return err
		}
		c.Args = args
	} else {
		c.Args = nil // no parens → no arg list
	}

	// YIELD clause. YIELD * is signalled by MULT token on this context.
	if ctx.YIELD() != nil {
		if ctx.MULT() != nil {
			c.Yield = []*ast.YieldItem{} // YIELD * → empty slice
		} else if yi := ctx.YieldItems(); yi != nil {
			yield, err := v.visitYieldItems(yi)
			if err != nil {
				return err
			}
			c.Yield = yield
		}
	}

	// CALL at top level wraps inside a SingleQuery with no RETURN.
	return &ast.SingleQuery{
		Pos:            positionOf(ctx),
		EndPos:         endPositionOf(ctx),
		ReadingClauses: []ast.ReadingClause{c},
	}
}

func (v *visitor) buildCallFromInvocationName(ctx antlr.ParserRuleContext, inCtx gen.IInvocationNameContext) *ast.Call {
	c := &ast.Call{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	if inCtx != nil {
		syms := inCtx.AllSymbol()
		for i, s := range syms {
			name := symbolText(s)
			if i < len(syms)-1 {
				c.Namespace = append(c.Namespace, name)
			} else {
				c.Procedure = name
			}
		}
	}
	return c
}

// -------------------------------------------------------------------------
// Reading statements
// -------------------------------------------------------------------------

func (v *visitor) VisitReadingStatement(ctx *gen.ReadingStatementContext) interface{} {
	if m := ctx.MatchSt(); m != nil {
		return v.visit(m)
	}
	if u := ctx.UnwindSt(); u != nil {
		return v.visit(u)
	}
	if c := ctx.QueryCallSt(); c != nil {
		return v.visit(c)
	}
	return unsupported(ctx, "readingStatement", "unknown reading statement variant")
}

// VisitMatchSt handles MATCH and OPTIONAL MATCH.
func (v *visitor) VisitMatchSt(ctx *gen.MatchStContext) interface{} {
	pw := ctx.PatternWhere()
	if pw == nil {
		return unsupported(ctx, "matchSt", "missing patternWhere")
	}
	pat, err := v.visitPatternWhere(pw)
	if err != nil {
		return err
	}

	if ctx.OPTIONAL() != nil {
		return &ast.OptionalMatch{
			Pos:     positionOf(ctx),
			EndPos:  endPositionOf(ctx),
			Pattern: pat.Pattern,
			Where:   pat.Where,
		}
	}
	return &ast.Match{
		Pos:     positionOf(ctx),
		EndPos:  endPositionOf(ctx),
		Pattern: pat.Pattern,
		Where:   pat.Where,
	}
}

// VisitUnwindSt handles UNWIND expr AS var.
func (v *visitor) VisitUnwindSt(ctx *gen.UnwindStContext) interface{} {
	expr, err := asExpr(v.visit(ctx.Expression()))
	if err != nil {
		return &SemaError{Rule: "unwindSt", Pos: positionOf(ctx), Message: err.Error()}
	}
	varName := symbolText(ctx.Symbol())
	return &ast.Unwind{
		Pos:      positionOf(ctx),
		EndPos:   endPositionOf(ctx),
		Expr:     expr,
		Variable: varName,
	}
}

// VisitQueryCallSt handles an in-query CALL statement.
func (v *visitor) VisitQueryCallSt(ctx *gen.QueryCallStContext) interface{} {
	c := v.buildCallFromInvocationName(ctx, ctx.InvocationName())

	if pec := ctx.ParenExpressionChain(); pec != nil {
		args, err := v.visitExpressionChain(pec.ExpressionChain())
		if err != nil {
			return err
		}
		c.Args = args
	}
	if yi := ctx.YieldItems(); yi != nil {
		yield, err := v.visitYieldItems(yi)
		if err != nil {
			return err
		}
		c.Yield = yield
	}
	return c
}

// -------------------------------------------------------------------------
// Updating statements
// -------------------------------------------------------------------------

func (v *visitor) VisitUpdatingStatement(ctx *gen.UpdatingStatementContext) interface{} {
	if c := ctx.CreateSt(); c != nil {
		return v.visit(c)
	}
	if m := ctx.MergeSt(); m != nil {
		return v.visit(m)
	}
	if s := ctx.SetSt(); s != nil {
		return v.visit(s)
	}
	if r := ctx.RemoveSt(); r != nil {
		return v.visit(r)
	}
	if d := ctx.DeleteSt(); d != nil {
		return v.visit(d)
	}
	return unsupported(ctx, "updatingStatement", "unknown updating statement variant")
}

// VisitCreateSt handles CREATE pattern.
func (v *visitor) VisitCreateSt(ctx *gen.CreateStContext) interface{} {
	pat, err := v.visitPattern(ctx.Pattern())
	if err != nil {
		return err
	}
	return &ast.Create{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Pattern: pat}
}

// VisitMergeSt handles MERGE path ON CREATE SET … ON MATCH SET ….
func (v *visitor) VisitMergeSt(ctx *gen.MergeStContext) interface{} {
	pp, err := v.visitPatternPart(ctx.PatternPart())
	if err != nil {
		return err
	}
	m := &ast.Merge{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Pattern: pp}

	for _, ma := range ctx.AllMergeAction() {
		maResult := v.visit(ma)
		if e := firstError(maResult); e != nil {
			return e
		}
		if mv, ok := maResult.(*mergeAction); ok {
			if mv.onCreate {
				m.OnCreate = append(m.OnCreate, mv.items...)
			} else {
				m.OnMatch = append(m.OnMatch, mv.items...)
			}
		}
	}
	return m
}

// VisitMergeAction handles ON CREATE SET / ON MATCH SET.
func (v *visitor) VisitMergeAction(ctx *gen.MergeActionContext) interface{} {
	ss := ctx.SetSt()
	if ss == nil {
		return unsupported(ctx, "mergeAction", "missing SET in merge action")
	}
	setResult := v.visit(ss)
	if err := firstError(setResult); err != nil {
		return err
	}
	set, ok := setResult.(*ast.Set)
	if !ok {
		return unsupported(ctx, "mergeAction", "expected SET")
	}
	// ON keyword is at position 0, then CREATE or MATCH token.
	// ctx.CREATE() returns the CREATE terminal, ctx.MATCH() returns MATCH.
	onCreate := ctx.CREATE() != nil
	return &mergeAction{onCreate: onCreate, items: set.Items}
}

// VisitSetSt handles SET item, item, ….
func (v *visitor) VisitSetSt(ctx *gen.SetStContext) interface{} {
	set := &ast.Set{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	for _, si := range ctx.AllSetItem() {
		siResult := v.visit(si)
		if err := firstError(siResult); err != nil {
			return err
		}
		if item, ok := siResult.(*ast.SetItem); ok {
			set.Items = append(set.Items, item)
		}
	}
	return set
}

// VisitSetItem handles one SET assignment.
//
// Three forms:
//  1. propertyExpr = expr          (property assignment)
//  2. variable = expr / variable += expr (variable assignment)
//  3. variable :Label1:Label2      (label assignment)
func (v *visitor) VisitSetItem(ctx *gen.SetItemContext) interface{} {
	item := &ast.SetItem{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}

	// Form 1: property = expr  (propertyExpression ASSIGN expression)
	if pe := ctx.PropertyExpression(); pe != nil {
		tgt, err := v.visitPropertyExpression(pe)
		if err != nil {
			return err
		}
		expr, err := asExpr(v.visit(ctx.Expression()))
		if err != nil {
			return &SemaError{Rule: "setItem", Pos: positionOf(ctx), Message: err.Error()}
		}
		if containsBareRelChainPattern(expr) {
			return &SemaError{
				Rule:    "setItem",
				Pos:     positionOf(ctx),
				Message: "relationship-chain pattern is not allowed as a SET right-hand side value",
			}
		}
		item.Target = tgt
		item.Value = expr
		item.Operator = "="
		return item
	}

	// Forms 2 & 3: variable-based (grammar uses Symbol, not Name)
	if symCtx := ctx.Symbol(); symCtx != nil {
		varName := symbolText(symCtx)
		item.Target = &ast.Variable{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Name: varName}

		// ASSIGN ('=') or ADD_ASSIGN ('+=') token?
		if ctx.ASSIGN() != nil {
			item.Operator = "="
			expr, err := asExpr(v.visit(ctx.Expression()))
			if err != nil {
				return &SemaError{Rule: "setItem", Pos: positionOf(ctx), Message: err.Error()}
			}
			if containsBareRelChainPattern(expr) {
				return &SemaError{
					Rule:    "setItem",
					Pos:     positionOf(ctx),
					Message: "relationship-chain pattern is not allowed as a SET right-hand side value",
				}
			}
			item.Value = expr
			return item
		}
		if ctx.ADD_ASSIGN() != nil {
			item.Operator = "+="
			expr, err := asExpr(v.visit(ctx.Expression()))
			if err != nil {
				return &SemaError{Rule: "setItem", Pos: positionOf(ctx), Message: err.Error()}
			}
			if containsBareRelChainPattern(expr) {
				return &SemaError{
					Rule:    "setItem",
					Pos:     positionOf(ctx),
					Message: "relationship-chain pattern is not allowed as a SET right-hand side value",
				}
			}
			item.Value = expr
			return item
		}

		// Label assignment: variable :Label1:Label2
		if nl := ctx.NodeLabels(); nl != nil {
			item.Labels = nodeLabels(nl)
			return item
		}
	}
	return unsupported(ctx, "setItem", "unrecognised set-item form")
}

// VisitRemoveSt handles REMOVE item, item, ….
func (v *visitor) VisitRemoveSt(ctx *gen.RemoveStContext) interface{} {
	rem := &ast.Remove{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	for _, ri := range ctx.AllRemoveItem() {
		riResult := v.visit(ri)
		if err := firstError(riResult); err != nil {
			return err
		}
		if item, ok := riResult.(*ast.RemoveItem); ok {
			rem.Items = append(rem.Items, item)
		}
	}
	return rem
}

// VisitRemoveItem handles one REMOVE item.
func (v *visitor) VisitRemoveItem(ctx *gen.RemoveItemContext) interface{} {
	item := &ast.RemoveItem{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}

	// Property removal: propertyExpression
	if pe := ctx.PropertyExpression(); pe != nil {
		tgt, err := v.visitPropertyExpression(pe)
		if err != nil {
			return err
		}
		item.Target = tgt
		return item
	}

	// Label removal: variable :Label1:Label2 (grammar uses Symbol)
	if symCtx := ctx.Symbol(); symCtx != nil {
		varName := symbolText(symCtx)
		item.Target = &ast.Variable{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Name: varName}
		if nl := ctx.NodeLabels(); nl != nil {
			item.Labels = nodeLabels(nl)
		}
		return item
	}
	return unsupported(ctx, "removeItem", "unrecognised remove-item form")
}

// VisitDeleteSt handles [DETACH] DELETE expr, expr, ….
func (v *visitor) VisitDeleteSt(ctx *gen.DeleteStContext) interface{} {
	var exprs []ast.Expression
	if ec := ctx.ExpressionChain(); ec != nil {
		args, err := v.visitExpressionChain(ec)
		if err != nil {
			return err
		}
		exprs = args
	}
	if ctx.DETACH() != nil {
		return &ast.DetachDelete{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Expressions: exprs}
	}
	return &ast.Delete{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Expressions: exprs}
}

// -------------------------------------------------------------------------
// RETURN / WITH / projections
// -------------------------------------------------------------------------

// VisitReturnSt handles RETURN projectionBody.
func (v *visitor) VisitReturnSt(ctx *gen.ReturnStContext) interface{} {
	proj, err := v.visitProjectionBody(ctx.ProjectionBody())
	if err != nil {
		return err
	}
	return &ast.Return{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Projection: proj}
}

// VisitWithSt handles WITH projectionBody [WHERE expr].
func (v *visitor) VisitWithSt(ctx *gen.WithStContext) interface{} {
	proj, err := v.visitProjectionBody(ctx.ProjectionBody())
	if err != nil {
		return err
	}
	w := &ast.With{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Projection: proj}
	if wh := ctx.Where(); wh != nil {
		where, err := v.visitWhere(wh)
		if err != nil {
			return err
		}
		w.Where = where
	}
	return w
}

func (v *visitor) visitProjectionBody(ctx gen.IProjectionBodyContext) (*ast.Projection, error) {
	if ctx == nil {
		return nil, fmt.Errorf("missing projectionBody")
	}
	proj := &ast.Projection{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	proj.Distinct = ctx.DISTINCT() != nil

	items := ctx.ProjectionItems()
	if items == nil {
		return nil, fmt.Errorf("missing projectionItems")
	}
	res := v.visit(items)
	if err := firstError(res); err != nil {
		return nil, err
	}
	if pi, ok := res.(*projItems); ok {
		proj.All = pi.all
		proj.Items = pi.items
	}

	if ord := ctx.OrderSt(); ord != nil {
		orderResult := v.visit(ord)
		if err := firstError(orderResult); err != nil {
			return nil, err
		}
		if sortItems, ok := orderResult.([]*ast.SortItem); ok {
			proj.OrderBy = sortItems
		}
	}
	if sk := ctx.SkipSt(); sk != nil {
		expr, err := asExpr(v.visit(sk.Expression()))
		if err != nil {
			return nil, err
		}
		proj.Skip = expr
	}
	if lm := ctx.LimitSt(); lm != nil {
		expr, err := asExpr(v.visit(lm.Expression()))
		if err != nil {
			return nil, err
		}
		proj.Limit = expr
	}
	return proj, nil
}

// VisitProjectionItems handles * or item, item, ….
func (v *visitor) VisitProjectionItems(ctx *gen.ProjectionItemsContext) interface{} {
	if ctx.MULT() != nil {
		return &projItems{all: true}
	}
	pi := &projItems{}
	for _, item := range ctx.AllProjectionItem() {
		r := v.visit(item)
		if err := firstError(r); err != nil {
			return err
		}
		if pitem, ok := r.(*ast.ProjectionItem); ok {
			pi.items = append(pi.items, pitem)
		}
	}
	return pi
}

// VisitProjectionItem handles expr [AS alias].
func (v *visitor) VisitProjectionItem(ctx *gen.ProjectionItemContext) interface{} {
	expr, err := asExpr(v.visit(ctx.Expression()))
	if err != nil {
		return &SemaError{Rule: "projectionItem", Pos: positionOf(ctx), Message: err.Error()}
	}
	// A bare relationship-chain pattern is not a valid projection value.
	// The openCypher specification permits patterns only inside MATCH /
	// CREATE / MERGE, pattern comprehensions, and EXISTS{…}/COUNT{…}
	// subqueries; using one as a RETURN / WITH projection item or as a
	// function argument (`size((…)-[…]->…)`) must raise
	// `UnexpectedSyntax` at compile time.
	if containsBareRelChainPattern(expr) {
		return &SemaError{
			Rule:    "projectionItem",
			Pos:     positionOf(ctx),
			Message: "relationship-chain pattern is not allowed as a projection value; use EXISTS{…}, COUNT{…}, or a pattern comprehension instead",
		}
	}
	item := &ast.ProjectionItem{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Expr: expr}
	if symCtx := ctx.Symbol(); symCtx != nil {
		s := symbolText(symCtx)
		item.Alias = &s
	}
	return item
}

// VisitOrderSt handles ORDER BY items.
func (v *visitor) VisitOrderSt(ctx *gen.OrderStContext) interface{} {
	var items []*ast.SortItem
	for _, oi := range ctx.AllOrderItem() {
		r := v.visit(oi)
		if err := firstError(r); err != nil {
			return err
		}
		if si, ok := r.(*ast.SortItem); ok {
			items = append(items, si)
		}
	}
	return items
}

// VisitOrderItem handles expr [ASC|DESC].
func (v *visitor) VisitOrderItem(ctx *gen.OrderItemContext) interface{} {
	expr, err := asExpr(v.visit(ctx.Expression()))
	if err != nil {
		return &SemaError{Rule: "orderItem", Pos: positionOf(ctx), Message: err.Error()}
	}
	// DESC or DESCENDING means descending; everything else is ascending.
	desc := ctx.DESC() != nil || ctx.DESCENDING() != nil
	return &ast.SortItem{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Expr: expr, Descending: desc}
}

// VisitSkipSt / VisitLimitSt — the expression is pulled by the parent.
func (v *visitor) VisitSkipSt(ctx *gen.SkipStContext) interface{}   { return v.visit(ctx.Expression()) }
func (v *visitor) VisitLimitSt(ctx *gen.LimitStContext) interface{} { return v.visit(ctx.Expression()) }

// -------------------------------------------------------------------------
// WHERE
// -------------------------------------------------------------------------

// VisitWhere handles WHERE expr.
func (v *visitor) VisitWhere(ctx *gen.WhereContext) interface{} {
	expr, err := asExpr(v.visit(ctx.Expression()))
	if err != nil {
		return &SemaError{Rule: "where", Pos: positionOf(ctx), Message: err.Error()}
	}
	return &ast.Where{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Predicate: expr}
}

func (v *visitor) visitWhere(ctx gen.IWhereContext) (*ast.Where, error) {
	if ctx == nil {
		return nil, nil
	}
	r := v.visit(ctx)
	if err := firstError(r); err != nil {
		return nil, err
	}
	if w, ok := r.(*ast.Where); ok {
		return w, nil
	}
	return nil, nil
}

// -------------------------------------------------------------------------
// Pattern helpers
// -------------------------------------------------------------------------

type patternWhereResult struct {
	Pattern *ast.Pattern
	Where   *ast.Where
}

func (v *visitor) visitPatternWhere(ctx gen.IPatternWhereContext) (*patternWhereResult, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil patternWhere")
	}
	pat, err := v.visitPattern(ctx.Pattern())
	if err != nil {
		return nil, err
	}
	where, err := v.visitWhere(ctx.Where())
	if err != nil {
		return nil, err
	}
	return &patternWhereResult{Pattern: pat, Where: where}, nil
}

// VisitPatternWhere is the visitor entry for patternWhere rule.
func (v *visitor) VisitPatternWhere(ctx *gen.PatternWhereContext) interface{} {
	r, err := v.visitPatternWhere(ctx)
	if err != nil {
		return &SemaError{Rule: "patternWhere", Pos: positionOf(ctx), Message: err.Error()}
	}
	return r
}

// VisitPattern handles comma-separated patternParts.
func (v *visitor) VisitPattern(ctx *gen.PatternContext) interface{} {
	pat := &ast.Pattern{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	for _, pp := range ctx.AllPatternPart() {
		r := v.visit(pp)
		if err := firstError(r); err != nil {
			return err
		}
		if path, ok := r.(*ast.PathPattern); ok {
			pat.Paths = append(pat.Paths, path)
		}
	}
	return pat
}

func (v *visitor) visitPattern(ctx gen.IPatternContext) (*ast.Pattern, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil pattern")
	}
	r := v.visit(ctx)
	if err := firstError(r); err != nil {
		return nil, err
	}
	if pat, ok := r.(*ast.Pattern); ok {
		return pat, nil
	}
	return nil, fmt.Errorf("expected *ast.Pattern, got %T", r)
}

// VisitPatternPart handles [variable =] patternElem.
func (v *visitor) VisitPatternPart(ctx *gen.PatternPartContext) interface{} {
	pp := &ast.PathPattern{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	if sym := ctx.Symbol(); sym != nil {
		s := symbolText(sym)
		pp.Variable = &s
	}
	r := v.visit(ctx.PatternElem())
	if err := firstError(r); err != nil {
		return err
	}
	if head, ok := r.(*ast.PathElement); ok {
		pp.Head = head
	}
	return pp
}

func (v *visitor) visitPatternPart(ctx gen.IPatternPartContext) (*ast.PathPattern, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil patternPart")
	}
	r := v.visit(ctx)
	if err := firstError(r); err != nil {
		return nil, err
	}
	if pp, ok := r.(*ast.PathPattern); ok {
		return pp, nil
	}
	return nil, fmt.Errorf("expected *ast.PathPattern, got %T", r)
}

// VisitPatternElem builds a linked list of PathElement nodes.
//
// Grammar: patternElem = nodePattern (patternElemChain)*
//
//	| '(' patternElem ')'
func (v *visitor) VisitPatternElem(ctx *gen.PatternElemContext) interface{} {
	// Parenthesised form: recurse.
	if inner := ctx.PatternElem(); inner != nil {
		return v.visit(inner)
	}

	np := ctx.NodePattern()
	if np == nil {
		return unsupported(ctx, "patternElem", "missing node pattern")
	}
	nodeR := v.visit(np)
	if err := firstError(nodeR); err != nil {
		return err
	}
	nodePat, ok := nodeR.(*ast.NodePattern)
	if !ok {
		return unsupported(ctx, "patternElem", "expected NodePattern")
	}

	head := &ast.PathElement{Node: nodePat}
	cur := head

	for _, chain := range ctx.AllPatternElemChain() {
		cr := v.visit(chain)
		if err := firstError(cr); err != nil {
			return err
		}
		chainElem, ok := cr.(*ast.PathElement)
		if !ok {
			continue
		}
		cur.Next = chainElem
		cur = chainElem
	}
	return head
}

// VisitPatternElemChain handles relPattern nodePattern.
func (v *visitor) VisitPatternElemChain(ctx *gen.PatternElemChainContext) interface{} {
	relR := v.visit(ctx.RelationshipPattern())
	if err := firstError(relR); err != nil {
		return err
	}
	rel, ok := relR.(*ast.RelationshipPattern)
	if !ok {
		return unsupported(ctx, "patternElemChain", "expected RelationshipPattern")
	}

	nodeR := v.visit(ctx.NodePattern())
	if err := firstError(nodeR); err != nil {
		return err
	}
	node, ok := nodeR.(*ast.NodePattern)
	if !ok {
		return unsupported(ctx, "patternElemChain", "expected NodePattern")
	}
	return &ast.PathElement{Relationship: rel, Node: node}
}

// VisitNodePattern handles (variable? labels? properties?).
func (v *visitor) VisitNodePattern(ctx *gen.NodePatternContext) interface{} {
	np := &ast.NodePattern{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	if sym := ctx.Symbol(); sym != nil {
		s := symbolText(sym)
		np.Variable = &s
	}
	if nl := ctx.NodeLabels(); nl != nil {
		np.Labels = nodeLabels(nl)
	}
	if props := ctx.Properties(); props != nil {
		propR := v.visit(props)
		if err := firstError(propR); err != nil {
			return err
		}
		if e, ok := propR.(ast.Expression); ok {
			np.Properties = e
		}
	}
	return np
}

// VisitRelationshipPattern handles -[relDetail]-> or <-[relDetail]- or -[relDetail]-.
func (v *visitor) VisitRelationshipPattern(ctx *gen.RelationshipPatternContext) interface{} {
	rp := &ast.RelationshipPattern{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}

	// Direction: LT present before SUB = incoming; GT present = outgoing.
	// The `<-->` shape (both LT and GT) parses as undirected — openCypher
	// implementations treat it that way for MATCH; CREATE-specific sema
	// rejects undirected relationships separately via checkCreateRelationshipTypes.
	hasLT := ctx.LT() != nil
	hasGT := ctx.GT() != nil
	switch {
	case hasLT && hasGT:
		rp.Direction = ast.RelDirectionNone
	case hasLT:
		rp.Direction = ast.RelDirectionIncoming
	case hasGT:
		rp.Direction = ast.RelDirectionOutgoing
	default:
		rp.Direction = ast.RelDirectionNone
	}

	if rd := ctx.RelationDetail(); rd != nil {
		r := v.visit(rd)
		if err := firstError(r); err != nil {
			return err
		}
		if detail, ok := r.(*relDetail); ok {
			rp.Variable = detail.variable
			rp.Types = detail.types
			rp.Range = detail.rangeQ
			rp.Properties = detail.properties
		}
	}
	return rp
}

// relDetail is an internal transfer object for VisitRelationDetail.
type relDetail struct {
	variable   *string
	types      []string
	rangeQ     *ast.RangeQuantifier
	properties ast.Expression
}

// VisitRelationDetail parses [variable? :types? range? properties?].
func (v *visitor) VisitRelationDetail(ctx *gen.RelationDetailContext) interface{} {
	d := &relDetail{}
	if sym := ctx.Symbol(); sym != nil {
		s := symbolText(sym)
		d.variable = &s
	}
	if rt := ctx.RelationshipTypes(); rt != nil {
		d.types = relationshipTypes(rt)
	}
	if rl := ctx.RangeLit(); rl != nil {
		rq, err := v.visitRangeLit(rl)
		if err != nil {
			return err
		}
		d.rangeQ = rq
	}
	if props := ctx.Properties(); props != nil {
		propR := v.visit(props)
		if err := firstError(propR); err != nil {
			return err
		}
		if e, ok := propR.(ast.Expression); ok {
			d.properties = e
		}
	}
	return d
}

// VisitRelationshipTypes returns the type list.
func (v *visitor) VisitRelationshipTypes(ctx *gen.RelationshipTypesContext) interface{} {
	return relationshipTypes(ctx)
}

// VisitProperties dispatches to mapLit or parameter.
func (v *visitor) VisitProperties(ctx *gen.PropertiesContext) interface{} {
	if ml := ctx.MapLit(); ml != nil {
		return v.visit(ml)
	}
	if p := ctx.Parameter(); p != nil {
		return v.visit(p)
	}
	return unsupported(ctx, "properties", "expected map literal or parameter")
}

// VisitNodeLabels returns the label list. Internal helper: see nodeLabels().
func (v *visitor) VisitNodeLabels(ctx *gen.NodeLabelsContext) interface{} {
	return nodeLabels(ctx)
}

// VisitRangeLit handles *min?..max?.
func (v *visitor) VisitRangeLit(ctx *gen.RangeLitContext) interface{} {
	rq, err := v.visitRangeLit(ctx)
	if err != nil {
		return err
	}
	return rq
}

func (v *visitor) visitRangeLit(ctx gen.IRangeLitContext) (*ast.RangeQuantifier, error) {
	if ctx == nil {
		return nil, nil
	}
	rq := &ast.RangeQuantifier{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}

	// normalizeVarlenBounds pre-processes unsigned integer bounds to their
	// negated form (e.g. "*1..3" → "*-1..-3") so that the lexer emits DIGIT
	// tokens instead of ID tokens. ctx.GetText() therefore returns strings like
	// "*-2", "*-1..-3", "*..-3", "*-1..", "*.." — always starting with "*".
	// We parse bounds from the raw text and take abs() to recover the original
	// positive hop count.
	text := ctx.GetText() // e.g. "*-2", "*-1..-3", "*..-3", "*-1..", "*.."
	inner := strings.TrimPrefix(text, "*")

	hasRange := strings.Contains(inner, "..")
	if !hasRange {
		// *n  — fixed length or bare *.
		if inner == "" {
			// bare *  — unbounded in both directions
			return rq, nil
		}
		n, err := strconv.ParseInt(inner, 10, 64)
		if err != nil {
			return nil, &SemaError{Rule: "rangeLit", Pos: rq.Pos, Message: "invalid range bound: " + inner}
		}
		if n < 0 {
			n = -n
		}
		rq.Min = &n
		rq.Max = &n
		return rq, nil
	}

	// *min?..max?
	parts := strings.SplitN(inner, "..", 2)
	if len(parts) == 2 {
		if parts[0] != "" {
			n, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				return nil, &SemaError{Rule: "rangeLit", Pos: rq.Pos, Message: "invalid range min: " + parts[0]}
			}
			if n < 0 {
				n = -n
			}
			rq.Min = &n
		}
		if parts[1] != "" {
			n, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return nil, &SemaError{Rule: "rangeLit", Pos: rq.Pos, Message: "invalid range max: " + parts[1]}
			}
			if n < 0 {
				n = -n
			}
			rq.Max = &n
		}
	}
	return rq, nil
}

// -------------------------------------------------------------------------
// Expression rules
// -------------------------------------------------------------------------

// VisitExpression handles expr (OR xorExpr)*.
func (v *visitor) VisitExpression(ctx *gen.ExpressionContext) interface{} {
	xors := ctx.AllXorExpression()
	if len(xors) == 0 {
		return unsupported(ctx, "expression", "empty expression")
	}
	left, err := asExpr(v.visit(xors[0]))
	if err != nil {
		return &SemaError{Rule: "expression", Pos: positionOf(ctx), Message: err.Error()}
	}
	for i := 1; i < len(xors); i++ {
		right, err := asExpr(v.visit(xors[i]))
		if err != nil {
			return &SemaError{Rule: "expression", Pos: positionOf(ctx), Message: err.Error()}
		}
		left = &ast.BinaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Left: left, Operator: "OR", Right: right}
	}
	return left
}

// VisitXorExpression handles xorExpr (XOR andExpr)*.
func (v *visitor) VisitXorExpression(ctx *gen.XorExpressionContext) interface{} {
	ands := ctx.AllAndExpression()
	if len(ands) == 0 {
		return unsupported(ctx, "xorExpression", "empty xorExpression")
	}
	left, err := asExpr(v.visit(ands[0]))
	if err != nil {
		return &SemaError{Rule: "xorExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	for i := 1; i < len(ands); i++ {
		right, err := asExpr(v.visit(ands[i]))
		if err != nil {
			return &SemaError{Rule: "xorExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		left = &ast.BinaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Left: left, Operator: "XOR", Right: right}
	}
	return left
}

// VisitAndExpression handles andExpr (AND notExpr)*.
func (v *visitor) VisitAndExpression(ctx *gen.AndExpressionContext) interface{} {
	nots := ctx.AllNotExpression()
	if len(nots) == 0 {
		return unsupported(ctx, "andExpression", "empty andExpression")
	}
	left, err := asExpr(v.visit(nots[0]))
	if err != nil {
		return &SemaError{Rule: "andExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	for i := 1; i < len(nots); i++ {
		right, err := asExpr(v.visit(nots[i]))
		if err != nil {
			return &SemaError{Rule: "andExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		left = &ast.BinaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Left: left, Operator: "AND", Right: right}
	}
	return left
}

// VisitNotExpression handles NOT* comparisonExpression.
func (v *visitor) VisitNotExpression(ctx *gen.NotExpressionContext) interface{} {
	inner, err := asExpr(v.visit(ctx.ComparisonExpression()))
	if err != nil {
		return &SemaError{Rule: "notExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	if ctx.NOT() != nil {
		return &ast.UnaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Operator: "NOT", Operand: inner}
	}
	return inner
}

// VisitComparisonExpression handles addSub (cmpSign addSub)*.
//
// Multiple chained comparison operators (`a < b < c`) lower to the
// AND-joined pairwise comparisons (`a < b AND b < c`), matching the
// openCypher 9 chained-comparison semantics. The intermediate operand
// (`b`) is duplicated in the AST; this is safe for pure references
// (variables, properties) which is the only legal shape for chained
// comparisons per the spec.
func (v *visitor) VisitComparisonExpression(ctx *gen.ComparisonExpressionContext) interface{} {
	adds := ctx.AllAddSubExpression()
	cmps := ctx.AllComparisonSigns()
	if len(adds) == 0 {
		return unsupported(ctx, "comparisonExpression", "empty")
	}
	operands := make([]ast.Expression, len(adds))
	for i, add := range adds {
		e, err := asExpr(v.visit(add))
		if err != nil {
			return &SemaError{Rule: "comparisonExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		operands[i] = e
	}
	if len(cmps) == 0 {
		return operands[0]
	}
	// Single comparison: legacy fast path.
	if len(cmps) == 1 {
		return &ast.BinaryOp{
			Pos:      positionOf(ctx),
			EndPos:   endPositionOf(ctx),
			Left:     operands[0],
			Operator: comparisonOp(cmps[0]),
			Right:    operands[1],
		}
	}
	// Chained: build (a OP1 b) AND (b OP2 c) AND ... left-associative.
	pos := positionOf(ctx)
	end := endPositionOf(ctx)
	var result ast.Expression = &ast.BinaryOp{
		Pos:      pos,
		EndPos:   end,
		Left:     operands[0],
		Operator: comparisonOp(cmps[0]),
		Right:    operands[1],
	}
	for i := 1; i < len(cmps); i++ {
		step := &ast.BinaryOp{
			Pos:      pos,
			EndPos:   end,
			Left:     operands[i],
			Operator: comparisonOp(cmps[i]),
			Right:    operands[i+1],
		}
		result = &ast.BinaryOp{
			Pos:      pos,
			EndPos:   end,
			Left:     result,
			Operator: "AND",
			Right:    step,
		}
	}
	return result
}

// VisitComparisonSigns returns the operator string (used by parent).
func (v *visitor) VisitComparisonSigns(ctx *gen.ComparisonSignsContext) interface{} {
	return comparisonOp(ctx)
}

// VisitAddSubExpression handles multDiv ([+-] multDiv)*.
func (v *visitor) VisitAddSubExpression(ctx *gen.AddSubExpressionContext) interface{} {
	mults := ctx.AllMultDivExpression()
	if len(mults) == 0 {
		return unsupported(ctx, "addSubExpression", "empty")
	}
	left, err := asExpr(v.visit(mults[0]))
	if err != nil {
		return &SemaError{Rule: "addSubExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	// Operators are interleaved as children; ANTLR provides AllPLUS / AllSUB
	// but their indices don't directly correspond to pairs. We reconstruct by
	// walking children in order.
	opIdx := 0
	operators := addSubOperators(ctx)
	for i := 1; i < len(mults); i++ {
		right, err := asExpr(v.visit(mults[i]))
		if err != nil {
			return &SemaError{Rule: "addSubExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		op := "+"
		if opIdx < len(operators) {
			op = operators[opIdx]
		}
		opIdx++
		bo := &ast.BinaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Left: left, Operator: op, Right: right}
		left = liftListPredicate(bo)
	}
	return left
}

// VisitMultDivExpression handles power ([*/%] power)*.
func (v *visitor) VisitMultDivExpression(ctx *gen.MultDivExpressionContext) interface{} {
	powers := ctx.AllPowerExpression()
	if len(powers) == 0 {
		return unsupported(ctx, "multDivExpression", "empty")
	}
	left, err := asExpr(v.visit(powers[0]))
	if err != nil {
		return &SemaError{Rule: "multDivExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	operators := multDivOperators(ctx)
	for i := 1; i < len(powers); i++ {
		right, err := asExpr(v.visit(powers[i]))
		if err != nil {
			return &SemaError{Rule: "multDivExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		op := "*"
		if i-1 < len(operators) {
			op = operators[i-1]
		}
		bo := &ast.BinaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Left: left, Operator: op, Right: right}
		left = liftListPredicate(bo)
	}
	return left
}

// VisitPowerExpression handles unary (^ unary)*.
func (v *visitor) VisitPowerExpression(ctx *gen.PowerExpressionContext) interface{} {
	unaries := ctx.AllUnaryAddSubExpression()
	if len(unaries) == 0 {
		return unsupported(ctx, "powerExpression", "empty")
	}
	left, err := asExpr(v.visit(unaries[0]))
	if err != nil {
		return &SemaError{Rule: "powerExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	for i := 1; i < len(unaries); i++ {
		right, err := asExpr(v.visit(unaries[i]))
		if err != nil {
			return &SemaError{Rule: "powerExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		bo := &ast.BinaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Left: left, Operator: "^", Right: right}
		left = liftListPredicate(bo)
	}
	return left
}

// VisitUnaryAddSubExpression handles [+-] atomicExpression.
func (v *visitor) VisitUnaryAddSubExpression(ctx *gen.UnaryAddSubExpressionContext) interface{} {
	inner, err := asExpr(v.visit(ctx.AtomicExpression()))
	if err != nil {
		return &SemaError{Rule: "unaryAddSubExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	if ctx.SUB() != nil {
		return &ast.UnaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Operator: "-", Operand: inner}
	}
	// Unary + is a no-op; omit the wrapper.
	return inner
}

// VisitAtomicExpression handles propertyOrLabelExpr (stringExpr | listExpr | nullExpr)*.
func (v *visitor) VisitAtomicExpression(ctx *gen.AtomicExpressionContext) interface{} {
	base, err := asExpr(v.visit(ctx.PropertyOrLabelExpression()))
	if err != nil {
		return &SemaError{Rule: "atomicExpression", Pos: positionOf(ctx), Message: err.Error()}
	}

	// Apply postfix string predicates (STARTS WITH, ENDS WITH, CONTAINS).
	for _, se := range ctx.AllStringExpression() {
		r := v.visit(se)
		if err := firstError(r); err != nil {
			return err
		}
		// stringExpression returns a partial BinaryOp with nil Left; fill it in.
		if partial, ok := r.(*ast.BinaryOp); ok {
			partial.Left = base
			base = partial
		}
	}

	// Apply IN / subscript / slice. Subscripts and slices that
	// immediately follow an IN clause apply to the IN's right operand,
	// not to the (lhs IN rhs) result — `3 IN list[0]` parses as
	// `3 IN (list[0])`, matching the openCypher precedence rule that
	// list/subscript operators bind tighter than the IN comparison.
	listExprs := ctx.AllListExpression()
	i := 0
	for i < len(listExprs) {
		r := v.visit(listExprs[i])
		if err := firstError(r); err != nil {
			return err
		}
		switch e := r.(type) {
		case *listInExpr:
			// Consume any trailing subscripts/slices that operate on
			// the IN's right operand before wrapping the whole thing.
			rhs := e.list
			j := i + 1
			for j < len(listExprs) {
				inner := v.visit(listExprs[j])
				if err := firstError(inner); err != nil {
					return err
				}
				sub, isSub := inner.(*subscriptOrSlice)
				if !isSub {
					break
				}
				if sub.isSlice {
					rhs = &ast.SliceExpr{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Expr: rhs, From: sub.from, To: sub.to}
				} else {
					rhs = &ast.SubscriptExpr{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Expr: rhs, Index: sub.index}
				}
				j++
			}
			base = &ast.BinaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Left: base, Operator: "IN", Right: rhs}
			i = j
			continue
		case *subscriptOrSlice:
			if e.isSlice {
				base = &ast.SliceExpr{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Expr: base, From: e.from, To: e.to}
			} else {
				base = &ast.SubscriptExpr{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Expr: base, Index: e.index}
			}
		}
		i++
	}

	// Apply IS NULL / IS NOT NULL.
	for _, ne := range ctx.AllNullExpression() {
		r := v.visit(ne)
		if err := firstError(r); err != nil {
			return err
		}
		if op, ok := r.(string); ok {
			base = &ast.UnaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Operator: op, Operand: base}
		}
	}

	return base
}

// VisitListExpression handles IN expr | [expr?..expr?] | [expr].
func (v *visitor) VisitListExpression(ctx *gen.ListExpressionContext) interface{} {
	if ctx.IN() != nil {
		// IN propertyOrLabelExpression
		right, err := asExpr(v.visit(ctx.PropertyOrLabelExpression()))
		if err != nil {
			return &SemaError{Rule: "listExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		return &listInExpr{list: right}
	}
	// [ … ] form: slice or subscript
	exprs := ctx.AllExpression()
	if ctx.RANGE() != nil {
		// Slice: [from?..to?]
		var from, to ast.Expression
		// Determine from / to by position: text before ".." is from, after is to.
		text := ctx.GetText()
		// Strip surrounding brackets
		inner := strings.TrimPrefix(strings.TrimSuffix(text, "]"), "[")
		idx := strings.Index(inner, "..")
		hasBefore := idx > 0
		hasAfter := idx >= 0 && idx < len(inner)-2

		if hasBefore && len(exprs) > 0 {
			var err error
			from, err = asExpr(v.visit(exprs[0]))
			if err != nil {
				return &SemaError{Rule: "listExpression", Pos: positionOf(ctx), Message: err.Error()}
			}
		}
		if hasAfter {
			exprIdx := 0
			if hasBefore {
				exprIdx = 1
			}
			if exprIdx < len(exprs) {
				var err error
				to, err = asExpr(v.visit(exprs[exprIdx]))
				if err != nil {
					return &SemaError{Rule: "listExpression", Pos: positionOf(ctx), Message: err.Error()}
				}
			}
		}
		return &subscriptOrSlice{isSlice: true, from: from, to: to}
	}
	// Subscript: [expr]
	if len(exprs) == 1 {
		idx, err := asExpr(v.visit(exprs[0]))
		if err != nil {
			return &SemaError{Rule: "listExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		return &subscriptOrSlice{isSlice: false, index: idx}
	}
	return unsupported(ctx, "listExpression", "unexpected form")
}

// listInExpr is an internal transfer type for the IN rhs.
type listInExpr struct{ list ast.Expression }

// subscriptOrSlice is an internal transfer type.
type subscriptOrSlice struct {
	isSlice bool
	from    ast.Expression
	to      ast.Expression
	index   ast.Expression
}

// VisitStringExpression returns a partial *BinaryOp with nil Left (filled by parent).
func (v *visitor) VisitStringExpression(ctx *gen.StringExpressionContext) interface{} {
	pfx := ctx.StringExpPrefix()
	if pfx == nil {
		return unsupported(ctx, "stringExpression", "missing prefix")
	}
	op := stringPrefixOp(pfx)
	right, err := asExpr(v.visit(ctx.PropertyOrLabelExpression()))
	if err != nil {
		return &SemaError{Rule: "stringExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	return &ast.BinaryOp{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Operator: op, Right: right}
}

// VisitStringExpPrefix returns the operator string.
func (v *visitor) VisitStringExpPrefix(ctx *gen.StringExpPrefixContext) interface{} {
	return stringPrefixOp(ctx)
}

// VisitNullExpression returns "IS NULL" or "IS NOT NULL".
func (v *visitor) VisitNullExpression(ctx *gen.NullExpressionContext) interface{} {
	if ctx.NOT() != nil {
		return "IS NOT NULL"
	}
	return "IS NULL"
}

// VisitPropertyOrLabelExpression handles atom (labelFilter | propertyAccess)*.
//
// The grammar rule produces either or both of:
//
//   - PropertyExpression: atom + dot-access chain (e.g. `n.name`, `a.b.c`).
//   - NodeLabels: trailing label filter (`:Foo:Bar`).
//
// When NodeLabels are present they wrap the (possibly empty) property
// chain in an ast.LabelPredicate so the predicate `n:Foo` evaluates to
// the right TRUE / FALSE / NULL at run-time. A bare property chain is
// returned verbatim.
func (v *visitor) VisitPropertyOrLabelExpression(ctx *gen.PropertyOrLabelExpressionContext) interface{} {
	pe := ctx.PropertyExpression()
	if pe == nil {
		return unsupported(ctx, "propertyOrLabelExpression", "missing propertyExpression")
	}
	base, err := v.visitPropertyExpression(pe)
	if err != nil {
		return &SemaError{Rule: "propertyOrLabelExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	if nl := ctx.NodeLabels(); nl != nil {
		labels := nodeLabels(nl)
		if len(labels) > 0 {
			return &ast.LabelPredicate{
				Pos:      positionOf(ctx),
				EndPos:   endPositionOf(ctx),
				Receiver: base,
				Labels:   labels,
			}
		}
	}
	return base
}

func (v *visitor) visitPropertyExpression(ctx gen.IPropertyExpressionContext) (ast.Expression, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil propertyExpression")
	}
	r := v.visit(ctx.Atom())
	if err := firstError(r); err != nil {
		return nil, err
	}
	base, err := asExpr(r)
	if err != nil {
		return nil, err
	}

	// Chain of .name accessors.
	names := ctx.AllName()

	// Special case (T937): ANTLR's lexer tokenises a float literal such as
	// `1.0` as the three tokens `1`, `.`, `0` because the DIGIT terminal
	// matches the integer `1` greedily and the trailing `.0` falls into the
	// propertyExpression chain as a property access. The visitor sees
	// Atom=IntLiteral{1} + AllName=["0"] and would otherwise emit
	// Property{Receiver: IntLiteral{1}, Key: "0"} — a runtime null on any
	// evaluation path. Property keys in openCypher cannot start with a
	// digit (unless explicitly back-quoted), so a single accessor whose key
	// is all digits unambiguously reconstructs the float literal.
	if len(names) == 1 {
		key := nameText(names[0])
		// The fractional-part token may carry an exponent suffix too
		// (e.g. `1.0e9` lexes as 1 + . + 0e9), so accept either a
		// pure-digit key or a digit-run-with-exponent shape.
		isFracKey := isAllDigits(key) || looksLikeExponentFloat(key)
		if isFracKey {
			if intLit, ok := base.(*ast.IntLiteral); ok {
				// Recover a leading sign that was absorbed into the integer
				// token: int64 cannot represent -0, so an input like
				// `-0.001` reaches this branch as IntLit{Value: 0} and the
				// sign is lost when we format the float string. When the
				// integer's source text starts with '-' AND the fractional
				// key has a non-zero digit we re-add the sign so ParseFloat
				// produces the negative value the literal denotes. A
				// fractional key that is all zeros (e.g. `-0.0`,
				// `-0.000`) collapses to a positive zero per the
				// openCypher canonical form (signed zero is not exposed at
				// the literal level). The non-zero-integer branch keeps
				// its sign in intLit.Value already.
				sign := ""
				if intLit.Value == 0 && ctx.Atom() != nil &&
					strings.HasPrefix(ctx.Atom().GetText(), "-") &&
					!isAllZeroDigits(key) {
					sign = "-"
				}
				f, ferr := strconv.ParseFloat(fmt.Sprintf("%s%d.%s", sign, intLit.Value, key), 64)
				if ferr == nil {
					return &ast.FloatLiteral{Pos: intLit.Pos, EndPos: endPositionOf(ctx), Value: f}, nil
				}
			}
			// When the integer part overflows int64, VisitNumLit returns an
			// OverflowIntLit sentinel. The subsequent `.0` (or `.0e9`)
			// accessor is the fractional part of a long float literal —
			// reconstruct the float64 by parsing the full "NNN.frac" form.
			if ovfLit, ok := base.(*ast.OverflowIntLit); ok {
				f, ferr := strconv.ParseFloat(ovfLit.Text+"."+key, 64)
				if ferr == nil {
					return &ast.FloatLiteral{Pos: ovfLit.Pos, EndPos: endPositionOf(ctx), Value: f}, nil
				}
			}
		}
	}

	// An OverflowIntLit that was NOT consumed by the float-reconstruction
	// block above is a pure integer literal whose value exceeds int64 range.
	// Surface it as an IntegerOverflow compile error per openCypher.
	if ovf, ok := base.(*ast.OverflowIntLit); ok {
		return nil, &SemaError{Rule: "numLit", Pos: ovf.Pos,
			Message: "integer literal out of range: " + ovf.Text}
	}

	for _, n := range names {
		key := nameText(n)
		base = &ast.Property{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Receiver: base, Key: key}
	}
	return base, nil
}

// isAllDigits reports whether s consists exclusively of ASCII decimal
// digits. An empty string returns false.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isAllZeroDigits reports whether s is a non-empty run of ASCII '0'
// characters. Used to detect canonical-zero fractional parts so a
// negated integer literal whose fraction collapses to zero (e.g.
// `-0.0`, `-0.000`) is normalised to a positive zero rather than
// IEEE-754 negative zero.
func isAllZeroDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// VisitPropertyExpression is the Visit method required by the interface.
func (v *visitor) VisitPropertyExpression(ctx *gen.PropertyExpressionContext) interface{} {
	e, err := v.visitPropertyExpression(ctx)
	if err != nil {
		return &SemaError{Rule: "propertyExpression", Pos: positionOf(ctx), Message: err.Error()}
	}
	return e
}

// -------------------------------------------------------------------------
// Atom
// -------------------------------------------------------------------------

// VisitAtom dispatches to the concrete atom variant.
//
//nolint:gocyclo // dispatch over all atom alternatives; complexity is inherent
func (v *visitor) VisitAtom(ctx *gen.AtomContext) interface{} {
	if l := ctx.Literal(); l != nil {
		return v.visit(l)
	}
	if p := ctx.Parameter(); p != nil {
		return v.visit(p)
	}
	if ce := ctx.CaseExpression(); ce != nil {
		return v.visit(ce)
	}
	if ca := ctx.CountAll(); ca != nil {
		return v.visit(ca)
	}
	if lc := ctx.ListComprehension(); lc != nil {
		return v.visit(lc)
	}
	if pc := ctx.PatternComprehension(); pc != nil {
		return v.visit(pc)
	}
	if fw := ctx.FilterWith(); fw != nil {
		return v.visit(fw)
	}
	if rcp := ctx.RelationshipsChainPattern(); rcp != nil {
		return v.visit(rcp)
	}
	if pe := ctx.ParenthesizedExpression(); pe != nil {
		return v.visit(pe)
	}
	if fi := ctx.FunctionInvocation(); fi != nil {
		return v.visit(fi)
	}
	if sym := ctx.Symbol(); sym != nil {
		name := symbolText(sym)
		// Detect hex (0x…/0X…) and octal (0o…/0O…) integer literals that overflow
		// int64. The ANTLR lexer tokenises these as ID rather than DIGIT, so they
		// reach VisitAtom as symbol tokens. When the literal is valid and fits in
		// int64, return an IntLiteral; when it overflows, return a SemaError so
		// that the caller reports a compile-time IntegerOverflow.
		if strings.HasPrefix(name, "0x") || strings.HasPrefix(name, "0X") ||
			strings.HasPrefix(name, "0o") || strings.HasPrefix(name, "0O") {
			n, err := parseInt(name)
			if err != nil {
				return &SemaError{Rule: "atom", Pos: positionOf(ctx), Message: "integer literal out of range: " + name}
			}
			return &ast.IntLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Value: n}
		}
		// Detect scientific-notation float literals such as `1e9`, `1E9`,
		// `1e-3`, `2E+10`. The ANTLR lexer's FLOAT rule accepts
		// `Digits ExponentPart` but in practice ANTLR's DIGIT/Symbol
		// dispatch tokenises these as ID for Symbol-position atoms
		// (e.g. RETURN 1e9 AS x), so they arrive here as a symbol that
		// begins with a digit and matches the exponent shape.
		// openCypher identifiers cannot begin with a digit, so the
		// reinterpretation is unambiguous.
		if name != "" && name[0] >= '0' && name[0] <= '9' && looksLikeExponentFloat(name) {
			f, ferr := strconv.ParseFloat(name, 64)
			if ferr == nil {
				return &ast.FloatLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Value: f}
			}
			// ErrRange means the literal is a well-formed exponent float that
			// overflows IEEE-754 double precision. Surface a FloatingPointOverflow
			// compile-time error per the openCypher TCK (Literals5 [27]).
			// Falling through to the Variable branch would turn "1e309" into a
			// variable named "1e309", which is both wrong and deceptive.
			if errors.Is(ferr, strconv.ErrRange) {
				return &SemaError{Rule: "atom", Pos: positionOf(ctx), Message: "floating-point literal out of range: " + name}
			}
		}

		// Detect malformed decimal integer literals that the ANTLR lexer
		// accepts as ID tokens because the LetterOrDigit class is broader than
		// the openCypher integer-literal grammar. A token that begins with a
		// decimal digit ([1-9]) but contains a non-numeric, non-float-suffix
		// letter (anything outside e/E/f/F/d/D) is not a valid identifier
		// (Cypher identifiers must begin with a letter or underscore) and is
		// not a valid numeric literal. The openCypher TCK expects such input
		// to raise an InvalidNumberLiteral compile-time error.
		if name != "" && name[0] >= '1' && name[0] <= '9' && hasInvalidNumericChar(name) {
			return &SemaError{Rule: "atom", Pos: positionOf(ctx), Message: "invalid number literal: " + name}
		}
		return &ast.Variable{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Name: name}
	}
	if se := ctx.SubqueryExist(); se != nil {
		return v.visit(se)
	}
	if sc := ctx.SubqueryCount(); sc != nil {
		return v.visit(sc)
	}
	return unsupported(ctx, "atom", "unknown atom variant")
}

// VisitLhs handles the left-hand side of a pattern comprehension.
// The name VisitLhs (not VisitLHS) is mandated by the generated CypherParserVisitor interface.
//
//nolint:revive // method name matches generated interface: gen.CypherParserVisitor.VisitLhs
func (v *visitor) VisitLhs(ctx *gen.LhsContext) interface{} {
	if sym := ctx.Symbol(); sym != nil {
		return symbolText(sym)
	}
	return ""
}

// VisitParenthesizedExpression returns the inner expression (no wrapper node).
// When the inner expression is a list/string predicate BinaryOp (IN, CONTAINS,
// STARTS WITH, ENDS WITH) the Parenthesized flag is set so the precedence-
// rebalancing pass run by [Parse] knows not to lift that predicate out of an
// enclosing arithmetic chain.
func (v *visitor) VisitParenthesizedExpression(ctx *gen.ParenthesizedExpressionContext) interface{} {
	inner := v.visit(ctx.Expression())
	if bo, ok := inner.(*ast.BinaryOp); ok {
		switch bo.Operator {
		case "IN", "CONTAINS", "STARTS WITH", "ENDS WITH":
			bo.Parenthesized = true
		}
	}
	return inner
}

// VisitCountAll returns a FunctionInvocation for COUNT(*).
// CountStar is set to true so that String() produces "count(*)" —
// matching the TCK column-header convention — without requiring a
// StarLiteral argument that would break downstream arg evaluation.
func (v *visitor) VisitCountAll(ctx *gen.CountAllContext) interface{} {
	return &ast.FunctionInvocation{
		Pos:       positionOf(ctx),
		EndPos:    endPositionOf(ctx),
		Name:      "count",
		CountStar: true,
		Args:      nil,
	}
}

// VisitFunctionInvocation handles namespace.func(DISTINCT? args).
func (v *visitor) VisitFunctionInvocation(ctx *gen.FunctionInvocationContext) interface{} {
	fi := &ast.FunctionInvocation{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	if in := ctx.InvocationName(); in != nil {
		syms := in.AllSymbol()
		for i, s := range syms {
			name := symbolText(s)
			if i < len(syms)-1 {
				fi.Namespace = append(fi.Namespace, name)
			} else {
				fi.Name = name
			}
		}
	}
	fi.Distinct = ctx.DISTINCT() != nil
	if ec := ctx.ExpressionChain(); ec != nil {
		args, err := v.visitExpressionChain(ec)
		if err != nil {
			return err
		}
		fi.Args = args
	}
	return fi
}

// VisitInvocationName is only called transitively; handled inline above.
func (v *visitor) VisitInvocationName(ctx *gen.InvocationNameContext) interface{} {
	// Return the symbol texts as []string; callers use AllSymbol directly.
	syms := ctx.AllSymbol()
	parts := make([]string, 0, len(syms))
	for _, s := range syms {
		parts = append(parts, symbolText(s))
	}
	return parts
}

// -------------------------------------------------------------------------
// Subquery forms
// -------------------------------------------------------------------------

// VisitSubqueryExist handles EXISTS { … }.
func (v *visitor) VisitSubqueryExist(ctx *gen.SubqueryExistContext) interface{} {
	if rq := ctx.RegularQuery(); rq != nil {
		qr := v.visit(rq)
		if err := firstError(qr); err != nil {
			return err
		}
		// EXISTS { fullQuery } — convert to ExistsSubquery with Query field.
		switch q := qr.(type) {
		case *ast.SingleQuery:
			if err := existsSubqueryHasUpdateClause(q); err != nil {
				return &SemaError{Rule: "subqueryExist", Pos: positionOf(ctx), Message: err.Error()}
			}
			return &ast.ExistsSubquery{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Query: q}
		case *ast.MultiQuery:
			// Wrap first part for simplicity; multi-union inside EXISTS is unusual.
			if len(q.Parts) > 0 {
				if err := existsSubqueryHasUpdateClause(q.Parts[0]); err != nil {
					return &SemaError{Rule: "subqueryExist", Pos: positionOf(ctx), Message: err.Error()}
				}
				return &ast.ExistsSubquery{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Query: q.Parts[0]}
			}
		}
	}
	if pw := ctx.PatternWhere(); pw != nil {
		pat, err := v.visitPatternWhere(pw)
		if err != nil {
			return err
		}
		// Build a minimal single-match query for the pattern form. The
		// inline WHERE (if present) is preserved on the ExistsSubquery
		// so the IR translator can attach it as a Selection inside the
		// SemiApply/AntiSemiApply subtree. Without this the WHERE would
		// be silently dropped and EXISTS { ... WHERE p } would behave
		// identically to EXISTS { ... } (per ExistentialSubquery1 [2]).
		return &ast.ExistsSubquery{
			Pos:     positionOf(ctx),
			EndPos:  endPositionOf(ctx),
			Pattern: pat.Pattern,
			Where:   pat.Where,
		}
	}
	return unsupported(ctx, "subqueryExist", "unrecognised EXISTS form")
}

// existsSubqueryHasUpdateClause reports whether sq contains an updating
// clause (CREATE, MERGE, SET, REMOVE, DELETE/DETACH DELETE). EXISTS { … }
// is a read-only existence check, so any embedded update is rejected at
// compile time per openCypher 9 §3.4.7 (InvalidClauseComposition).
func existsSubqueryHasUpdateClause(sq *ast.SingleQuery) error {
	if sq == nil {
		return nil
	}
	for _, c := range sq.UpdatingClauses {
		switch c.(type) {
		case *ast.Create, *ast.Merge, *ast.Set, *ast.Remove, *ast.Delete:
			return fmt.Errorf("InvalidClauseComposition: EXISTS subquery cannot contain update clauses")
		}
	}
	return nil
}

// VisitSubqueryCount handles COUNT { … }. The shape mirrors EXISTS { … }
// exactly — both subquery forms accept either a regularQuery or a
// patternWhere body — but the resulting AST node is [ast.CountSubquery]
// instead of [ast.ExistsSubquery] so the IR/executor can dispatch the
// row-count semantics rather than the existence-check semantics.
func (v *visitor) VisitSubqueryCount(ctx *gen.SubqueryCountContext) interface{} {
	if rq := ctx.RegularQuery(); rq != nil {
		qr := v.visit(rq)
		if err := firstError(qr); err != nil {
			return err
		}
		switch q := qr.(type) {
		case *ast.SingleQuery:
			return &ast.CountSubquery{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Query: q}
		case *ast.MultiQuery:
			if len(q.Parts) > 0 {
				return &ast.CountSubquery{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Query: q.Parts[0]}
			}
		}
	}
	if pw := ctx.PatternWhere(); pw != nil {
		pat, err := v.visitPatternWhere(pw)
		if err != nil {
			return err
		}
		return &ast.CountSubquery{
			Pos:     positionOf(ctx),
			EndPos:  endPositionOf(ctx),
			Pattern: pat.Pattern,
		}
	}
	return unsupported(ctx, "subqueryCount", "unrecognised COUNT form")
}

// -------------------------------------------------------------------------
// CASE expression
// -------------------------------------------------------------------------

// VisitCaseExpression handles CASE [subject] (WHEN cond THEN expr)+ [ELSE e] END.
//
// The grammar stores all expressions in AllExpression(); their mapping to
// WHEN/THEN/ELSE positions requires walking tokens by count.
func (v *visitor) VisitCaseExpression(ctx *gen.CaseExpressionContext) interface{} {
	ce := &ast.CaseExpression{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	exprs := ctx.AllExpression()
	whenCount := len(ctx.AllWHEN())
	thenCount := len(ctx.AllTHEN())
	hasElse := ctx.ELSE() != nil

	// expr count = [subject?] + whenCount + thenCount + [else?]
	// subject present when: len(exprs) > whenCount + thenCount + (1 if hasElse)
	expectedMin := whenCount + thenCount
	if hasElse {
		expectedMin++
	}
	hasSubject := len(exprs) > expectedMin

	idx := 0
	if hasSubject {
		subj, err := asExpr(v.visit(exprs[idx]))
		if err != nil {
			return &SemaError{Rule: "caseExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		ce.Subject = subj
		idx++
	}

	for i := 0; i < whenCount && i < thenCount; i++ {
		cond, err := asExpr(v.visit(exprs[idx]))
		if err != nil {
			return &SemaError{Rule: "caseExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		idx++
		cons, err := asExpr(v.visit(exprs[idx]))
		if err != nil {
			return &SemaError{Rule: "caseExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		idx++
		ce.Alternatives = append(ce.Alternatives, &ast.CaseAlternative{
			Pos:        positionOf(ctx),
			EndPos:     endPositionOf(ctx),
			Condition:  cond,
			Consequent: cons,
		})
	}

	if hasElse && idx < len(exprs) {
		el, err := asExpr(v.visit(exprs[idx]))
		if err != nil {
			return &SemaError{Rule: "caseExpression", Pos: positionOf(ctx), Message: err.Error()}
		}
		ce.ElseExpr = el
	}
	return ce
}

// -------------------------------------------------------------------------
// List / Pattern comprehensions
// -------------------------------------------------------------------------

// VisitListComprehension handles [filterExpr | projection?].
func (v *visitor) VisitListComprehension(ctx *gen.ListComprehensionContext) interface{} {
	fe := ctx.FilterExpression()
	if fe == nil {
		return unsupported(ctx, "listComprehension", "missing filterExpression")
	}
	varName, src, pred, err := v.visitFilterExpression(fe)
	if err != nil {
		return err
	}
	lc := &ast.ListComprehension{
		Pos:       positionOf(ctx),
		EndPos:    endPositionOf(ctx),
		Variable:  varName,
		Source:    src,
		Predicate: pred,
	}
	if ctx.STICK() != nil {
		proj, err := asExpr(v.visit(ctx.Expression()))
		if err != nil {
			return &SemaError{Rule: "listComprehension", Pos: positionOf(ctx), Message: err.Error()}
		}
		lc.Projection = proj
	}
	return lc
}

// VisitFilterExpression is the visitor entry; internal logic in visitFilterExpression.
func (v *visitor) VisitFilterExpression(ctx *gen.FilterExpressionContext) interface{} {
	varName, src, pred, err := v.visitFilterExpression(ctx)
	if err != nil {
		return err
	}
	// FilterExpression itself is only used by callers; return a struct.
	type filterExprResult struct {
		varName string
		src     ast.Expression
		pred    ast.Expression
	}
	return &filterExprResult{varName: varName, src: src, pred: pred}
}

func (v *visitor) visitFilterExpression(ctx gen.IFilterExpressionContext) (varName string, src, pred ast.Expression, outErr *SemaError) {
	varName = symbolText(ctx.Symbol())
	var err error
	src, err = asExpr(v.visit(ctx.Expression()))
	if err != nil {
		outErr = &SemaError{Rule: "filterExpression", Pos: positionOf(ctx), Message: err.Error()}
		return
	}
	if wh := ctx.Where(); wh != nil {
		where, werr := v.visitWhere(wh)
		if werr != nil {
			outErr = &SemaError{Rule: "filterExpression", Pos: positionOf(ctx), Message: werr.Error()}
			return
		}
		if where != nil {
			pred = where.Predicate
		}
	}
	return
}

// VisitFilterWith handles ALL/ANY/NONE/SINGLE(filterExpr).
func (v *visitor) VisitFilterWith(ctx *gen.FilterWithContext) interface{} {
	fe := ctx.FilterExpression()
	if fe == nil {
		return unsupported(ctx, "filterWith", "missing filterExpression")
	}
	varName, src, pred, err := v.visitFilterExpression(fe)
	if err != nil {
		return err
	}

	var funcName string
	switch {
	case ctx.ALL() != nil:
		funcName = "all"
	case ctx.ANY() != nil:
		funcName = "any"
	case ctx.NONE() != nil:
		funcName = "none"
	case ctx.SINGLE() != nil:
		funcName = "single"
	default:
		return unsupported(ctx, "filterWith", "unknown quantifier")
	}

	// Reconstruct as: funcName(var IN src WHERE pred)
	// We represent this as a FunctionInvocation whose sole arg is a
	// ListComprehension with no projection (consistent with Cypher semantics).
	lc := &ast.ListComprehension{
		Pos:       positionOf(ctx),
		EndPos:    endPositionOf(ctx),
		Variable:  varName,
		Source:    src,
		Predicate: pred,
	}
	return &ast.FunctionInvocation{
		Pos:    positionOf(ctx),
		EndPos: endPositionOf(ctx),
		Name:   funcName,
		Args:   []ast.Expression{lc},
	}
}

// VisitPatternComprehension handles [[lhs =] relChain [WHERE pred] | expr].
func (v *visitor) VisitPatternComprehension(ctx *gen.PatternComprehensionContext) interface{} {
	rcp := ctx.RelationshipsChainPattern()
	if rcp == nil {
		return unsupported(ctx, "patternComprehension", "missing relationshipsChainPattern")
	}
	pathR := v.visit(rcp)
	if err := firstError(pathR); err != nil {
		return err
	}
	pp, ok := pathR.(*ast.PathPattern)
	if !ok {
		return unsupported(ctx, "patternComprehension", "expected PathPattern")
	}

	pc := &ast.PatternComprehension{
		Pos:     positionOf(ctx),
		EndPos:  endPositionOf(ctx),
		Pattern: pp,
	}

	// Optional path variable on the lhs.
	if lhs := ctx.Lhs(); lhs != nil {
		lhsR := v.visit(lhs)
		if s, ok := lhsR.(string); ok && s != "" {
			pc.Variable = &s
		}
	}

	if wh := ctx.Where(); wh != nil {
		where, err := v.visitWhere(wh)
		if err != nil {
			return err
		}
		pc.Predicate = where.Predicate
	}

	proj, err := asExpr(v.visit(ctx.Expression()))
	if err != nil {
		return &SemaError{Rule: "patternComprehension", Pos: positionOf(ctx), Message: err.Error()}
	}
	pc.Projection = proj
	return pc
}

// VisitRelationshipsChainPattern handles a bare relationships-chain atom:
// nodePattern (relPattern nodePattern)*.
func (v *visitor) VisitRelationshipsChainPattern(ctx *gen.RelationshipsChainPatternContext) interface{} {
	// RelationshipsChainPattern has the same structure as PatternElem.
	// Re-use the PatternElem logic: it expects AllPatternElemChain.
	// The context doesn't extend PatternElemContext directly, so we build
	// the linked list manually.
	nodeR := v.visit(ctx.NodePattern())
	if err := firstError(nodeR); err != nil {
		return err
	}
	nodePat, ok := nodeR.(*ast.NodePattern)
	if !ok {
		return unsupported(ctx, "relationshipsChainPattern", "expected NodePattern")
	}

	head := &ast.PathElement{Node: nodePat}
	cur := head
	for _, chain := range ctx.AllPatternElemChain() {
		cr := v.visit(chain)
		if err := firstError(cr); err != nil {
			return err
		}
		chainElem, ok := cr.(*ast.PathElement)
		if !ok {
			continue
		}
		cur.Next = chainElem
		cur = chainElem
	}

	pp := &ast.PathPattern{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Head: head}
	return pp
}

// -------------------------------------------------------------------------
// Parameter / literals
// -------------------------------------------------------------------------

// VisitParameter handles $name or $0.
func (v *visitor) VisitParameter(ctx *gen.ParameterContext) interface{} {
	var name string
	if sym := ctx.Symbol(); sym != nil {
		name = symbolText(sym)
	} else if nl := ctx.NumLit(); nl != nil {
		name = nl.GetText()
	}
	return &ast.Parameter{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Name: name}
}

// VisitLiteral dispatches to a specific literal type.
func (v *visitor) VisitLiteral(ctx *gen.LiteralContext) interface{} {
	if bl := ctx.BoolLit(); bl != nil {
		return v.visit(bl)
	}
	if nl := ctx.NumLit(); nl != nil {
		return v.visit(nl)
	}
	if ctx.NULL_W() != nil {
		return &ast.NullLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	}
	if sl := ctx.StringLit(); sl != nil {
		return v.visit(sl)
	}
	if cl := ctx.CharLit(); cl != nil {
		return v.visit(cl)
	}
	if ll := ctx.ListLit(); ll != nil {
		return v.visit(ll)
	}
	if ml := ctx.MapLit(); ml != nil {
		return v.visit(ml)
	}
	return unsupported(ctx, "literal", "unknown literal type")
}

// VisitBoolLit handles TRUE / FALSE.
func (v *visitor) VisitBoolLit(ctx *gen.BoolLitContext) interface{} {
	return &ast.BoolLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Value: ctx.TRUE() != nil}
}

// VisitNumLit handles integer and float literals.
//
// Positive integers are tokenised as ID rather than DIGIT due to lexer-rule
// ordering (ID : LetterOrDigit+ appears before DIGIT : SUB? ... in the grammar).
// The parser's numeric-ID fixes ensure that purely-numeric ID tokens reach this
// visitor via the numLit rule; ctx.GetText() is used instead of
// ctx.DIGIT().GetText() to support both token types transparently.
func (v *visitor) VisitNumLit(ctx *gen.NumLitContext) interface{} {
	text := ctx.GetText()
	if text == "" {
		// Fallback: try the DIGIT terminal if GetText is somehow empty.
		if d := ctx.DIGIT(); d != nil {
			text = d.GetText()
		}
	}
	if text == "" {
		return &SemaError{Rule: "numLit", Pos: positionOf(ctx), Message: "empty number literal"}
	}
	// Try float first (handles scientific notation / decimals in the DIGIT token).
	if strings.ContainsAny(text, ".eEfFdD") {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			// Distinguish overflow (FloatingPointOverflow per openCypher TCK
			// Literals5 [27]) from a malformed literal (generic SemaError).
			if errors.Is(err, strconv.ErrRange) {
				return &SemaError{Rule: "numLit", Pos: positionOf(ctx), Message: "floating-point literal out of range: " + text}
			}
			return &SemaError{Rule: "numLit", Pos: positionOf(ctx), Message: "invalid number: " + text}
		}
		return &ast.FloatLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Value: f}
	}
	// Hex / octal / decimal integer.
	n, intErr := parseInt(text)
	if intErr != nil {
		// Integer literals that overflow int64 are a compile-time IntegerOverflow
		// error per the openCypher TCK.  However, very long decimal literals
		// (more than 19 ASCII digits, optionally preceded by '-') are a special
		// case: the ANTLR DIGIT rule tokenises the integer part of a long float
		// literal such as "126354…09218.0" as a standalone token, leaving ".0"
		// as a separate DOT+ID pair.  The openCypher TCK expects these long-float
		// queries to succeed, producing a rounded IEEE-754 double.  We fall back
		// to float64 only for this case: when the digit run exceeds 19 characters
		// (longer than any valid int64).  Exactly-19-digit overflowing integers
		// (e.g. 9223372036854775808) are an error.
		digitLen := len(text)
		if text != "" && (text[0] == '-' || text[0] == '+') {
			digitLen = len(text) - 1
		}
		if digitLen > 19 {
			// Return a sentinel so visitPropertyExpression can reconstruct a
			// float literal when a fractional accessor follows (e.g. NNN.0).
			// In every other context the sentinel surfaces as IntegerOverflow.
			return &ast.OverflowIntLit{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Text: text}
		}
		return &SemaError{Rule: "numLit", Pos: positionOf(ctx), Message: "integer literal out of range: " + text}
	}
	return &ast.IntLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Value: n}
}

// VisitStringLit handles "string" or 'string' tokens.
func (v *visitor) VisitStringLit(ctx *gen.StringLitContext) interface{} {
	raw := ctx.STRING_LITERAL().GetText()
	s := unquoteString(raw)
	return &ast.StringLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Value: s}
}

// VisitCharLit handles char literals (treated as strings in Cypher).
func (v *visitor) VisitCharLit(ctx *gen.CharLitContext) interface{} {
	raw := ctx.CHAR_LITERAL().GetText()
	s := unquoteString(raw)
	return &ast.StringLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Value: s}
}

// VisitListLit handles [expr, expr, …].
func (v *visitor) VisitListLit(ctx *gen.ListLitContext) interface{} {
	ll := &ast.ListLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	if ec := ctx.ExpressionChain(); ec != nil {
		args, err := v.visitExpressionChain(ec)
		if err != nil {
			return err
		}
		ll.Elements = args
	}
	return ll
}

// VisitMapLit handles {key: expr, …}.
func (v *visitor) VisitMapLit(ctx *gen.MapLitContext) interface{} {
	ml := &ast.MapLiteral{Pos: positionOf(ctx), EndPos: endPositionOf(ctx)}
	for _, mp := range ctx.AllMapPair() {
		r := v.visit(mp)
		if err := firstError(r); err != nil {
			return err
		}
		if pair, ok := r.(*mapPair); ok {
			ml.Keys = append(ml.Keys, pair.key)
			ml.Values = append(ml.Values, pair.value)
		}
	}
	return ml
}

// mapPair is an internal transfer type.
type mapPair struct {
	key   string
	value ast.Expression
}

// VisitMapPair handles name: expr.
//
// Cypher requires a map key to be a valid SymbolicName, i.e. it must begin
// with a letter (a-z, A-Z) or underscore. The ANTLR grammar accepts any
// ID-shaped token, which includes digit-prefixed forms like `1B2c3e67`
// (because ID is defined as `LetterOrDigit+`). This visitor enforces the
// SymbolicName constraint and reports `InvalidSyntax` for digit-prefixed
// or purely-numeric keys, matching the openCypher 9 specification.
func (v *visitor) VisitMapPair(ctx *gen.MapPairContext) interface{} {
	key := nameText(ctx.Name())
	if key != "" {
		first := key[0]
		if first >= '0' && first <= '9' {
			return &SemaError{
				Rule:    "mapPair",
				Pos:     positionOf(ctx),
				Message: "map key must start with a letter or underscore, got: " + key,
			}
		}
	}
	val, err := asExpr(v.visit(ctx.Expression()))
	if err != nil {
		return &SemaError{Rule: "mapPair", Pos: positionOf(ctx), Message: err.Error()}
	}
	return &mapPair{key: key, value: val}
}

// -------------------------------------------------------------------------
// Name / Symbol helpers
// -------------------------------------------------------------------------

// VisitName returns the name string.
func (v *visitor) VisitName(ctx *gen.NameContext) interface{} {
	return nameText(ctx)
}

// VisitSymbol returns the symbol string.
func (v *visitor) VisitSymbol(ctx *gen.SymbolContext) interface{} {
	return symbolText(ctx)
}

// VisitReservedWord returns the reserved word string.
func (v *visitor) VisitReservedWord(ctx *gen.ReservedWordContext) interface{} {
	return ctx.GetText()
}

// -------------------------------------------------------------------------
// YIELD helpers
// -------------------------------------------------------------------------

// VisitYieldItems handles the YIELD clause items.
func (v *visitor) VisitYieldItems(ctx *gen.YieldItemsContext) interface{} {
	yield, err := v.visitYieldItems(ctx)
	if err != nil {
		return err
	}
	return yield
}

func (v *visitor) visitYieldItems(ctx gen.IYieldItemsContext) ([]*ast.YieldItem, *SemaError) {
	if ctx == nil {
		return nil, nil
	}
	// YIELD * — empty slice signals "yield all"
	if len(ctx.AllYieldItem()) == 0 {
		return []*ast.YieldItem{}, nil
	}
	var items []*ast.YieldItem
	for _, yi := range ctx.AllYieldItem() {
		r := v.visit(yi)
		if err := firstError(r); err != nil {
			return nil, err
		}
		if item, ok := r.(*ast.YieldItem); ok {
			items = append(items, item)
		}
	}
	return items, nil
}

// VisitYieldItem handles symbol [AS alias].
func (v *visitor) VisitYieldItem(ctx *gen.YieldItemContext) interface{} {
	syms := ctx.AllSymbol()
	if len(syms) == 0 {
		return unsupported(ctx, "yieldItem", "missing symbol")
	}
	item := &ast.YieldItem{Pos: positionOf(ctx), EndPos: endPositionOf(ctx), Name: symbolText(syms[0])}
	if len(syms) >= 2 {
		alias := symbolText(syms[1])
		item.Alias = &alias
	}
	return item
}

// VisitParenExpressionChain handles (expr, expr, …).
func (v *visitor) VisitParenExpressionChain(ctx *gen.ParenExpressionChainContext) interface{} {
	if ec := ctx.ExpressionChain(); ec != nil {
		exprs, err := v.visitExpressionChain(ec)
		if err != nil {
			return err
		}
		return exprs
	}
	return []ast.Expression(nil)
}

// -------------------------------------------------------------------------
// ExpressionChain helper
// -------------------------------------------------------------------------

func (v *visitor) visitExpressionChain(ctx gen.IExpressionChainContext) ([]ast.Expression, *SemaError) {
	if ctx == nil {
		return nil, nil
	}
	var exprs []ast.Expression
	for _, e := range ctx.AllExpression() {
		expr, err := asExpr(v.visit(e))
		if err != nil {
			return nil, &SemaError{Rule: "expressionChain", Message: err.Error()}
		}
		exprs = append(exprs, expr)
	}
	return exprs, nil
}

// VisitExpressionChain is the visitor entry.
func (v *visitor) VisitExpressionChain(ctx *gen.ExpressionChainContext) interface{} {
	exprs, err := v.visitExpressionChain(ctx)
	if err != nil {
		return err
	}
	return exprs
}

// -------------------------------------------------------------------------
// Pure helper functions (no visitor state needed)
// -------------------------------------------------------------------------

// symbolText returns the text of a symbol context, stripping backtick escaping.
func symbolText(ctx gen.ISymbolContext) string {
	if ctx == nil {
		return ""
	}
	t := ctx.GetText()
	if strings.HasPrefix(t, "`") && strings.HasSuffix(t, "`") {
		return t[1 : len(t)-1]
	}
	return t
}

// nameText returns the text of a name context (symbol or reserved word).
func nameText(ctx gen.INameContext) string {
	if ctx == nil {
		return ""
	}
	if sym := ctx.Symbol(); sym != nil {
		return symbolText(sym)
	}
	return ctx.GetText()
}

// nodeLabels extracts label strings from a NodeLabels context.
func nodeLabels(ctx gen.INodeLabelsContext) []string {
	if ctx == nil {
		return nil
	}
	var labels []string
	for _, n := range ctx.AllName() {
		labels = append(labels, nameText(n))
	}
	return labels
}

// relationshipTypes extracts the type list from a RelationshipTypes context.
func relationshipTypes(ctx gen.IRelationshipTypesContext) []string {
	if ctx == nil {
		return nil
	}
	var types []string
	for _, n := range ctx.AllName() {
		types = append(types, nameText(n))
	}
	return types
}

// comparisonOp returns the operator string for a ComparisonSigns node.
func comparisonOp(ctx gen.IComparisonSignsContext) string {
	if ctx.ASSIGN() != nil {
		return "="
	}
	if ctx.LE() != nil {
		return "<="
	}
	if ctx.GE() != nil {
		return ">="
	}
	if ctx.GT() != nil {
		return ">"
	}
	if ctx.LT() != nil {
		return "<"
	}
	if ctx.NOT_EQUAL() != nil {
		return "<>"
	}
	return "="
}

// stringPrefixOp maps StringExpPrefix alternatives to Cypher operators.
func stringPrefixOp(ctx gen.IStringExpPrefixContext) string {
	if ctx.STARTS() != nil {
		return "STARTS WITH"
	}
	if ctx.ENDS() != nil {
		return "ENDS WITH"
	}
	if ctx.CONTAINS() != nil {
		return "CONTAINS"
	}
	return "STARTS WITH"
}

// addSubOperators returns the ordered list of + / - tokens in an
// addSubExpression by walking all children and recording operator tokens.
func addSubOperators(ctx *gen.AddSubExpressionContext) []string {
	var ops []string
	for _, child := range ctx.GetChildren() {
		if t, ok := child.(antlr.TerminalNode); ok {
			switch t.GetSymbol().GetTokenType() {
			case gen.CypherParserPLUS:
				ops = append(ops, "+")
			case gen.CypherParserSUB:
				ops = append(ops, "-")
			}
		}
	}
	return ops
}

// multDivOperators returns the ordered operator list for a multDivExpression.
func multDivOperators(ctx *gen.MultDivExpressionContext) []string {
	var ops []string
	for _, child := range ctx.GetChildren() {
		if t, ok := child.(antlr.TerminalNode); ok {
			switch t.GetSymbol().GetTokenType() {
			case gen.CypherParserMULT:
				ops = append(ops, "*")
			case gen.CypherParserDIV:
				ops = append(ops, "/")
			case gen.CypherParserMOD:
				ops = append(ops, "%")
			}
		}
	}
	return ops
}

// parseInt parses decimal, hex (0x…/0X…), and octal (0o…/0O…) integer strings.
//
// For hexadecimal literals, it first tries signed parsing (base 16, 64-bit);
// if that overflows, it retries with unsigned 64-bit parsing and reinterprets
// the bits as a signed int64.  This handles the two's-complement minimum value
// 0x8000000000000000 (= INT64_MIN = -9223372036854775808) which cannot be
// represented as a signed positive hex literal but is valid when negated.
func parseInt(s string) (int64, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X"):
		return strconv.ParseInt(s[2:], 16, 64)
	case strings.HasPrefix(s, "0o") || strings.HasPrefix(s, "0O"):
		return strconv.ParseInt(s[2:], 8, 64)
	default:
		return strconv.ParseInt(s, 10, 64)
	}
}

// looksLikeExponentFloat reports whether s has the textual shape of a
// scientific-notation float literal: a leading digit run followed by an
// exponent marker (e/E), an optional sign, and a trailing digit run.
// The string is permitted to carry the optional `f`/`d` type suffix
// declared by the openCypher FLOAT lexer rule but no other characters.
// Examples: "1e9", "1E9", "2e-3", "10E+05".
func looksLikeExponentFloat(s string) bool {
	if s == "" {
		return false
	}
	i := skipDigitRun(s, 0)
	if i == 0 {
		return false
	}
	if i >= len(s) || (s[i] != 'e' && s[i] != 'E') {
		return false
	}
	i++
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	start := i
	i = skipDigitRun(s, i)
	if i == start {
		return false
	}
	if i < len(s) {
		switch s[i] {
		case 'f', 'F', 'd', 'D':
			i++
		}
	}
	return i == len(s)
}

// skipDigitRun advances past consecutive ASCII decimal digits starting
// at position i and returns the new index.
func skipDigitRun(s string, i int) int {
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

// hasInvalidNumericChar reports whether s contains any character that is
// not a decimal digit and not a recognised float-literal suffix character
// (e, E, f, F, d, D — exponent marker or type suffix). It is the inner
// check for the digit-prefixed-but-malformed integer rejection in
// VisitAtom: a token starting with [1-9] that contains any other letter
// (such as `h` in "9223372h54775808") is neither a valid identifier nor a
// valid numeric literal.
func hasInvalidNumericChar(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case 'e', 'E', 'f', 'F', 'd', 'D', '+', '-':
			continue
		}
		return true
	}
	return false
}

// unquoteString strips surrounding quotes from a string/char literal token
// and processes the openCypher 9 §6.3.3 escape sequences:
//
//	\\ \' \" \b \f \n \r \t \uXXXX
//
// A backslash followed by any other character is preserved verbatim
// (matches the ANTLR grammar that accepts e.g. `\\u41` as the two
// literal characters `\` and `u41` once the leading `\\` is decoded).
// The grammar already rejects malformed \u escapes at parse time
// (`\u`, `\uH`, `\u00ZZ`, …) so the 4-hex-digit case is safe here.
//
// Implementation note: chained strings.ReplaceAll cannot be used —
// it would mis-decode patterns like `\\'` (literal backslash followed
// by an end-of-string quote) because the `\\'` substring would match
// the `\'` rule before the `\\` rule. A single left-to-right walk
// over the runes is the only correct decoding.
func unquoteString(raw string) string {
	if len(raw) < 2 {
		return raw
	}
	q := raw[0]
	if q != '\'' && q != '"' {
		return raw
	}
	inner := raw[1 : len(raw)-1]
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c != '\\' || i+1 >= len(inner) {
			b.WriteByte(c)
			continue
		}
		next := inner[i+1]
		switch next {
		case '\\':
			b.WriteByte('\\')
			i++
		case '\'':
			b.WriteByte('\'')
			i++
		case '"':
			b.WriteByte('"')
			i++
		case 'n':
			b.WriteByte('\n')
			i++
		case 't':
			b.WriteByte('\t')
			i++
		case 'r':
			b.WriteByte('\r')
			i++
		case 'b':
			b.WriteByte('\b')
			i++
		case 'f':
			b.WriteByte('\f')
			i++
		case 'u':
			// Walk past any additional `u` characters (the grammar
			// accepts `\uuu...XXXX` where the trailing 4 hex digits
			// define the codepoint). Decode the 4 hex digits if they
			// are present; otherwise emit the raw backslash and let
			// the unconsumed text follow.
			j := i + 2
			for j < len(inner) && inner[j] == 'u' {
				j++
			}
			if j+4 <= len(inner) && isHex(inner[j]) && isHex(inner[j+1]) && isHex(inner[j+2]) && isHex(inner[j+3]) {
				cp, _ := strconv.ParseUint(inner[j:j+4], 16, 32)
				b.WriteRune(rune(cp))
				i = j + 3
			} else {
				b.WriteByte('\\')
			}
		default:
			b.WriteByte('\\')
		}
	}
	return b.String()
}

// isHex reports whether b is an ASCII hexadecimal digit.
func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// containsBareRelChainPattern reports whether expr contains an
// [*ast.PathPattern] node that is used as a value sub-expression. The
// openCypher specification permits relationship-chain patterns inside
// MATCH/CREATE/MERGE clauses, [*ast.PatternComprehension] brackets, and
// [*ast.ExistsSubquery] / [*ast.CountSubquery] braces, but rejects them as
// projection items (RETURN/WITH), as the right-hand side of SET, and as
// function arguments (including `size((…)-[…]->…)`). The TCK reports such
// usage as a `UnexpectedSyntax` compile-time error.
//
// The walker recurses through every expression-bearing field except those
// that introduce a new pattern-bearing context: PatternComprehension,
// ExistsSubquery, and CountSubquery are treated as opaque (they may
// legitimately contain a PathPattern as their root).
//
// Returns true on the first PathPattern found; the caller is responsible
// for converting that into a SemaError with the appropriate position.
//
//nolint:gocyclo // dispatcher over every Expression concrete type; complexity is essentially the cardinality of the AST
func containsBareRelChainPattern(expr ast.Expression) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *ast.PathPattern:
		return true
	case *ast.Variable, *ast.Parameter,
		*ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral,
		*ast.BoolLiteral, *ast.NullLiteral:
		return false
	case *ast.Property:
		return containsBareRelChainPattern(e.Receiver)
	case *ast.FunctionInvocation:
		for _, a := range e.Args {
			if containsBareRelChainPattern(a) {
				return true
			}
		}
		return false
	case *ast.BinaryOp:
		return containsBareRelChainPattern(e.Left) || containsBareRelChainPattern(e.Right)
	case *ast.UnaryOp:
		return containsBareRelChainPattern(e.Operand)
	case *ast.CaseExpression:
		if containsBareRelChainPattern(e.Subject) {
			return true
		}
		for _, alt := range e.Alternatives {
			if containsBareRelChainPattern(alt.Condition) ||
				containsBareRelChainPattern(alt.Consequent) {
				return true
			}
		}
		return containsBareRelChainPattern(e.ElseExpr)
	case *ast.ListLiteral:
		for _, el := range e.Elements {
			if containsBareRelChainPattern(el) {
				return true
			}
		}
		return false
	case *ast.MapLiteral:
		for _, v := range e.Values {
			if containsBareRelChainPattern(v) {
				return true
			}
		}
		return false
	case *ast.ListComprehension:
		return containsBareRelChainPattern(e.Source) ||
			containsBareRelChainPattern(e.Predicate) ||
			containsBareRelChainPattern(e.Projection)
	case *ast.PatternComprehension:
		// PatternComprehension legitimately contains a PathPattern; do not
		// recurse into Pattern. The predicate and projection are scoped to
		// the bindings introduced by the pattern.
		return containsBareRelChainPattern(e.Predicate) ||
			containsBareRelChainPattern(e.Projection)
	case *ast.MapProjection:
		if containsBareRelChainPattern(e.Subject) {
			return true
		}
		for _, item := range e.Items {
			if containsBareRelChainPattern(item.Value) {
				return true
			}
		}
		return false
	case *ast.ExistsSubquery, *ast.CountSubquery:
		// Subqueries are opaque pattern-bearing contexts.
		return false
	case *ast.SubscriptExpr:
		return containsBareRelChainPattern(e.Expr) ||
			containsBareRelChainPattern(e.Index)
	case *ast.SliceExpr:
		return containsBareRelChainPattern(e.Expr) ||
			containsBareRelChainPattern(e.From) ||
			containsBareRelChainPattern(e.To)
	}
	// Unknown expression type: conservatively report false so the validator
	// does not over-reject. New expression types should be added to the
	// switch above.
	return false
}
