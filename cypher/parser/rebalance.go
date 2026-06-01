package parser

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// liftListPredicate reshapes a freshly-constructed arithmetic BinaryOp to
// surface any inner IN / CONTAINS / STARTS WITH / ENDS WITH operator above
// the arithmetic. This is the canonical fix for openCypher operator
// precedence — the grammar consumes IN and the string predicates as
// postfixes on atomicExpression (giving them higher precedence than +/-/*//
// /%/^), but openCypher specifies the opposite ordering. The arithmetic
// visitors call this helper after constructing each step of an addSub /
// multDiv / power chain so the predicate bubbles up one level per
// surrounding arithmetic, eventually settling above the whole chain.
//
// Parenthesised predicates are skipped — when the user wrote `(2 IN [3])`
// inside an arithmetic chain the predicate is meant to be evaluated as a
// boolean operand of the arithmetic, and the visitor marks the BinaryOp
// with Parenthesized=true so this pass leaves it untouched. The flag is
// retained on the AST after parsing finishes; downstream consumers
// ignore it.
//
// liftListPredicate returns its argument unchanged when no lift applies.
func liftListPredicate(bo *ast.BinaryOp) ast.Expression {
	if bo == nil || !isArithmeticOp(bo.Operator) {
		return bo
	}
	// Right-hand pattern: arith(L, pred(X, Y)) → pred(arith(L, X), Y).
	if rb, ok := bo.Right.(*ast.BinaryOp); ok && !rb.Parenthesized && isListPredicateOp(rb.Operator) {
		return &ast.BinaryOp{
			Pos:      bo.Pos,
			EndPos:   bo.EndPos,
			Operator: rb.Operator,
			Left: &ast.BinaryOp{
				Pos:      bo.Pos,
				EndPos:   rb.Pos,
				Left:     bo.Left,
				Operator: bo.Operator,
				Right:    rb.Left,
			},
			Right: rb.Right,
		}
	}
	// Left-hand pattern: arith(pred(X, Y), R) → pred(X, arith(Y, R)).
	if lb, ok := bo.Left.(*ast.BinaryOp); ok && !lb.Parenthesized && isListPredicateOp(lb.Operator) {
		return &ast.BinaryOp{
			Pos:      bo.Pos,
			EndPos:   bo.EndPos,
			Operator: lb.Operator,
			Left:     lb.Left,
			Right: &ast.BinaryOp{
				Pos:      lb.EndPos,
				EndPos:   bo.EndPos,
				Left:     lb.Right,
				Operator: bo.Operator,
				Right:    bo.Right,
			},
		}
	}
	return bo
}

// isArithmeticOp reports whether op is one of the arithmetic infix operators
// that bind tighter than IN/CONTAINS/STARTS WITH/ENDS WITH per openCypher.
func isArithmeticOp(op string) bool {
	switch op {
	case "+", "-", "*", "/", "%", "^":
		return true
	}
	return false
}

// isListPredicateOp reports whether op is one of the list/string predicate
// operators consumed as a postfix on atomicExpression by the grammar.
func isListPredicateOp(op string) bool {
	switch op {
	case "IN", "CONTAINS", "STARTS WITH", "ENDS WITH":
		return true
	}
	return false
}
