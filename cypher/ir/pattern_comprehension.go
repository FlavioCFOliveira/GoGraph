package ir

import (
	"fmt"

	"gograph/cypher/ast"
)

// pattern_comprehension.go — PatternComprehension → RollUpApply translation.
//
// A Cypher pattern comprehension:
//
//	[(a)-[:R]->(b) | b.name]
//
// is translated as:
//
//	RollUpApply(outer, inner, collectVar)
//
// where:
//   - outer   is the current plan (the row context in which the comprehension
//             is evaluated).
//   - inner   is a sub-plan that enumerates the matches of the path pattern
//             relative to the outer bindings (rooted at Argument(outerVars)),
//             optionally filtered by the comprehension's WHERE predicate, and
//             ending with a Projection of the comprehension's projection
//             expression.
//   - collectVar is the output variable that receives the collected list. When
//             the comprehension is used inside a RETURN/WITH alias that is known
//             to the caller, the caller passes that alias; otherwise the
//             comprehension's String() representation is used as the name.

// translatePatternComprehension builds the RollUpApply sub-tree for a
// PatternComprehension expression found in a projection item.
//
// outer is the driving plan. outputVar is the name of the output variable
// (usually the alias from the projection item).
func (t *translator) translatePatternComprehension(
	pc *ast.PatternComprehension,
	outer LogicalPlan,
	outputVar string,
) (LogicalPlan, error) {
	// Collect outer variables for the Argument leaf. The leaf carries an
	// explicit tag so the physical builder routes the matching
	// exec.Argument instance allocated by RollUpApply. Use collectAllVars
	// (recursive) rather than outer.Vars() (top-level only) so that
	// FromVar of an Expand outer — which Expand.Vars() omits — still ends
	// up in the correlation set. Without this, matchPattern sees the
	// leading node as unbound and falls back to a Cartesian Apply with a
	// fresh AllNodesScan over the whole graph instead of correlating to
	// the outer row.
	var corrVars []string
	if outer != nil {
		corrVars = collectAllVars(outer)
	}
	tag := nextArgTag()
	arg := NewArgumentWithTag(corrVars, tag)

	// Translate the path pattern using the Argument leaf as the base.
	// PatternComprehension.Pattern is a single *PathPattern; wrap it in a
	// *Pattern (which holds a slice of paths) so we can reuse matchPattern.
	// When the comprehension declares a named path (`p = (a)-->(b) | p`)
	// propagate the variable onto the wrapped PathPattern so the
	// matcher registers the path metadata that buildIRProjection /
	// buildRowCtx rely on to reconstruct a PathValue.
	var inner LogicalPlan
	if pc.Pattern != nil {
		pp := pc.Pattern
		if pc.Variable != nil && pp.Variable == nil {
			cp := *pp
			cp.Variable = pc.Variable
			pp = &cp
		}
		pat := &ast.Pattern{Paths: []*ast.PathPattern{pp}}
		var err error
		inner, err = t.matchPattern(pat, arg, false)
		if err != nil {
			return nil, err
		}
	} else {
		inner = arg
	}

	// Apply an optional WHERE predicate inside the comprehension.
	if pc.Predicate != nil {
		inner = NewSelection(pc.Predicate.String(), inner)
	}

	// Project the comprehension's projection expression. The collected
	// list contains the value of this projection at each Inner row.
	// Pass the parsed AST via Expr so the executor evaluates the
	// expression via expr.Eval rather than falling back to a schema-
	// key lookup that would miss property-access shapes like b.name.
	if pc.Projection != nil {
		projName := pc.Projection.String()
		inner = NewProjection([]ProjectionItem{
			{Name: projName, Expression: projName, Expr: pc.Projection},
		}, inner)
	}

	if outputVar == "" {
		outputVar = pc.String()
	}

	rua := NewRollUpApply(outer, inner, outputVar)
	rua.ArgTag = tag
	return rua, nil
}

// projectionsWithComprehensions scans projection items and, for any item that
// contains a PatternComprehension at the top level OR nested inside a larger
// expression, replaces each comprehension with a RollUpApply sub-tree layered
// on top of plan and substitutes a Variable reference to the synthetic
// collected-list column. Non-comprehension items pass through unchanged.
//
// Top-level form (`RETURN [(n)-->(b) | b.name] AS list`): the surrounding
// projection item collapses into a bare Variable reference to the
// RollUpApply's output variable, mirroring the historical behaviour.
//
// Nested form (`RETURN size([(n)-->(b) | 1]) AS deg`): the comprehension is
// extracted into a synthetic `__pc_<n>` column via RollUpApply and the
// surrounding expression (size(...)) is rewritten so the Variable reference
// points at that column. The wrapping expression is preserved on the
// projection item's Expr field so the physical projection still evaluates
// the outer call (size, length, head, ...) against the row.
//
// Aggregate-argument form (`RETURN count([p = (n)-->() | p]) AS c`): the
// comprehension lives inside an aggregate FunctionInvocation. The walker
// rewrites the argument to reference the synthetic column, and the returned
// rewrittenProj carries the same substitution so the caller's
// detectAggregation sees count(__pc_<n>) rather than the raw comprehension.
//
// This is called from translateReturn / translateWith BEFORE detectAggregation
// so the aggregation pipeline never observes a raw PatternComprehension.
func (t *translator) projectionsWithComprehensions(
	proj *ast.Projection,
	plan LogicalPlan,
) (LogicalPlan, []ProjectionItem, *ast.Projection, error) {
	if proj == nil {
		return plan, nil, nil, nil
	}

	var regularItems []ProjectionItem
	rewrittenItems := make([]*ast.ProjectionItem, 0, len(proj.Items))
	pcCounter := 0
	for _, item := range proj.Items {
		// Top-level PatternComprehension: keep the historical fast-path
		// that collapses the projection item to a bare Variable.
		if pc, ok := item.Expr.(*ast.PatternComprehension); ok {
			outputVar := pc.String()
			if item.Alias != nil {
				outputVar = *item.Alias
			}
			var err error
			plan, err = t.translatePatternComprehension(pc, plan, outputVar)
			if err != nil {
				return nil, nil, nil, err
			}
			regularItems = append(regularItems, ProjectionItem{
				Name:       outputVar,
				Expression: outputVar,
			})
			itemCopy := *item
			itemCopy.Expr = &ast.Variable{Name: outputVar}
			rewrittenItems = append(rewrittenItems, &itemCopy)
			continue
		}

		// Walk the item expression for nested PatternComprehensions. Each
		// occurrence is hoisted into its own RollUpApply layer; the
		// expression tree is rewritten to reference the synthetic
		// column. The original expression survives only in the rewritten
		// form — the physical projection evaluates size/length/...
		// against the row, where the synthetic column now lives.
		rewritten, newPlan, err := t.extractNestedPatternComprehensions(item.Expr, plan, &pcCounter)
		if err != nil {
			return nil, nil, nil, err
		}
		plan = newPlan
		regularItems = append(regularItems, ProjectionItem{
			Name:       projectionColumnName(item),
			Expression: rewritten.String(),
			Expr:       rewritten,
		})
		itemCopy := *item
		itemCopy.Expr = rewritten
		rewrittenItems = append(rewrittenItems, &itemCopy)
	}

	rewrittenProj := *proj
	rewrittenProj.Items = rewrittenItems
	return plan, regularItems, &rewrittenProj, nil
}

// extractNestedPatternComprehensions walks e and replaces every
// *ast.PatternComprehension found inside (but not at the top level —
// callers handle the top-level case separately) with an
// *ast.Variable reference to a synthetic `__pc_<n>` column produced by a
// fresh RollUpApply layer over plan. counter is incremented per
// extraction to keep synthetic names unique across an entire projection.
// The returned (rewritten, plan) pair is suitable for direct use as the
// item's Expr and the carrying plan node.
func (t *translator) extractNestedPatternComprehensions(
	e ast.Expression,
	plan LogicalPlan,
	counter *int,
) (ast.Expression, LogicalPlan, error) {
	if e == nil {
		return nil, plan, nil
	}
	if pc, ok := e.(*ast.PatternComprehension); ok {
		name := fmt.Sprintf("__pc_%d", *counter)
		*counter++
		newPlan, err := t.translatePatternComprehension(pc, plan, name)
		if err != nil {
			return nil, plan, err
		}
		return &ast.Variable{Name: name}, newPlan, nil
	}
	switch n := e.(type) {
	case *ast.BinaryOp:
		left, plan2, err := t.extractNestedPatternComprehensions(n.Left, plan, counter)
		if err != nil {
			return nil, plan, err
		}
		right, plan3, err := t.extractNestedPatternComprehensions(n.Right, plan2, counter)
		if err != nil {
			return nil, plan, err
		}
		cp := *n
		cp.Left = left
		cp.Right = right
		return &cp, plan3, nil
	case *ast.UnaryOp:
		op, plan2, err := t.extractNestedPatternComprehensions(n.Operand, plan, counter)
		if err != nil {
			return nil, plan, err
		}
		cp := *n
		cp.Operand = op
		return &cp, plan2, nil
	case *ast.Property:
		rec, plan2, err := t.extractNestedPatternComprehensions(n.Receiver, plan, counter)
		if err != nil {
			return nil, plan, err
		}
		cp := *n
		cp.Receiver = rec
		return &cp, plan2, nil
	case *ast.FunctionInvocation:
		newArgs := make([]ast.Expression, len(n.Args))
		curPlan := plan
		for i, arg := range n.Args {
			a2, p2, err := t.extractNestedPatternComprehensions(arg, curPlan, counter)
			if err != nil {
				return nil, plan, err
			}
			newArgs[i] = a2
			curPlan = p2
		}
		cp := *n
		cp.Args = newArgs
		return &cp, curPlan, nil
	case *ast.SubscriptExpr:
		ex, plan2, err := t.extractNestedPatternComprehensions(n.Expr, plan, counter)
		if err != nil {
			return nil, plan, err
		}
		ix, plan3, err := t.extractNestedPatternComprehensions(n.Index, plan2, counter)
		if err != nil {
			return nil, plan, err
		}
		cp := *n
		cp.Expr = ex
		cp.Index = ix
		return &cp, plan3, nil
	case *ast.SliceExpr:
		ex, plan2, err := t.extractNestedPatternComprehensions(n.Expr, plan, counter)
		if err != nil {
			return nil, plan, err
		}
		fr, plan3, err := t.extractNestedPatternComprehensions(n.From, plan2, counter)
		if err != nil {
			return nil, plan, err
		}
		to, plan4, err := t.extractNestedPatternComprehensions(n.To, plan3, counter)
		if err != nil {
			return nil, plan, err
		}
		cp := *n
		cp.Expr = ex
		cp.From = fr
		cp.To = to
		return &cp, plan4, nil
	case *ast.ListLiteral:
		newElems := make([]ast.Expression, len(n.Elements))
		curPlan := plan
		for i, el := range n.Elements {
			e2, p2, err := t.extractNestedPatternComprehensions(el, curPlan, counter)
			if err != nil {
				return nil, plan, err
			}
			newElems[i] = e2
			curPlan = p2
		}
		cp := *n
		cp.Elements = newElems
		return &cp, curPlan, nil
	case *ast.MapLiteral:
		newVals := make([]ast.Expression, len(n.Values))
		curPlan := plan
		for i, val := range n.Values {
			v2, p2, err := t.extractNestedPatternComprehensions(val, curPlan, counter)
			if err != nil {
				return nil, plan, err
			}
			newVals[i] = v2
			curPlan = p2
		}
		cp := *n
		cp.Values = newVals
		return &cp, curPlan, nil
	case *ast.ListComprehension:
		// Only the Source sits in the outer scope where a hoisted
		// RollUpApply on plan would correlate correctly. Predicate /
		// Projection run per element with the iteration variable in
		// scope, so a PatternComprehension nested there would need to
		// see the element binding, not the outer row — recurse anyway:
		// the existing RollUpApply currently captures only outer
		// variables, so a nested correlated comprehension that depends
		// on the iteration variable is best-effort. The common openCypher
		// case (Pattern2 [7]) uses the iteration variable as the
		// PatternComprehension's source node, and the matcher resolves
		// it via the runtime row context the executor builds for the
		// surrounding list comprehension.
		src, plan2, err := t.extractNestedPatternComprehensions(n.Source, plan, counter)
		if err != nil {
			return nil, plan, err
		}
		pred, plan3, err := t.extractNestedPatternComprehensions(n.Predicate, plan2, counter)
		if err != nil {
			return nil, plan, err
		}
		proj, plan4, err := t.extractNestedPatternComprehensions(n.Projection, plan3, counter)
		if err != nil {
			return nil, plan, err
		}
		cp := *n
		cp.Source = src
		cp.Predicate = pred
		cp.Projection = proj
		return &cp, plan4, nil
	case *ast.CaseExpression:
		newAlts := make([]*ast.CaseAlternative, len(n.Alternatives))
		curPlan := plan
		var subj ast.Expression
		var err error
		subj, curPlan, err = t.extractNestedPatternComprehensions(n.Subject, curPlan, counter)
		if err != nil {
			return nil, plan, err
		}
		for i, alt := range n.Alternatives {
			cond, p2, err := t.extractNestedPatternComprehensions(alt.Condition, curPlan, counter)
			if err != nil {
				return nil, plan, err
			}
			cons, p3, err := t.extractNestedPatternComprehensions(alt.Consequent, p2, counter)
			if err != nil {
				return nil, plan, err
			}
			altCp := *alt
			altCp.Condition = cond
			altCp.Consequent = cons
			newAlts[i] = &altCp
			curPlan = p3
		}
		elseE, curPlan, err := t.extractNestedPatternComprehensions(n.ElseExpr, curPlan, counter)
		if err != nil {
			return nil, plan, err
		}
		cp := *n
		cp.Subject = subj
		cp.Alternatives = newAlts
		cp.ElseExpr = elseE
		return &cp, curPlan, nil
	}
	return e, plan, nil
}
