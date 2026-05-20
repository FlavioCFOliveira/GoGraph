package ir

import (
	"fmt"

	"gograph/cypher/ast"
)

// ─────────────────────────────────────────────────────────────────────────────
// MATCH / OPTIONAL MATCH translation
// ─────────────────────────────────────────────────────────────────────────────

// translateMatch converts a MATCH clause into a logical plan subtree.
//
// Algorithm:
//  1. Each comma-separated path in the pattern is translated independently,
//     starting from a nil child (it is a new scan root, not a continuation of
//     an existing plan).
//  2. If there are multiple paths, they are combined left-to-right into an
//     Apply chain, which implements Cartesian-product semantics: for every row
//     produced by the left side, the right side is re-evaluated with the outer
//     bindings injected via an Argument leaf.
//  3. If there is an existing child plan (the plan accumulated so far by
//     preceding reading clauses), it is prepended to the Apply chain so that
//     the MATCH is correlated to the preceding context.
//  4. Inline property predicates on node patterns become Selection operators
//     immediately above the scan/expand, which is the lowest legal position.
//  5. A WHERE predicate is lifted as a Selection on top of the entire pattern plan.
//
// optional=true emits OptionalExpand instead of Expand for relationship hops.
func (t *translator) translateMatch(m *ast.Match, child LogicalPlan, optional bool) (LogicalPlan, error) {
	plan, err := t.matchPattern(m.Pattern, child, optional)
	if err != nil {
		return nil, err
	}
	if m.Where != nil {
		plan = NewSelection(m.Where.Predicate.String(), plan)
	}
	return plan, nil
}

// translateOptionalMatch converts an OPTIONAL MATCH clause into a plan.
func (t *translator) translateOptionalMatch(m *ast.OptionalMatch, child LogicalPlan) (LogicalPlan, error) {
	plan, err := t.matchPattern(m.Pattern, child, true)
	if err != nil {
		return nil, err
	}
	if m.Where != nil {
		plan = NewSelection(m.Where.Predicate.String(), plan)
	}
	return plan, nil
}

// matchPattern translates a MATCH Pattern (comma-separated path list) into a
// plan. Multiple paths are joined left-to-right using Apply (Cartesian product).
// When child is non-nil (preceding reading clauses already produced a plan),
// child is used as the outer side of the first Apply.
func (t *translator) matchPattern(pat *ast.Pattern, child LogicalPlan, optional bool) (LogicalPlan, error) {
	if pat == nil || len(pat.Paths) == 0 {
		return child, nil
	}

	// Translate every path independently into its own scan/expand tree.
	paths := make([]LogicalPlan, 0, len(pat.Paths))
	for _, pp := range pat.Paths {
		p, err := t.matchPathPattern(pp, optional)
		if err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}

	// Fold paths left-to-right into an Apply chain.
	// If there is an existing child plan, prepend it.
	var plan LogicalPlan
	if child != nil {
		plan = child
	} else {
		plan = paths[0]
		paths = paths[1:]
	}

	for _, p := range paths {
		plan = NewApply(plan, p)
	}
	return plan, nil
}

// matchPathPattern translates a single PathPattern into a scan/expand subtree.
// Unlike pathPattern in translator.go, it does NOT accept a child: each path
// starts fresh so that matchPattern can compose them via Apply.
func (t *translator) matchPathPattern(pp *ast.PathPattern, optional bool) (LogicalPlan, error) {
	if pp == nil || pp.Head == nil {
		return nil, nil
	}

	el := pp.Head

	// The first element is always a node; translate it as a scan.
	var plan LogicalPlan
	if el.Node != nil {
		plan = t.matchNodeScan(el.Node)
	}

	// Walk remaining (rel, node) pairs.
	el = el.Next
	for el != nil {
		if el.Relationship != nil && el.Node != nil {
			plan = t.matchExpandStep(el.Relationship, el.Node, plan, optional)
		}
		el = el.Next
	}
	return plan, nil
}

// matchNodeScan produces AllNodesScan or NodeByLabelScan for a NodePattern,
// with inline property predicates wrapped as Selection operators immediately
// above the scan — the lowest legal position.
func (t *translator) matchNodeScan(np *ast.NodePattern) LogicalPlan {
	nodeVar := ""
	if np.Variable != nil {
		nodeVar = *np.Variable
	}

	if len(np.Labels) == 0 {
		scan := NewAllNodesScan(nodeVar)
		if np.Properties != nil {
			return NewSelection(nodePropertiesPredicate(nodeVar, np.Properties), scan)
		}
		return scan
	}

	// Use the first label for the scan; additional labels become Selection operators.
	scan := NewNodeByLabelScan(nodeVar, np.Labels[0])
	var plan LogicalPlan = scan

	// Extra labels: AND-filter as Selection operators, innermost first.
	for _, lbl := range np.Labels[1:] {
		pred := fmt.Sprintf("%s:%s", nodeVar, lbl)
		plan = NewSelection(pred, plan)
	}

	// Inline property predicates sit above label selections, still below any
	// WHERE predicate — the lowest legal position.
	if np.Properties != nil {
		plan = NewSelection(nodePropertiesPredicate(nodeVar, np.Properties), plan)
	}

	return plan
}

// matchExpandStep translates a single (rel, node) hop into Expand or
// OptionalExpand, then wraps destination-node label/property constraints as
// Selection operators immediately above the expand.
func (t *translator) matchExpandStep(rp *ast.RelationshipPattern, to *ast.NodePattern, child LogicalPlan, optional bool) LogicalPlan {
	fromVar := firstVar(child)

	relVar := ""
	if rp.Variable != nil {
		relVar = *rp.Variable
	}

	toVar := ""
	if to.Variable != nil {
		toVar = *to.Variable
	}

	dir := relDirection(rp.Direction)
	relTypes := make([]string, len(rp.Types))
	copy(relTypes, rp.Types)

	// Variable-length expansion (e.g. -[r*1..3]->).
	if rp.Range != nil {
		minDepth := 1
		maxDepth := 0 // 0 means unbounded
		if rp.Range.Min != nil {
			minDepth = int(*rp.Range.Min)
		}
		if rp.Range.Max != nil {
			maxDepth = int(*rp.Range.Max)
		}
		plan := NewVarLengthExpand(fromVar, relVar, relTypes, dir, toVar, minDepth, maxDepth, child)
		return t.matchApplyNodeFilter(to, toVar, plan)
	}

	var plan LogicalPlan
	if optional {
		plan = NewOptionalExpand(fromVar, relVar, relTypes, dir, toVar, child)
	} else {
		plan = NewExpand(fromVar, relVar, relTypes, dir, toVar, child)
	}

	return t.matchApplyNodeFilter(to, toVar, plan)
}

// matchApplyNodeFilter wraps plan with Selection operators for label and
// property constraints declared on the destination node pattern.
func (t *translator) matchApplyNodeFilter(np *ast.NodePattern, nodeVar string, plan LogicalPlan) LogicalPlan {
	for _, lbl := range np.Labels {
		pred := fmt.Sprintf("%s:%s", nodeVar, lbl)
		plan = NewSelection(pred, plan)
	}
	if np.Properties != nil {
		plan = NewSelection(nodePropertiesPredicate(nodeVar, np.Properties), plan)
	}
	return plan
}
