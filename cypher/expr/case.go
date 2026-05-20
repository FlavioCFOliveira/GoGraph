package expr

// case.go — CASE expression evaluation (task-263).
//
// Both the simple (value-form) and generic CASE expressions are evaluated here.
//
// Simple CASE: CASE subject WHEN v1 THEN r1 … [ELSE rn] END
//   - Each WHEN value is compared to the subject using 3VL equality.
//   - A NULL subject never matches any WHEN arm (NULL = x → NULL, not true).
//
// Generic CASE: CASE WHEN pred1 THEN r1 … [ELSE rn] END
//   - Each WHEN predicate is evaluated as a boolean.
//   - A NULL predicate counts as non-matching (falsy).
//
// In both forms the first matching arm short-circuits evaluation of subsequent
// arms. When no arm matches, the ELSE expression is evaluated; if there is no
// ELSE clause NULL is returned.

import "gograph/cypher/ast"

// evalCase evaluates a [ast.CaseExpression] in the context of row and params.
// It delegates to the simple-form or generic-form path depending on whether
// n.Subject is non-nil.
func evalCase(n *ast.CaseExpression, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	if n.Subject != nil {
		return evalCaseSimple(n, row, params, reg)
	}
	return evalCaseGeneric(n, row, params, reg)
}

// evalCaseSimple handles CASE subject WHEN v THEN r … [ELSE e] END.
//
// The subject is evaluated once. Each WHEN arm's condition is compared to the
// subject using 3VL equality: only IsTruthy(subject.Equal(cond)) triggers the
// match. A NULL subject never matches any arm.
func evalCaseSimple(n *ast.CaseExpression, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	subject, err := evalExpr(n.Subject, row, params, reg)
	if err != nil {
		return nil, err
	}

	for _, alt := range n.Alternatives {
		cond, err := evalExpr(alt.Condition, row, params, reg)
		if err != nil {
			return nil, err
		}
		// 3VL equality: NULL subject or NULL cond produces NULL, which is not truthy.
		if IsTruthy(subject.Equal(cond)) {
			return evalExpr(alt.Consequent, row, params, reg)
		}
	}

	if n.ElseExpr != nil {
		return evalExpr(n.ElseExpr, row, params, reg)
	}
	return Null, nil
}

// evalCaseGeneric handles CASE WHEN pred THEN r … [ELSE e] END.
//
// Each WHEN predicate is independently evaluated as a boolean. A NULL predicate
// is not truthy and therefore does not match.
func evalCaseGeneric(n *ast.CaseExpression, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	for _, alt := range n.Alternatives {
		cond, err := evalExpr(alt.Condition, row, params, reg)
		if err != nil {
			return nil, err
		}
		if IsTruthy(cond) {
			return evalExpr(alt.Consequent, row, params, reg)
		}
	}

	if n.ElseExpr != nil {
		return evalExpr(n.ElseExpr, row, params, reg)
	}
	return Null, nil
}
