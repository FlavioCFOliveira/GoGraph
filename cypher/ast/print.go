package ast

import (
	"strings"
)

// Print returns the canonical Cypher representation of q.
//
// The output is deterministic and differs from q.String() in one critical
// respect: string literals are emitted with double-quotes ("value") rather
// than single-quotes ('value').  This is required for the round-trip property
// (see package-level documentation): the grammar's CHAR_LITERAL rule matches
// only a single character in single-quotes, so multi-character values enclosed
// in single-quotes are lexed as identifiers rather than string literals.
// Double-quoted STRING_LITERAL has no such restriction.
//
// All other formatting rules are inherited from the existing String() methods:
//   - Keywords in UPPER CASE
//   - Single-space token separators
//   - Explicit parentheses around every binary and unary operator
//
// Round-trip guarantee: for any Cypher query Q that [github.com/FlavioCFOliveira/GoGraph/cypher/parser.Parse]
// accepts, Parse(Print(Parse(Q))) produces an AST that is structurally equal to
// Parse(Q) when Position fields are ignored.
func Print(q Query) string {
	var p printer
	return p.query(q)
}

// printer holds no mutable state; all methods are value receivers.
// It is a zero-allocation, call-stack-only traversal.
type printer struct{}

// ─────────────────────────────────────────────────────────────────────────────
// Query-level
// ─────────────────────────────────────────────────────────────────────────────

func (p printer) query(q Query) string {
	switch v := q.(type) {
	case *SingleQuery:
		return p.singleQuery(v)
	case *MultiQuery:
		return p.multiQuery(v)
	default:
		// Fallback: use the node's own String() method.
		return q.String()
	}
}

func (p printer) singleQuery(q *SingleQuery) string {
	parts := make([]string, 0, len(q.ReadingClauses)+len(q.With)+len(q.UpdatingClauses)+1)
	for _, rc := range q.ReadingClauses {
		parts = append(parts, p.readingClause(rc))
	}
	// When LeadingCountSet is true (parser-generated MultiPartQ queries), WITH
	// clauses are already embedded in ReadingClauses in document order — skip
	// q.With to avoid printing them twice.
	if !q.LeadingCountSet {
		for _, w := range q.With {
			parts = append(parts, p.withClause(w))
		}
	}
	for _, uc := range q.UpdatingClauses {
		parts = append(parts, p.updatingClause(uc))
	}
	if q.Return != nil {
		parts = append(parts, p.returnClause(q.Return))
	}
	return strings.Join(parts, " ")
}

func (p printer) multiQuery(q *MultiQuery) string {
	if len(q.Parts) == 0 {
		return ""
	}
	keyword := " UNION "
	if q.All {
		keyword = " UNION ALL "
	}
	parts := make([]string, len(q.Parts))
	for i, part := range q.Parts {
		parts[i] = p.singleQuery(part)
	}
	return strings.Join(parts, keyword)
}

// ─────────────────────────────────────────────────────────────────────────────
// Clauses
// ─────────────────────────────────────────────────────────────────────────────

func (p printer) readingClause(rc ReadingClause) string {
	switch v := rc.(type) {
	case *Match:
		return p.matchClause(v)
	case *OptionalMatch:
		return p.optionalMatchClause(v)
	case *Unwind:
		return p.unwindClause(v)
	case *Return:
		return p.returnClause(v)
	case *With:
		return p.withClause(v)
	case *Call:
		return p.callClause(v)
	case *Where:
		return p.whereClause(v)
	default:
		return rc.String()
	}
}

func (p printer) updatingClause(uc UpdatingClause) string {
	switch v := uc.(type) {
	case *Create:
		return p.createClause(v)
	case *Merge:
		return p.mergeClause(v)
	case *Set:
		return p.setClause(v)
	case *Remove:
		return p.removeClause(v)
	case *Delete:
		return p.deleteClause(v)
	case *DetachDelete:
		return p.detachDeleteClause(v)
	case *Call:
		return p.callClause(v)
	default:
		return uc.String()
	}
}

func (p printer) matchClause(m *Match) string {
	out := "MATCH " + p.pattern(m.Pattern)
	if m.Where != nil {
		out += " " + p.whereClause(m.Where)
	}
	return out
}

func (p printer) optionalMatchClause(o *OptionalMatch) string {
	out := "OPTIONAL MATCH " + p.pattern(o.Pattern)
	if o.Where != nil {
		out += " " + p.whereClause(o.Where)
	}
	return out
}

func (p printer) whereClause(w *Where) string {
	return "WHERE " + p.expr(w.Predicate)
}

func (p printer) unwindClause(u *Unwind) string {
	return "UNWIND " + p.expr(u.Expr) + " AS " + u.Variable
}

func (p printer) returnClause(r *Return) string {
	return "RETURN " + p.projection(r.Projection)
}

func (p printer) withClause(w *With) string {
	out := "WITH " + p.projection(w.Projection)
	if w.Where != nil {
		out += " " + p.whereClause(w.Where)
	}
	return out
}

func (p printer) createClause(c *Create) string {
	return "CREATE " + p.pattern(c.Pattern)
}

func (p printer) mergeClause(m *Merge) string {
	out := "MERGE " + p.pathPattern(m.Pattern)
	if len(m.OnCreate) > 0 {
		parts := make([]string, len(m.OnCreate))
		for i, s := range m.OnCreate {
			parts[i] = p.setItem(s)
		}
		out += " ON CREATE SET " + strings.Join(parts, ", ")
	}
	if len(m.OnMatch) > 0 {
		parts := make([]string, len(m.OnMatch))
		for i, s := range m.OnMatch {
			parts[i] = p.setItem(s)
		}
		out += " ON MATCH SET " + strings.Join(parts, ", ")
	}
	return out
}

func (p printer) setClause(s *Set) string {
	parts := make([]string, len(s.Items))
	for i, item := range s.Items {
		parts[i] = p.setItem(item)
	}
	return "SET " + strings.Join(parts, ", ")
}

func (p printer) setItem(s *SetItem) string {
	if len(s.Labels) > 0 {
		out := p.expr(s.Target)
		for _, l := range s.Labels {
			out += ":" + l
		}
		return out
	}
	return p.expr(s.Target) + " " + s.Operator + " " + p.expr(s.Value)
}

func (p printer) removeClause(r *Remove) string {
	parts := make([]string, len(r.Items))
	for i, item := range r.Items {
		parts[i] = p.removeItem(item)
	}
	return "REMOVE " + strings.Join(parts, ", ")
}

func (p printer) removeItem(r *RemoveItem) string {
	if len(r.Labels) > 0 {
		out := p.expr(r.Target)
		for _, l := range r.Labels {
			out += ":" + l
		}
		return out
	}
	return p.expr(r.Target)
}

func (p printer) deleteClause(d *Delete) string {
	parts := make([]string, len(d.Expressions))
	for i, e := range d.Expressions {
		parts[i] = p.expr(e)
	}
	return "DELETE " + strings.Join(parts, ", ")
}

func (p printer) detachDeleteClause(d *DetachDelete) string {
	parts := make([]string, len(d.Expressions))
	for i, e := range d.Expressions {
		parts[i] = p.expr(e)
	}
	return "DETACH DELETE " + strings.Join(parts, ", ")
}

func (p printer) callClause(c *Call) string {
	parts := make([]string, 0, len(c.Namespace)+1)
	parts = append(parts, c.Namespace...)
	parts = append(parts, c.Procedure)
	out := "CALL " + strings.Join(parts, ".")

	if c.Args != nil {
		argParts := make([]string, len(c.Args))
		for i, a := range c.Args {
			argParts[i] = p.expr(a)
		}
		out += "(" + strings.Join(argParts, ", ") + ")"
	}

	if c.Yield != nil {
		if len(c.Yield) == 0 {
			out += " YIELD *"
		} else {
			yieldParts := make([]string, len(c.Yield))
			for i, y := range c.Yield {
				yieldParts[i] = y.String() // YieldItem has no string literals
			}
			out += " YIELD " + strings.Join(yieldParts, ", ")
		}
	}

	if c.Where != nil {
		out += " " + p.whereClause(c.Where)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Projection
// ─────────────────────────────────────────────────────────────────────────────

func (p printer) projection(proj *Projection) string {
	if proj.All {
		return "*"
	}
	out := ""
	if proj.Distinct {
		out += "DISTINCT "
	}
	for i, item := range proj.Items {
		if i > 0 {
			out += ", "
		}
		out += p.projectionItem(item)
	}
	if len(proj.OrderBy) > 0 {
		out += " ORDER BY "
		for i, s := range proj.OrderBy {
			if i > 0 {
				out += ", "
			}
			out += p.sortItem(s)
		}
	}
	if proj.Skip != nil {
		out += " SKIP " + p.expr(proj.Skip)
	}
	if proj.Limit != nil {
		out += " LIMIT " + p.expr(proj.Limit)
	}
	return out
}

func (p printer) projectionItem(item *ProjectionItem) string {
	if item.Alias != nil {
		return p.expr(item.Expr) + " AS " + *item.Alias
	}
	return p.expr(item.Expr)
}

func (p printer) sortItem(s *SortItem) string {
	if s.Descending {
		return p.expr(s.Expr) + " DESC"
	}
	return p.expr(s.Expr) + " ASC"
}

// ─────────────────────────────────────────────────────────────────────────────
// Patterns
// ─────────────────────────────────────────────────────────────────────────────

func (p printer) pattern(pat *Pattern) string {
	parts := make([]string, len(pat.Paths))
	for i, path := range pat.Paths {
		parts[i] = p.pathPattern(path)
	}
	return strings.Join(parts, ", ")
}

func (p printer) pathPattern(pp *PathPattern) string {
	out := ""
	if pp.Variable != nil {
		out += *pp.Variable + " = "
	}
	el := pp.Head
	for el != nil {
		if el.Relationship != nil {
			// RelationshipPattern.String() contains no string literals; safe to delegate.
			out += el.Relationship.String()
		}
		if el.Node != nil {
			out += p.nodePattern(el.Node)
		}
		el = el.Next
	}
	return out
}

func (p printer) nodePattern(n *NodePattern) string {
	out := "("
	if n.Variable != nil {
		out += *n.Variable
	}
	for _, l := range n.Labels {
		out += ":" + l
	}
	if n.Properties != nil {
		out += " " + p.expr(n.Properties)
	}
	out += ")"
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Expressions
// ─────────────────────────────────────────────────────────────────────────────

// expr dispatches to a type-specific printer.  The only case that differs from
// the node's own String() method is StringLiteral, which is emitted as a
// double-quoted STRING_LITERAL so that it re-parses correctly.
//
//nolint:gocyclo // One branch per concrete Expression type; complexity is structural, not logical.
func (p printer) expr(e Expression) string {
	if e == nil {
		return ""
	}
	switch v := e.(type) {
	case *StringLiteral:
		return p.stringLiteral(v)
	case *IntLiteral:
		return v.String()
	case *FloatLiteral:
		return v.String()
	case *BoolLiteral:
		return v.String()
	case *NullLiteral:
		return v.String()
	case *Variable:
		return v.String()
	case *Parameter:
		return v.String()
	case *Property:
		return p.property(v)
	case *FunctionInvocation:
		return p.functionInvocation(v)
	case *BinaryOp:
		return p.binaryOp(v)
	case *UnaryOp:
		return p.unaryOp(v)
	case *CaseExpression:
		return p.caseExpression(v)
	case *ListComprehension:
		return p.listComprehension(v)
	case *PatternComprehension:
		return p.patternComprehension(v)
	case *ReduceExpr:
		return v.String()
	case *MapProjection:
		return p.mapProjection(v)
	case *ExistsSubquery:
		return p.existsSubquery(v)
	case *CountSubquery:
		return p.countSubquery(v)
	case *SubscriptExpr:
		return p.subscriptExpr(v)
	case *SliceExpr:
		return p.sliceExpr(v)
	case *ListLiteral:
		return p.listLiteral(v)
	case *MapLiteral:
		return p.mapLiteral(v)
	case *PathPattern:
		// PathPattern implements Expression (for pattern comprehensions, etc.).
		return p.pathPattern(v)
	default:
		// Safe fallback: if a new Expression type is added, use its String().
		return e.String()
	}
}

// stringLiteral emits the value enclosed in double-quotes with internal
// double-quotes and backslashes escaped.  This produces a STRING_LITERAL token
// (not a CHAR_LITERAL), which the grammar accepts for any length.
func (p printer) stringLiteral(s *StringLiteral) string {
	escaped := strings.ReplaceAll(s.Value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

func (p printer) property(prop *Property) string {
	return p.expr(prop.Receiver) + "." + prop.Key
}

func (p printer) functionInvocation(f *FunctionInvocation) string {
	parts := make([]string, 0, len(f.Namespace)+1)
	parts = append(parts, f.Namespace...)
	parts = append(parts, f.Name)
	funcName := strings.Join(parts, ".")

	if f.CountStar {
		return funcName + "(*)"
	}

	argParts := make([]string, len(f.Args))
	for i, a := range f.Args {
		argParts[i] = p.expr(a)
	}
	argStr := strings.Join(argParts, ", ")

	if f.Distinct {
		return funcName + "(DISTINCT " + argStr + ")"
	}
	return funcName + "(" + argStr + ")"
}

func (p printer) binaryOp(b *BinaryOp) string {
	return "(" + p.expr(b.Left) + " " + b.Operator + " " + p.expr(b.Right) + ")"
}

func (p printer) unaryOp(u *UnaryOp) string {
	switch u.Operator {
	case "IS NULL", "IS NOT NULL":
		return "(" + p.expr(u.Operand) + " " + u.Operator + ")"
	}
	return "(" + u.Operator + " " + p.expr(u.Operand) + ")"
}

func (p printer) caseExpression(c *CaseExpression) string {
	out := "CASE"
	if c.Subject != nil {
		out += " " + p.expr(c.Subject)
	}
	for _, alt := range c.Alternatives {
		out += " WHEN " + p.expr(alt.Condition) + " THEN " + p.expr(alt.Consequent)
	}
	if c.ElseExpr != nil {
		out += " ELSE " + p.expr(c.ElseExpr)
	}
	out += " END"
	return out
}

func (p printer) listComprehension(lc *ListComprehension) string {
	out := "[" + lc.Variable + " IN " + p.expr(lc.Source)
	if lc.Predicate != nil {
		out += " WHERE " + p.expr(lc.Predicate)
	}
	if lc.Projection != nil {
		out += " | " + p.expr(lc.Projection)
	}
	out += "]"
	return out
}

func (p printer) patternComprehension(pc *PatternComprehension) string {
	out := "["
	if pc.Variable != nil {
		out += *pc.Variable + " = "
	}
	out += p.pathPattern(pc.Pattern)
	if pc.Predicate != nil {
		out += " WHERE " + p.expr(pc.Predicate)
	}
	out += " | " + p.expr(pc.Projection) + "]"
	return out
}

func (p printer) mapProjection(mp *MapProjection) string {
	parts := make([]string, len(mp.Items))
	for i, item := range mp.Items {
		if item.IsAll {
			parts[i] = ".*"
		} else if item.Value == nil {
			parts[i] = "." + item.Key
		} else if item.Key != "" {
			parts[i] = item.Key + ": " + p.expr(item.Value)
		} else {
			parts[i] = p.expr(item.Value)
		}
	}
	return p.expr(mp.Subject) + " {" + strings.Join(parts, ", ") + "}"
}

func (p printer) existsSubquery(e *ExistsSubquery) string {
	if e.Pattern != nil {
		return "EXISTS { " + p.pattern(e.Pattern) + " }"
	}
	return "EXISTS { " + p.singleQuery(e.Query) + " }"
}

func (p printer) countSubquery(c *CountSubquery) string {
	if c.Pattern != nil {
		return "COUNT { " + p.pattern(c.Pattern) + " }"
	}
	return "COUNT { " + p.singleQuery(c.Query) + " }"
}

func (p printer) subscriptExpr(s *SubscriptExpr) string {
	return p.expr(s.Expr) + "[" + p.expr(s.Index) + "]"
}

func (p printer) sliceExpr(s *SliceExpr) string {
	out := p.expr(s.Expr) + "["
	if s.From != nil {
		out += p.expr(s.From)
	}
	out += ".."
	if s.To != nil {
		out += p.expr(s.To)
	}
	out += "]"
	return out
}

func (p printer) listLiteral(ll *ListLiteral) string {
	parts := make([]string, len(ll.Elements))
	for i, e := range ll.Elements {
		parts[i] = p.expr(e)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (p printer) mapLiteral(ml *MapLiteral) string {
	if len(ml.Keys) == 0 {
		return "{}"
	}
	parts := make([]string, len(ml.Keys))
	for i, k := range ml.Keys {
		parts[i] = k + ": " + p.expr(ml.Values[i])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
