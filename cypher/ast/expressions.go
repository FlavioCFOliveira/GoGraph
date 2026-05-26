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
	Pos    Position
	EndPos Position
	Name   string
}

func (*Variable) astNode()  {}
func (*Variable) exprNode() {}

// String returns the variable name.
func (v *Variable) String() string { return v.Name }

// Parameter is a query parameter: $name or $0.
type Parameter struct {
	Pos    Position
	EndPos Position
	Name   string // the name/index without the leading '$'
}

func (*Parameter) astNode()  {}
func (*Parameter) exprNode() {}

// String returns the Cypher parameter reference.
func (p *Parameter) String() string { return "$" + p.Name }

// Property is a property access: expr.key.
type Property struct {
	Pos      Position
	EndPos   Position
	Receiver Expression
	Key      string
}

func (*Property) astNode()  {}
func (*Property) exprNode() {}

// String returns the Cypher property access.
func (p *Property) String() string { return p.Receiver.String() + "." + p.Key }

// FunctionInvocation is a function call: func(args…) or func(DISTINCT args…).
type FunctionInvocation struct {
	Pos       Position
	EndPos    Position
	Namespace []string // e.g. ["apoc", "path"] for apoc.path.expand
	Name      string
	Distinct  bool
	// CountStar is true when this is COUNT(*). String() renders it as
	// "count(*)" and downstream aggregation detects it without needing
	// a wildcard argument expression.
	CountStar bool
	Args      []Expression
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
	Pos      Position
	EndPos   Position
	Left     Expression
	Operator string // e.g. "+", "-", "=", "<>", "AND", "OR", "IN", "CONTAINS"
	Right    Expression
}

func (*BinaryOp) astNode()  {}
func (*BinaryOp) exprNode() {}

// String returns the Cypher infix expression.
func (b *BinaryOp) String() string {
	return "(" + b.Left.String() + " " + b.Operator + " " + b.Right.String() + ")"
}

// UnaryOp is a unary operator expression: OP expr.
type UnaryOp struct {
	Pos      Position
	EndPos   Position
	Operator string // e.g. "-", "NOT", "IS NULL", "IS NOT NULL"
	Operand  Expression
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

// CaseAlternative is a single WHEN … THEN … arm in a CASE expression.
type CaseAlternative struct {
	Pos        Position
	EndPos     Position
	Condition  Expression
	Consequent Expression
}

// String returns the WHEN…THEN arm.
func (c *CaseAlternative) String() string {
	return "WHEN " + c.Condition.String() + " THEN " + c.Consequent.String()
}

// CaseExpression is a CASE expression, either generic or value-form.
//
//	CASE [subject] WHEN … THEN … [ELSE …] END
type CaseExpression struct {
	Pos          Position
	EndPos       Position
	Subject      Expression // nil for generic CASE
	Alternatives []*CaseAlternative
	ElseExpr     Expression // nil when no ELSE clause
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
	Pos        Position
	EndPos     Position
	Variable   string
	Source     Expression
	Predicate  Expression // nil when no WHERE clause
	Projection Expression // nil when no projection expression
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
	Pos        Position
	EndPos     Position
	Variable   *string // optional path variable
	Pattern    *PathPattern
	Predicate  Expression // nil when no WHERE clause
	Projection Expression
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
	Pos    Position
	EndPos Position
	Key    string     // explicit key when present; otherwise empty
	Value  Expression // nil for the property-selector shorthand (`.key`)
	IsAll  bool       // true for the .*  selector
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
type MapProjection struct {
	Pos     Position
	EndPos  Position
	Subject Expression
	Items   []*MapProjectionItem
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
	Pos     Position
	EndPos  Position
	Pattern *Pattern     // pattern form: EXISTS { (a)-[r]->(b) }
	Query   *SingleQuery // full subquery form: EXISTS { MATCH … RETURN … }
}

func (*ExistsSubquery) astNode()  {}
func (*ExistsSubquery) exprNode() {}

// String returns the Cypher EXISTS subquery.
func (e *ExistsSubquery) String() string {
	if e.Pattern != nil {
		return "EXISTS { " + e.Pattern.String() + " }"
	}
	return "EXISTS { " + e.Query.String() + " }"
}

// CountSubquery is a COUNT { … } subquery expression.
type CountSubquery struct {
	Pos     Position
	EndPos  Position
	Pattern *Pattern     // pattern form
	Query   *SingleQuery // full subquery form
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
	Pos    Position
	EndPos Position
	Expr   Expression
	Index  Expression
}

func (*SubscriptExpr) astNode()  {}
func (*SubscriptExpr) exprNode() {}

// String returns the Cypher subscript expression.
func (s *SubscriptExpr) String() string {
	return s.Expr.String() + "[" + s.Index.String() + "]"
}

// SliceExpr is a slice expression: expr[from..to].
type SliceExpr struct {
	Pos    Position
	EndPos Position
	Expr   Expression
	From   Expression // nil when absent
	To     Expression // nil when absent
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
