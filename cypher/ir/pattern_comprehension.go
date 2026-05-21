package ir

import (
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
	// Collect outer variables for the Argument leaf.
	var corrVars []string
	if outer != nil {
		corrVars = outer.Vars()
	}
	arg := NewArgument(corrVars)

	// Translate the path pattern using the Argument leaf as the base.
	// PatternComprehension.Pattern is a single *PathPattern; wrap it in a
	// *Pattern (which holds a slice of paths) so we can reuse matchPattern.
	var inner LogicalPlan
	if pc.Pattern != nil {
		pat := &ast.Pattern{Paths: []*ast.PathPattern{pc.Pattern}}
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

	// Project the comprehension's projection expression.
	if pc.Projection != nil {
		projName := pc.Projection.String()
		inner = NewProjection([]ProjectionItem{
			{Name: projName, Expression: projName},
		}, inner)
	}

	if outputVar == "" {
		outputVar = pc.String()
	}

	return NewRollUpApply(outer, inner, outputVar), nil
}

// projectionsWithComprehensions scans projection items and, for any item whose
// expression is a PatternComprehension, replaces it with a RollUpApply sub-tree
// layered on top of plan. Non-comprehension items are collected into a
// Projection that wraps the final plan.
//
// This is called from translateReturn / translateWith after basic aggregation
// detection, so the comprehension items are processed last.
func (t *translator) projectionsWithComprehensions(
	proj *ast.Projection,
	plan LogicalPlan,
) (LogicalPlan, []ProjectionItem, error) {
	if proj == nil {
		return plan, nil, nil
	}

	var regularItems []ProjectionItem
	for _, item := range proj.Items {
		pc, ok := item.Expr.(*ast.PatternComprehension)
		if !ok {
			// Regular item — pass through, preserving the parsed AST.
			name := item.Expr.String()
			if item.Alias != nil {
				name = *item.Alias
			} else if v, ok2 := item.Expr.(*ast.Variable); ok2 {
				name = v.Name
			}
			regularItems = append(regularItems, ProjectionItem{
				Name:       name,
				Expression: item.Expr.String(),
				Expr:       item.Expr,
			})
			continue
		}

		// Determine the output variable name.
		outputVar := pc.String()
		if item.Alias != nil {
			outputVar = *item.Alias
		}

		var err error
		plan, err = t.translatePatternComprehension(pc, plan, outputVar)
		if err != nil {
			return nil, nil, err
		}
	}

	return plan, regularItems, nil
}
