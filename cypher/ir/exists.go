package ir

import (
	"gograph/cypher/ast"
)

// exists.go — EXISTS / NOT EXISTS subquery translation.
//
// EXISTS { … } and NOT EXISTS { … } appear as expressions inside WHERE
// predicates. The translator intercepts them when a WHERE predicate is a
// top-level ExistsSubquery (or a NOT-wrapped ExistsSubquery) and emits a
// SemiApply / AntiSemiApply instead of a Selection.
//
// Translation strategy:
//
//  1. EXISTS { MATCH (a)-[:R]->(b) }
//     → SemiApply(outer=currentPlan, inner=subPlan)
//     where subPlan uses Argument(correlationVars) as its leaf so that outer
//     bindings are injected into the inner evaluation.
//
//  2. NOT EXISTS { … }
//     → AntiSemiApply(outer=currentPlan, inner=subPlan)
//
//  3. All other predicates fall back to a plain Selection, preserving
//     backward compatibility.
//
// correlationVars are the variable names that are in scope in the outer plan.
// We approximate them as the outer plan's Vars() when available.

// translateExistsPredicate inspects predExpr and, if it is a top-level EXISTS
// or NOT EXISTS pattern, produces a SemiApply / AntiSemiApply. For all other
// predicates it returns a Selection wrapping the child.
//
// outer is the plan produced so far (before the WHERE clause).
func (t *translator) translateExistsPredicate(predExpr ast.Expression, outer LogicalPlan) (LogicalPlan, error) {
	// Case 1: EXISTS { … }
	if exists, ok := predExpr.(*ast.ExistsSubquery); ok {
		tag := nextArgTag()
		inner, err := t.existsSubPlan(exists, outer, tag)
		if err != nil {
			return nil, err
		}
		return NewSemiApplyWithTag(outer, inner, tag), nil
	}

	// Case 2: NOT EXISTS { … } — represented as UnaryOp{"NOT", ExistsSubquery}
	if notOp, ok := predExpr.(*ast.UnaryOp); ok && notOp.Operator == "NOT" {
		if exists, ok := notOp.Operand.(*ast.ExistsSubquery); ok {
			tag := nextArgTag()
			inner, err := t.existsSubPlan(exists, outer, tag)
			if err != nil {
				return nil, err
			}
			return NewAntiSemiApplyWithTag(outer, inner, tag), nil
		}
	}

	// Case 3: plain predicate → Selection (with AST preserved for execution).
	return NewSelectionExpr(predExpr.String(), predExpr, outer), nil
}

// existsSubPlan builds the inner plan for a SemiApply / AntiSemiApply.
//
// The inner plan uses Argument(correlationVars) as its leaf, which injects the
// outer bindings into the inner evaluation. The correlation variables are the
// variables produced by the outer plan, and the Argument's Tag is shared with
// the enclosing SemiApply/AntiSemiApply via outerArgTag so the exec layer can
// route the matching exec.Argument instance per outer row.
func (t *translator) existsSubPlan(exists *ast.ExistsSubquery, outer LogicalPlan, outerArgTag uint32) (LogicalPlan, error) {
	// Collect correlation variables from the outer plan.
	var corrVars []string
	if outer != nil {
		corrVars = outer.Vars()
	}
	arg := NewArgumentWithTag(corrVars, outerArgTag)

	// EXISTS { pattern } — translate the pattern with the Argument as base.
	if exists.Pattern != nil {
		return t.matchPattern(exists.Pattern, arg, false)
	}

	// EXISTS { MATCH … } — translate the full subquery.
	if exists.Query != nil {
		// Replace the first reading clause scan root with arg by processing the
		// subquery but pre-seeding the plan with arg.
		plan := LogicalPlan(arg)
		for _, rc := range exists.Query.ReadingClauses {
			var err error
			plan, err = t.readingClause(rc, plan)
			if err != nil {
				return nil, err
			}
		}
		return plan, nil
	}

	// Empty EXISTS — degenerate; return Argument alone.
	return arg, nil
}
