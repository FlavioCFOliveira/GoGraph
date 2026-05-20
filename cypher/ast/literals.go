package ast

import (
	"fmt"
	"strconv"
	"strings"
)

// ----------------------------------------------------------------------------
// Literal nodes
// All literals implement Expression.
// ----------------------------------------------------------------------------

// IntLiteral is an integer literal value.
type IntLiteral struct {
	Pos   Position
	Value int64
}

func (*IntLiteral) astNode()  {}
func (*IntLiteral) exprNode() {}

// String returns the decimal representation of the integer.
func (n *IntLiteral) String() string { return strconv.FormatInt(n.Value, 10) }

// FloatLiteral is a floating-point literal value.
type FloatLiteral struct {
	Pos   Position
	Value float64
}

func (*FloatLiteral) astNode()  {}
func (*FloatLiteral) exprNode() {}

// String returns the decimal representation of the float.
func (n *FloatLiteral) String() string { return strconv.FormatFloat(n.Value, 'f', -1, 64) }

// StringLiteral is a single-quoted or double-quoted string literal.
type StringLiteral struct {
	Pos   Position
	Value string
}

func (*StringLiteral) astNode()  {}
func (*StringLiteral) exprNode() {}

// String returns the value enclosed in single quotes with internal single
// quotes escaped.
func (n *StringLiteral) String() string {
	escaped := strings.ReplaceAll(n.Value, "'", "\\'")
	return "'" + escaped + "'"
}

// BoolLiteral is the literal true or false.
type BoolLiteral struct {
	Pos   Position
	Value bool
}

func (*BoolLiteral) astNode()  {}
func (*BoolLiteral) exprNode() {}

// String returns "true" or "false".
func (n *BoolLiteral) String() string {
	if n.Value {
		return "true"
	}
	return "false"
}

// NullLiteral is the literal null.
type NullLiteral struct {
	Pos Position
}

func (*NullLiteral) astNode()  {}
func (*NullLiteral) exprNode() {}

// String returns "null".
func (n *NullLiteral) String() string { return "null" }

// ListLiteral is a bracketed list of expressions: [e1, e2, …].
type ListLiteral struct {
	Pos      Position
	Elements []Expression
}

func (*ListLiteral) astNode()  {}
func (*ListLiteral) exprNode() {}

// String returns the Cypher list literal.
func (n *ListLiteral) String() string {
	parts := make([]string, len(n.Elements))
	for i, e := range n.Elements {
		parts[i] = e.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// MapLiteral is a map expression: {key1: expr1, key2: expr2, …}.
type MapLiteral struct {
	Pos    Position
	Keys   []string
	Values []Expression
}

func (*MapLiteral) astNode()  {}
func (*MapLiteral) exprNode() {}

// String returns the Cypher map literal.
func (n *MapLiteral) String() string {
	if len(n.Keys) == 0 {
		return "{}"
	}
	parts := make([]string, len(n.Keys))
	for i, k := range n.Keys {
		parts[i] = fmt.Sprintf("%s: %s", k, n.Values[i].String())
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
