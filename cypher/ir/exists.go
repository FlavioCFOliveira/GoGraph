package ir

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
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
// correlationVars are the variable names that are in scope in the outer plan,
// computed by [liveOutputVarSlice]. Neither of the two obvious shortcuts is
// correct: outer.Vars() is too NARROW and collectAllVars(outer) is too WIDE.
// See [existsSubPlan] for both failure modes.

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
// outer bindings into the inner evaluation. The Argument's Tag is shared with
// the enclosing SemiApply/AntiSemiApply via outerArgTag so the exec layer can
// route the matching exec.Argument instance per outer row.
//
// The correlation set must be the outer plan's SCOPE, and it is computed by
// [liveOutputVarSlice]. Getting it from either of the two neighbouring
// definitions is wrong, in opposite directions, and each error is a silent
// wrong answer rather than a failure (rmp #2659).
//
// Too NARROW — outer.Vars(). [LogicalPlan.Vars] is contracted to report only
// the variables an operator introduces or requires, so every pipeline operator
// that appends a binding without re-declaring its child's under-reports the
// scope. Given
// `MATCH (a:P) CREATE (:Q) WITH a WHERE EXISTS { MATCH (a)-[:Z]->(:P) }` the
// outer plan's root is [CreateNode], whose Vars() is just the created node's
// synthetic name, so `a` never reaches the Argument. Two defects follow from
// that one omission:
//
//  1. matchPattern treats the subquery's `(a)` as a fresh variable and plans an
//     [AllNodesScan]. The EXISTS silently stops being correlated and reports
//     true for every outer row as soon as any matching relationship exists
//     anywhere in the graph.
//  2. That AllNodesScan re-registers `a` in the physical builder's shared
//     column-index map at an inner-side index. SemiApply forwards the OUTER
//     row, which is narrower, so every downstream read of `a` — `a.sid`,
//     `id(a)`, `a` itself — lands on a slot the row does not carry and
//     evaluates to null.
//
// Too WIDE — collectAllVars(outer). That walker descends every child, and
// [SemiApply.Children] / [AntiSemiApply.Children] return {Outer, Inner} while
// [SemiApply.Vars] is documented as "only outer variables are visible
// downstream". It therefore harvests a PRIOR subquery's private bindings, and
// upstream-only names a [Projection] has already dropped, against both
// operators' own contracts. openCypher makes this a correctness question, not a
// tidiness one: CIP2015-05-13-EXISTS states that both forms of existential
// subquery "are allowed to introduce new variables" and that "any variables
// introduced in an <ExistentialSubquery> are not available outside the subquery
// context". So in
// `… WHERE EXISTS { MATCH (a)-[r:Z]->(b:P) } WITH a WHERE EXISTS { MATCH (b)-[:Z]->(:P) }`
// the second subquery's `b` is a BINDING occurrence of a new variable, and that
// subquery is legally UNCORRELATED. Claiming `b` as a correlation variable
// makes it resolve against an absent slot and wrongly returns zero rows.
//
// This mirrors the decision matchPattern already took: its boundVars set was
// moved off collectAllVars and onto the same scope-aware walker in TCK round 73
// (see cypher/tck/runner_test.go, the baseline-3872 note, change (b), and the
// in-line comment in [translator.matchPattern]), because upstream-only
// variables after a Projection "must NOT drive destRebinding (which would emit
// a `b = synthetic` Selection that always rejects against an absent `b` slot)"
// — the same failure mode, reached from the same over-wide seed. existsSubPlan
// was simply never migrated with it.
//
// Note that the scope cut of the WITH that OWNS this WHERE does not apply here:
// [translator.translateWith] calls translateExistsPredicate against the
// PRE-projection plan, so a variable bound before the WITH and not projected by
// it is still correctly in scope for the WITH's own WHERE. Our TCK pins that
// (clauses/with-where/WithWhere7.feature [1] and [3]).
func (t *translator) existsSubPlan(exists *ast.ExistsSubquery, outer LogicalPlan, outerArgTag uint32) (LogicalPlan, error) {
	// Scope-accurate and deterministically ordered — see the doc comment.
	corrVars := liveOutputVarSlice(outer)
	arg := NewArgumentWithTag(corrVars, outerArgTag)

	// EXISTS { pattern } — translate the pattern with the Argument as base.
	if exists.Pattern != nil {
		plan, err := t.matchPattern(exists.Pattern, arg, false)
		if err != nil {
			return nil, err
		}
		// Inline WHERE clause inside the pattern form (e.g.
		// EXISTS { (n)-->(m) WHERE n.prop = m.prop }) becomes a
		// Selection wrapping the matched plan, so SemiApply / AntiSemiApply
		// only see rows that satisfy the WHERE filter.
		if exists.Where != nil {
			plan = NewSelectionExpr(exists.Where.Predicate.String(), exists.Where.Predicate, plan)
		}
		return plan, nil
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
