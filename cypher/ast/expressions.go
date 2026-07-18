package ast

import (
	"strings"
)

// ----------------------------------------------------------------------------
// Expression nodes
// All types in this file implement Expression (and therefore Node).
// ----------------------------------------------------------------------------

// Variable is a named reference: n, r, x.
type Variable struct {
	Name   string
	Pos    Position
	EndPos Position
}

func (*Variable) astNode()  {}
func (*Variable) exprNode() {}

// String returns the variable name.
func (v *Variable) String() string { return v.Name }

// Parameter is a query parameter: $name or $0.
type Parameter struct {
	Name   string // the name/index without the leading '$'
	Pos    Position
	EndPos Position
}

func (*Parameter) astNode()  {}
func (*Parameter) exprNode() {}

// String returns the Cypher parameter reference.
func (p *Parameter) String() string { return "$" + p.Name }

// Property is a property access: expr.key.
type Property struct {
	Receiver Expression
	Key      string
	Pos      Position
	EndPos   Position
}

func (*Property) astNode()  {}
func (*Property) exprNode() {}

// String returns the Cypher property access.
func (p *Property) String() string { return p.Receiver.String() + "." + p.Key }

// FunctionInvocation is a function call: func(args…) or func(DISTINCT args…).
type FunctionInvocation struct {
	Name      string
	Namespace []string // e.g. ["apoc", "path"] for apoc.path.expand
	Args      []Expression
	Pos       Position
	EndPos    Position
	Distinct  bool
	// CountStar is true when this is COUNT(*). String() renders it as
	// "count(*)" and downstream aggregation detects it without needing
	// a wildcard argument expression.
	CountStar bool
}

func (*FunctionInvocation) astNode()  {}
func (*FunctionInvocation) exprNode() {}

// String returns the Cypher function call.
func (f *FunctionInvocation) String() string {
	parts := make([]string, 0, len(f.Namespace)+1)
	parts = append(parts, f.Namespace...)
	parts = append(parts, f.Name)
	funcName := strings.Join(parts, ".")

	if f.CountStar {
		return funcName + "(*)"
	}

	argParts := make([]string, len(f.Args))
	for i, a := range f.Args {
		argParts[i] = a.String()
	}
	argStr := strings.Join(argParts, ", ")

	if f.Distinct {
		return funcName + "(DISTINCT " + argStr + ")"
	}
	return funcName + "(" + argStr + ")"
}

// BinaryOp is a binary operator expression: left OP right.
type BinaryOp struct {
	Left     Expression
	Right    Expression
	Operator string // e.g. "+", "-", "=", "<>", "AND", "OR", "IN", "CONTAINS"
	Pos      Position
	EndPos   Position
	// Parenthesized records that this BinaryOp was explicitly parenthesized in
	// the source. The precedence-rebalancing pass in cypher/parser uses this
	// flag to suppress lifting list/string-predicate operators (IN, CONTAINS,
	// STARTS WITH, ENDS WITH) out of arithmetic chains when the user wrote the
	// parentheses explicitly: `[1] + (2 IN [3]) + 4` must remain `[1] +
	// bool + 4`, but `[1] + 2 IN [3] + 4` must rebalance to `([1] + 2) IN
	// ([3] + 4)`. The flag is cleared once the rebalance pass completes;
	// downstream consumers ignore it.
	Parenthesized bool
}

func (*BinaryOp) astNode()  {}
func (*BinaryOp) exprNode() {}

// String returns the Cypher infix expression.
func (b *BinaryOp) String() string {
	return "(" + b.Left.String() + " " + b.Operator + " " + b.Right.String() + ")"
}

// UnaryOp is a unary operator expression: OP expr.
type UnaryOp struct {
	Operand  Expression
	Operator string // e.g. "-", "NOT", "IS NULL", "IS NOT NULL"
	Pos      Position
	EndPos   Position
}

func (*UnaryOp) astNode()  {}
func (*UnaryOp) exprNode() {}

// String returns the Cypher prefix expression.
func (u *UnaryOp) String() string {
	// IS NULL / IS NOT NULL are postfix in Cypher.
	switch u.Operator {
	case "IS NULL", "IS NOT NULL":
		return "(" + u.Operand.String() + " " + u.Operator + ")"
	}
	return "(" + u.Operator + " " + u.Operand.String() + ")"
}

// LabelPredicate is the conjunctive-label test on a node-valued
// expression: `n:Foo:Bar` evaluates to true when n is a node carrying
// every named label. The form appears both in WHERE filters and as a
// stand-alone projection (`RETURN (n:Foo)`). Receiver may be any
// expression; at evaluation time non-Node values yield NULL.
type LabelPredicate struct {
	Receiver Expression
	Labels   []string
	Pos      Position
	EndPos   Position
}

func (*LabelPredicate) astNode()  {}
func (*LabelPredicate) exprNode() {}

// String returns the Cypher predicate `(receiver:Label1:Label2)`. The
// parentheses match the canonical openCypher column-header form
// projected by `RETURN (n:Foo)`, so RETURN columns line up with the
// TCK comparison table.
func (l *LabelPredicate) String() string {
	var b strings.Builder
	b.WriteByte('(')
	b.WriteString(l.Receiver.String())
	for _, lbl := range l.Labels {
		b.WriteByte(':')
		b.WriteString(lbl)
	}
	b.WriteByte(')')
	return b.String()
}

// CaseAlternative is a single WHEN … THEN … arm in a CASE expression.
type CaseAlternative struct {
	Condition  Expression
	Consequent Expression
	Pos        Position
	EndPos     Position
}

// String returns the WHEN…THEN arm.
func (c *CaseAlternative) String() string {
	return "WHEN " + c.Condition.String() + " THEN " + c.Consequent.String()
}

// CaseExpression is a CASE expression, either generic or value-form.
//
//	CASE [subject] WHEN … THEN … [ELSE …] END
type CaseExpression struct {
	Subject      Expression // nil for generic CASE
	ElseExpr     Expression // nil when no ELSE clause
	Alternatives []*CaseAlternative
	Pos          Position
	EndPos       Position
}

func (*CaseExpression) astNode()  {}
func (*CaseExpression) exprNode() {}

// String returns the Cypher CASE expression.
func (c *CaseExpression) String() string {
	out := "CASE"
	if c.Subject != nil {
		out += " " + c.Subject.String()
	}
	for _, alt := range c.Alternatives {
		out += " " + alt.String()
	}
	if c.ElseExpr != nil {
		out += " ELSE " + c.ElseExpr.String()
	}
	out += " END"
	return out
}

// ListComprehension is a list comprehension: [var IN list WHERE pred | expr].
type ListComprehension struct {
	Source     Expression
	Predicate  Expression // nil when no WHERE clause
	Projection Expression // nil when no projection expression
	Variable   string
	Pos        Position
	EndPos     Position
}

func (*ListComprehension) astNode()  {}
func (*ListComprehension) exprNode() {}

// String returns the Cypher list comprehension.
func (l *ListComprehension) String() string {
	out := "[" + l.Variable + " IN " + l.Source.String()
	if l.Predicate != nil {
		out += " WHERE " + l.Predicate.String()
	}
	if l.Projection != nil {
		out += " | " + l.Projection.String()
	}
	out += "]"
	return out
}

// PatternComprehension is a pattern comprehension:
// [(a)-[r]->(b) WHERE pred | expr].
type PatternComprehension struct {
	Predicate  Expression // nil when no WHERE clause
	Projection Expression
	Variable   *string // optional path variable
	Pattern    *PathPattern
	Pos        Position
	EndPos     Position
}

func (*PatternComprehension) astNode()  {}
func (*PatternComprehension) exprNode() {}

// String returns the Cypher pattern comprehension.
func (p *PatternComprehension) String() string {
	out := "["
	if p.Variable != nil {
		out += *p.Variable + " = "
	}
	out += p.Pattern.String()
	if p.Predicate != nil {
		out += " WHERE " + p.Predicate.String()
	}
	out += " | " + p.Projection.String() + "]"
	return out
}

// MapProjectionItem represents one item in a map projection.
type MapProjectionItem struct {
	Value  Expression // nil for the property-selector shorthand (`.key`)
	Key    string     // explicit key when present; otherwise empty
	Pos    Position
	EndPos Position
	IsAll  bool // true for the .*  selector
}

// String returns the item representation.
func (m *MapProjectionItem) String() string {
	if m.IsAll {
		return ".*"
	}
	if m.Value == nil {
		// property selector: `.key`
		return "." + m.Key
	}
	if m.Key != "" {
		return m.Key + ": " + m.Value.String()
	}
	return m.Value.String()
}

// MapProjection is a map projection expression: n {.name, .age, extra: $x}.
//
// The map-projection production lives in the ANTLR grammar
// (cypher/parser/grammar/CypherParser.g4, the mapProjection / mapProjectionItem
// rules); the parser visitor
// ([github.com/FlavioCFOliveira/GoGraph/cypher/parser] VisitMapProjection)
// constructs this node, which is then evaluated by
// ([github.com/FlavioCFOliveira/GoGraph/cypher/expr] evalMapProjection) and
// type-checked by the semantic analyser. Map projection is an accepted
// openCypher extension (CIP2014-12-12); it is NOT part of the openCypher TCK,
// so it does not affect TCK conformance.
type MapProjection struct {
	Subject Expression
	Items   []*MapProjectionItem
	Pos     Position
	EndPos  Position
}

func (*MapProjection) astNode()  {}
func (*MapProjection) exprNode() {}

// String returns the Cypher map projection.
func (m *MapProjection) String() string {
	parts := make([]string, len(m.Items))
	for i, item := range m.Items {
		parts[i] = item.String()
	}
	return m.Subject.String() + " {" + strings.Join(parts, ", ") + "}"
}

// ExistsSubquery is an EXISTS { … } subquery expression.
type ExistsSubquery struct {
	Pattern *Pattern     // pattern form: EXISTS { (a)-[r]->(b) }
	Where   *Where       // optional inline WHERE clause for the pattern form
	Query   *SingleQuery // full subquery form: EXISTS { MATCH … RETURN … }
	Pos     Position
	EndPos  Position
}

func (*ExistsSubquery) astNode()  {}
func (*ExistsSubquery) exprNode() {}

// String returns the Cypher EXISTS subquery.
func (e *ExistsSubquery) String() string {
	if e.Pattern != nil {
		out := "EXISTS { " + e.Pattern.String()
		if e.Where != nil {
			out += " " + e.Where.String()
		}
		return out + " }"
	}
	return "EXISTS { " + e.Query.String() + " }"
}

// CountSubquery is a COUNT { … } subquery expression.
type CountSubquery struct {
	Pattern *Pattern     // pattern form
	Query   *SingleQuery // full subquery form
	Pos     Position
	EndPos  Position
}

func (*CountSubquery) astNode()  {}
func (*CountSubquery) exprNode() {}

// String returns the Cypher COUNT subquery.
func (c *CountSubquery) String() string {
	if c.Pattern != nil {
		return "COUNT { " + c.Pattern.String() + " }"
	}
	return "COUNT { " + c.Query.String() + " }"
}

// SubscriptExpr is a subscript access: expr[index].
type SubscriptExpr struct {
	Expr   Expression
	Index  Expression
	Pos    Position
	EndPos Position
}

func (*SubscriptExpr) astNode()  {}
func (*SubscriptExpr) exprNode() {}

// String returns the Cypher subscript expression.
func (s *SubscriptExpr) String() string {
	return s.Expr.String() + "[" + s.Index.String() + "]"
}

// SliceExpr is a slice expression: expr[from..to].
type SliceExpr struct {
	Expr   Expression
	From   Expression // nil when absent
	To     Expression // nil when absent
	Pos    Position
	EndPos Position
}

func (*SliceExpr) astNode()  {}
func (*SliceExpr) exprNode() {}

// String returns the Cypher slice expression.
func (s *SliceExpr) String() string {
	out := s.Expr.String() + "["
	if s.From != nil {
		out += s.From.String()
	}
	out += ".."
	if s.To != nil {
		out += s.To.String()
	}
	out += "]"
	return out
}

// ReduceExpr is a reduce expression: reduce(acc = init, x IN list | expr).
// AccVar is the accumulator variable name, Init is the initial value expression,
// ElemVar is the loop variable name, Source is the list expression, and
// Projection is the accumulation expression evaluated on each iteration.
type ReduceExpr struct {
	Init       Expression
	Source     Expression
	Projection Expression
	AccVar     string
	ElemVar    string
	Pos        Position
	EndPos     Position
}

func (*ReduceExpr) astNode()  {}
func (*ReduceExpr) exprNode() {}

// String returns the Cypher reduce expression.
func (r *ReduceExpr) String() string {
	return "reduce(" + r.AccVar + " = " + r.Init.String() +
		", " + r.ElemVar + " IN " + r.Source.String() +
		" | " + r.Projection.String() + ")"
}
