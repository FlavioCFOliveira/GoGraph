package sema_test

// Tests for the checkExpr recursion-depth budget (task #1424).
//
// The pre-parse guard in cypher/parser/guard.go deliberately excludes '*' and
// '-' from countBinaryOpTokens because those characters appear structurally in
// relationship arrows and varlen path patterns. An adversarial query can
// therefore build an arbitrarily deep BinaryOp spine using multiplication or
// subtraction and drive checkExpr into a Go stack overflow.
//
// The fix adds a maxExprDepth budget (1 000) tracked via analyser.exprDepth:
// when the limit is exceeded checkExpr records a KindExpressionTooDeep
// ScopeError and returns without recursing further.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
)

// buildDeepMulChain returns a left-deep BinaryOp chain of n '*' operations:
//
//	((1 * 1) * 1) * … * 1   (n multiplications, n+1 integer leaves)
func buildDeepMulChain(n int) ast.Expression {
	var e ast.Expression = &ast.IntLiteral{Value: 1}
	for i := 0; i < n; i++ {
		e = &ast.BinaryOp{
			Operator: "*",
			Left:     e,
			Right:    &ast.IntLiteral{Value: 1},
		}
	}
	return e
}

// buildDeepSubChain returns a left-deep BinaryOp chain of n '-' operations.
func buildDeepSubChain(n int) ast.Expression {
	var e ast.Expression = &ast.IntLiteral{Value: 1}
	for i := 0; i < n; i++ {
		e = &ast.BinaryOp{
			Operator: "-",
			Left:     e,
			Right:    &ast.IntLiteral{Value: 1},
		}
	}
	return e
}

// wrapInReturn wraps an expression as a RETURN <expr> query.
func wrapInReturn(expr ast.Expression) ast.Query {
	alias := "x"
	return &ast.SingleQuery{
		Return: &ast.Return{
			Projection: &ast.Projection{
				Items: []*ast.ProjectionItem{
					{Expr: expr, Alias: &alias},
				},
			},
		},
	}
}

// TestCheckExprDepthBudgetMul verifies that a multiplication chain deeper than
// maxExprDepth (1 000) is rejected with KindExpressionTooDeep instead of
// crashing the goroutine with a stack overflow.
func TestCheckExprDepthBudgetMul(t *testing.T) {
	const chainLen = 1500 // well above maxExprDepth (1 000)
	q := wrapInReturn(buildDeepMulChain(chainLen))

	errs := sema.Analyse(q)
	if len(errs) == 0 {
		t.Fatal("expected at least one ScopeError for deep '*' chain, got none")
	}
	found := false
	for _, e := range errs {
		if e.Kind == sema.KindExpressionTooDeep {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected KindExpressionTooDeep in errors, got: %v", errs)
	}
}

// TestCheckExprDepthBudgetSub verifies the same guarantee for '-' chains.
func TestCheckExprDepthBudgetSub(t *testing.T) {
	const chainLen = 1500
	q := wrapInReturn(buildDeepSubChain(chainLen))

	errs := sema.Analyse(q)
	if len(errs) == 0 {
		t.Fatal("expected at least one ScopeError for deep '-' chain, got none")
	}
	found := false
	for _, e := range errs {
		if e.Kind == sema.KindExpressionTooDeep {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected KindExpressionTooDeep in errors, got: %v", errs)
	}
}

// TestCheckExprDepthBudgetNormalQuery verifies that a legitimate query with
// shallow operator nesting (well below maxExprDepth) is NOT rejected.
func TestCheckExprDepthBudgetNormalQuery(t *testing.T) {
	// RETURN 1 * 2 * 3 * 4 * 5  — five multiplications, depth=5, safe.
	q := wrapInReturn(buildDeepMulChain(5))

	errs := sema.Analyse(q)
	for _, e := range errs {
		if e.Kind == sema.KindExpressionTooDeep {
			t.Errorf("normal shallow query unexpectedly rejected with KindExpressionTooDeep")
		}
	}
}

// TestCheckExprDepthErrorMessage verifies the error message text for the depth
// limit, confirming it is human-readable and cites the limit.
func TestCheckExprDepthErrorMessage(t *testing.T) {
	const chainLen = 1500
	q := wrapInReturn(buildDeepMulChain(chainLen))

	errs := sema.Analyse(q)
	for _, e := range errs {
		if e.Kind == sema.KindExpressionTooDeep {
			if !strings.Contains(e.Message, "1000") {
				t.Errorf("error message %q should cite the limit 1000", e.Message)
			}
			return
		}
	}
	t.Fatal("KindExpressionTooDeep error not found")
}
