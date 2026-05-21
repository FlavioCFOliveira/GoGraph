package ir

import (
	"fmt"

	"gograph/cypher/ast"
)

// buildPropertySelection wraps child with Selection operator(s) for inline node
// property predicates. When props is a *ast.MapLiteral the individual key-value
// pairs are converted to BinaryOp equality expressions so that the downstream
// physical builder can pattern-match them for index-seek rewrites.
//
// Single key:
//
//	{name: 'Alice'}  →  NewSelectionExpr("(n.name = 'Alice')", BinaryOp{…}, child)
//
// Multiple keys produce a chain of SelectionExpr nodes, innermost first.
// Non-MapLiteral expressions fall back to the opaque-string Selection.
func buildPropertySelection(nodeVar string, props ast.Expression, child LogicalPlan) LogicalPlan {
	ml, ok := props.(*ast.MapLiteral)
	if !ok || len(ml.Keys) == 0 {
		// Unknown expression type — fall back to opaque predicate.
		return NewSelection(nodeVar+" "+props.String(), child)
	}

	plan := child
	// Apply innermost first (first key closest to the scan leaf), so that when
	// the physical builder walks Selection→child it sees the scan directly.
	for i := 0; i < len(ml.Keys); i++ {
		binOp := &ast.BinaryOp{
			Operator: "=",
			Left: &ast.Property{
				Receiver: &ast.Variable{Name: nodeVar},
				Key:      ml.Keys[i],
			},
			Right: ml.Values[i],
		}
		plan = NewSelectionExpr(binOp.String(), binOp, plan)
	}
	return plan
}

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
		plan, err = t.translateExistsPredicate(m.Where.Predicate, plan)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// translateOptionalMatch converts an OPTIONAL MATCH clause into a plan.
//
// Semantics:
//   - When child is nil (OPTIONAL MATCH at the start of the query), each
//     relationship hop is built as OptionalExpand, which already preserves the
//     NULL-extended row when the per-hop expansion fails. The pattern's path
//     list is composed with the same shared-variable rules as MATCH.
//   - When child is non-nil, the OPTIONAL MATCH is correlated to the driving
//     subplan and must emit one NULL-extended row per outer row that fails to
//     match the entire pattern (including the leading node-scan, label, and
//     property predicates). This case is wrapped by an OptionalApply node;
//     the inner pattern uses regular Expand (not OptionalExpand) because the
//     outer OptionalApply already provides the full-pattern NULL emission.
func (t *translator) translateOptionalMatch(m *ast.OptionalMatch, child LogicalPlan) (LogicalPlan, error) {
	if child == nil {
		plan, err := t.matchPattern(m.Pattern, nil, true)
		if err != nil {
			return nil, err
		}
		if m.Where != nil {
			plan, err = t.translateExistsPredicate(m.Where.Predicate, plan)
			if err != nil {
				return nil, err
			}
		}
		return plan, nil
	}

	// Build the inner pattern as a standalone subtree using a fresh Argument
	// tag that the surrounding OptionalApply node will then carry. The leading
	// Argument leaf in the inner subtree is wired so the OptionalApply's
	// exec.Argument seeds the outer row into it at execution time.
	optTag := nextArgTag()
	innerPlan, err := t.optionalInnerPattern(m.Pattern, child, optTag)
	if err != nil {
		return nil, err
	}
	if m.Where != nil {
		innerPlan, err = t.translateExistsPredicate(m.Where.Predicate, innerPlan)
		if err != nil {
			return nil, err
		}
	}
	return NewOptionalApplyWithTag(child, innerPlan, optTag), nil
}

// optionalInnerPattern builds the inner subplan of an OPTIONAL MATCH when an
// outer driving subplan (child) is present. The leading node of the FIRST
// path whose variable is bound by child is replaced by an Argument leaf
// carrying outerArgTag — this leaf is the seam through which the surrounding
// OptionalApply injects the current outer row. Subsequent paths within the
// OPTIONAL MATCH use CorrelatedApply with freshly issued tags for further
// shared-variable joins, or plain Apply for Cartesian products.
//
// Relationships are built with Expand (NOT OptionalExpand) because the outer
// OptionalApply already provides the full-pattern NULL emission semantics.
//
// The returned plan is the inner subtree only; child is NOT included. The
// caller must wrap it with NewOptionalApplyWithTag(child, plan, outerArgTag).
func (t *translator) optionalInnerPattern(pat *ast.Pattern, child LogicalPlan, outerArgTag uint32) (LogicalPlan, error) {
	if pat == nil || len(pat.Paths) == 0 {
		// No paths — the inner subtree is just the Argument leaf so the outer
		// OptionalApply has something to drive (one inner row per outer row).
		return NewArgumentWithTag(childVarSlice(child), outerArgTag), nil
	}

	// Collect outer-scope variables that the OPTIONAL MATCH may correlate to.
	outerVars := map[string]struct{}{}
	if child != nil {
		for _, v := range child.Vars() {
			outerVars[v] = struct{}{}
		}
	}

	// Track variables bound by paths already translated within the OPTIONAL
	// MATCH so that subsequent paths can also use shared-variable joins.
	innerBound := map[string]struct{}{}
	for k := range outerVars {
		innerBound[k] = struct{}{}
	}

	// The first shared-leaf Argument in the inner subtree carries outerArgTag.
	// All later shared-leaf Arguments use fresh tags routed through nested
	// CorrelatedApply nodes.
	outerArgConsumed := false

	var plan LogicalPlan

	for _, pp := range pat.Paths {
		leadVar := leadingNodeVar(pp)
		_, sharedWithOuter := outerVars[leadVar]
		_, sharedWithInner := innerBound[leadVar]

		if plan == nil {
			// First path of the OPTIONAL MATCH.
			if sharedWithOuter && !outerArgConsumed {
				// The leading variable is bound by the outer (child) scope; use
				// outerArgTag as the Argument leaf's tag so OptionalApply seeds
				// the outer row into it.
				outerArgConsumed = true
				p, err := t.matchPathPatternWithArg(pp, false, leadVar, outerArgTag, copyVarSet(innerBound))
				if err != nil {
					return nil, err
				}
				plan = p
			} else {
				p, err := t.matchPathPattern(pp, false, "", copyVarSet(innerBound))
				if err != nil {
					return nil, err
				}
				// If the first path has no shared variable with the outer, the
				// inner subtree must still consume outerArgTag for the
				// OptionalApply to function. Wrap the path with a
				// CorrelatedApply over an Argument leaf so the row stream
				// remains correlated. This corresponds to OPTIONAL MATCH whose
				// pattern is an independent scan — a Cartesian product per
				// outer row.
				outerArgConsumed = true
				outerLeaf := NewArgumentWithTag(childVarSlice(child), outerArgTag)
				plan = NewCorrelatedApplyWithTag(outerLeaf, p, nextArgTag()) //nolint:staticcheck // inner-tag is unused by p (no Argument referencing it)
				// The inner p has no Argument node, so the CorrelatedApply's
				// inner-tag is effectively a no-op. The OptionalApply still
				// drives outerLeaf via outerArgTag.
				_ = outerLeaf
			}
			for _, v := range pathPatternVars(pp) {
				innerBound[v] = struct{}{}
			}
			continue
		}

		// Subsequent path within the OPTIONAL MATCH.
		if sharedWithInner {
			tag := nextArgTag()
			p, err := t.matchPathPatternWithArg(pp, false, leadVar, tag, copyVarSet(innerBound))
			if err != nil {
				return nil, err
			}
			plan = NewCorrelatedApplyWithTag(plan, p, tag)
		} else {
			p, err := t.matchPathPattern(pp, false, "", copyVarSet(innerBound))
			if err != nil {
				return nil, err
			}
			plan = NewApply(plan, p)
		}
		for _, v := range pathPatternVars(pp) {
			innerBound[v] = struct{}{}
		}
	}

	// Edge case: the inner subtree did NOT consume outerArgTag (e.g. the OPTIONAL
	// MATCH's leading path has no shared variable AND we never wrapped with the
	// Argument leaf). Wrap the whole plan in a CorrelatedApply over an
	// outerArgTag Argument so the OptionalApply has a seam to drive.
	if !outerArgConsumed {
		outerLeaf := NewArgumentWithTag(childVarSlice(child), outerArgTag)
		plan = NewCorrelatedApplyWithTag(outerLeaf, plan, nextArgTag())
	}

	return plan, nil
}

// copyVarSet returns a defensive copy of a variable set.
func copyVarSet(s map[string]struct{}) map[string]struct{} {
	cp := make(map[string]struct{}, len(s))
	for k := range s {
		cp[k] = struct{}{}
	}
	return cp
}

// childVarSlice returns child.Vars() as a fresh slice, or nil when child is nil.
func childVarSlice(child LogicalPlan) []string {
	if child == nil {
		return nil
	}
	vs := child.Vars()
	cp := make([]string, len(vs))
	copy(cp, vs)
	return cp
}

// matchPattern translates a MATCH Pattern (comma-separated path list) into a
// plan. Comma-separated patterns are joined on shared variables using
// CorrelatedApply when a shared variable is present; otherwise they are
// composed via Apply (Cartesian product).
//
// When child is non-nil (preceding reading clauses already produced a plan),
// child is used as the outer side of the first join, and the first path is
// itself eligible for the shared-variable rewrite against child.Vars().
func (t *translator) matchPattern(pat *ast.Pattern, child LogicalPlan, optional bool) (LogicalPlan, error) {
	if pat == nil || len(pat.Paths) == 0 {
		return child, nil
	}

	var plan LogicalPlan
	boundVars := map[string]struct{}{}
	if child != nil {
		plan = child
		for _, v := range child.Vars() {
			boundVars[v] = struct{}{}
		}
	}

	for _, pp := range pat.Paths {
		leadVar := leadingNodeVar(pp)
		// Detect whether this path's leading node variable is already bound by
		// the cumulative plan. When it is, build the path with an Argument leaf
		// re-emitting the outer row, and wrap with CorrelatedApply so the
		// shared variable acts as a join key.
		_, shared := boundVars[leadVar]

		if plan == nil {
			// First path with no preceding child: build as a standalone subtree.
			p, err := t.matchPathPattern(pp, optional, "", copyVarSet(boundVars))
			if err != nil {
				return nil, err
			}
			plan = p
			for _, v := range pathPatternVars(pp) {
				boundVars[v] = struct{}{}
			}
			continue
		}

		if shared {
			// Build the path's inner subtree with an Argument leaf carrying a
			// shared tag. The CorrelatedApply node uses the same tag so the
			// physical builder can route the same exec.Argument instance.
			tag := nextArgTag()
			innerPlan, err := t.matchPathPatternWithArg(pp, optional, leadVar, tag, copyVarSet(boundVars))
			if err != nil {
				return nil, err
			}
			plan = NewCorrelatedApplyWithTag(plan, innerPlan, tag)
		} else {
			// No shared variable — fall back to plain Apply (Cartesian product).
			p, err := t.matchPathPattern(pp, optional, "", copyVarSet(boundVars))
			if err != nil {
				return nil, err
			}
			plan = NewApply(plan, p)
		}
		for _, v := range pathPatternVars(pp) {
			boundVars[v] = struct{}{}
		}
	}
	return plan, nil
}

// leadingNodeVar returns the variable name of the path's leading node, or ""
// when the leading node is anonymous.
func leadingNodeVar(pp *ast.PathPattern) string {
	if pp == nil || pp.Head == nil || pp.Head.Node == nil || pp.Head.Node.Variable == nil {
		return ""
	}
	return *pp.Head.Node.Variable
}

// pathPatternVars collects all named node and relationship variables appearing
// in pp, including the optional path-binding variable. Anonymous nodes and
// relationships are skipped.
func pathPatternVars(pp *ast.PathPattern) []string {
	if pp == nil {
		return nil
	}
	var out []string
	if pp.Variable != nil {
		out = append(out, *pp.Variable)
	}
	el := pp.Head
	for el != nil {
		if el.Node != nil && el.Node.Variable != nil {
			out = append(out, *el.Node.Variable)
		}
		if el.Relationship != nil && el.Relationship.Variable != nil {
			out = append(out, *el.Relationship.Variable)
		}
		el = el.Next
	}
	return out
}

// matchPathPattern translates a single PathPattern into a scan/expand subtree.
// When sharedLeadVar is empty, the leading node is built as a fresh scan; when
// non-empty, the leading node must be handled by the caller (see
// matchPathPatternWithArg).
//
// boundVars is the set of variables already in scope (from outer correlation
// or earlier paths within the same MATCH). When an Expand step targets a
// destination variable in boundVars, the step is wrapped in a Selection that
// equates the freshly bound value with the previously bound one — turning the
// destination into an implicit join key rather than a redefinition.
func (t *translator) matchPathPattern(pp *ast.PathPattern, optional bool, sharedLeadVar string, boundVars map[string]struct{}) (LogicalPlan, error) {
	if pp == nil || pp.Head == nil {
		return nil, nil
	}
	if boundVars == nil {
		boundVars = map[string]struct{}{}
	}

	el := pp.Head

	// The first element is always a node; translate it as a scan unless the
	// caller has indicated this leading variable is shared with outer scope.
	var plan LogicalPlan
	prevNodeVar := ""
	if el.Node != nil {
		if sharedLeadVar != "" && el.Node.Variable != nil && *el.Node.Variable == sharedLeadVar {
			// Caller is expected to use matchPathPatternWithArg for this case.
			return nil, fmt.Errorf("ir: matchPathPattern called with sharedLeadVar but the leading node is not a plain scan; use matchPathPatternWithArg")
		}
		plan = t.matchNodeScan(el.Node)
		if el.Node.Variable != nil {
			boundVars[*el.Node.Variable] = struct{}{}
			prevNodeVar = *el.Node.Variable
		}
	}

	// Walk remaining (rel, node) pairs. prevNodeVar threads the previous node's
	// variable name into the next Expand step so its FromVar is correct.
	el = el.Next
	for el != nil {
		if el.Relationship != nil && el.Node != nil {
			plan = t.matchExpandStepBoundWithFrom(el.Relationship, el.Node, plan, optional, boundVars, prevNodeVar)
			if el.Node.Variable != nil {
				prevNodeVar = *el.Node.Variable
				boundVars[*el.Node.Variable] = struct{}{}
			} else {
				prevNodeVar = ""
			}
			if el.Relationship.Variable != nil {
				boundVars[*el.Relationship.Variable] = struct{}{}
			}
		}
		el = el.Next
	}
	return plan, nil
}

// matchPathPatternWithArg translates a single PathPattern whose leading node
// is already bound (its variable appears in an outer scope). It places an
// [Argument] leaf at the start of the subtree so the inner pipeline observes
// the outer row, then layers Expand operators on top using sharedLeadVar as
// the source. The Argument's Tag equals argTag so the physical builder routes
// the matching exec.Argument instance from the enclosing CorrelatedApply or
// OptionalApply.
//
// Inline label/property predicates on the leading node still need enforcing,
// since the outer row only guarantees a value of the right NodeID — not a
// matching label or property. They are emitted as Selection operators on top
// of the Argument leaf.
func (t *translator) matchPathPatternWithArg(pp *ast.PathPattern, optional bool, sharedLeadVar string, argTag uint32, boundVars map[string]struct{}) (LogicalPlan, error) {
	if pp == nil || pp.Head == nil {
		return nil, nil
	}
	if boundVars == nil {
		boundVars = map[string]struct{}{}
	}
	el := pp.Head
	if el.Node == nil || el.Node.Variable == nil || *el.Node.Variable != sharedLeadVar {
		return nil, fmt.Errorf("ir: matchPathPatternWithArg called with mismatched shared variable")
	}

	var plan LogicalPlan = NewArgumentWithTag([]string{sharedLeadVar}, argTag)
	boundVars[sharedLeadVar] = struct{}{}
	prevNodeVar := sharedLeadVar

	// Enforce any label/property constraints declared on the leading node.
	for _, lbl := range el.Node.Labels {
		pred := fmt.Sprintf("%s:%s", sharedLeadVar, lbl)
		plan = NewSelection(pred, plan)
	}
	if el.Node.Properties != nil {
		plan = buildPropertySelection(sharedLeadVar, el.Node.Properties, plan)
	}

	// Walk remaining (rel, node) pairs. prevNodeVar threads the previous node's
	// variable name into the next Expand step so its FromVar is correct.
	el = el.Next
	for el != nil {
		if el.Relationship != nil && el.Node != nil {
			plan = t.matchExpandStepBoundWithFrom(el.Relationship, el.Node, plan, optional, boundVars, prevNodeVar)
			if el.Node.Variable != nil {
				prevNodeVar = *el.Node.Variable
				boundVars[*el.Node.Variable] = struct{}{}
			} else {
				prevNodeVar = ""
			}
			if el.Relationship.Variable != nil {
				boundVars[*el.Relationship.Variable] = struct{}{}
			}
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
			return buildPropertySelection(nodeVar, np.Properties, scan)
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
		plan = buildPropertySelection(nodeVar, np.Properties, plan)
	}

	return plan
}

// matchExpandStep translates a single (rel, node) hop into Expand or
// OptionalExpand, then wraps destination-node label/property constraints as
// Selection operators immediately above the expand. This variant assumes the
// destination variable is fresh (not already bound).
func (t *translator) matchExpandStep(rp *ast.RelationshipPattern, to *ast.NodePattern, child LogicalPlan, optional bool) LogicalPlan {
	return t.matchExpandStepBound(rp, to, child, optional, nil)
}

// matchExpandStepBound is the binding-aware variant of [matchExpandStep]. When
// the destination variable is already in boundVars, the step expands into a
// synthetic anonymous variable and adds a Selection equating the original
// (already-bound) variable with the synthetic one — implementing the implicit
// equi-join semantics of repeated variable bindings.
//
// Deprecated: prefer matchExpandStepBoundWithFrom which threads the previous
// node's variable name explicitly. This shim is retained only for callers
// that have no convenient way to track the previous node variable.
func (t *translator) matchExpandStepBound(rp *ast.RelationshipPattern, to *ast.NodePattern, child LogicalPlan, optional bool, boundVars map[string]struct{}) LogicalPlan {
	return t.matchExpandStepBoundWithFrom(rp, to, child, optional, boundVars, firstVar(child))
}

// matchExpandStepBoundWithFrom is the canonical Expand-translation helper. It
// takes fromVar explicitly because relying on firstVar(child) is unreliable
// once child is itself an Expand (whose Vars() lead with the RelVar, often
// the empty string for anonymous relationships).
func (t *translator) matchExpandStepBoundWithFrom(rp *ast.RelationshipPattern, to *ast.NodePattern, child LogicalPlan, optional bool, boundVars map[string]struct{}, fromVar string) LogicalPlan {

	relVar := ""
	if rp.Variable != nil {
		relVar = *rp.Variable
	}

	toVar := ""
	if to.Variable != nil {
		toVar = *to.Variable
	}

	// Detect destination rebinding: if toVar is already bound, expand into a
	// synthetic variable and equate it with the existing toVar via Selection.
	destRebinding := false
	syntheticTo := ""
	if toVar != "" && boundVars != nil {
		if _, ok := boundVars[toVar]; ok {
			destRebinding = true
			syntheticTo = t.freshAnonVar() + "_to_" + toVar
		}
	}

	dir := relDirection(rp.Direction)
	relTypes := make([]string, len(rp.Types))
	copy(relTypes, rp.Types)

	expandTo := toVar
	if destRebinding {
		expandTo = syntheticTo
	}

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
		var plan LogicalPlan = NewVarLengthExpand(fromVar, relVar, relTypes, dir, expandTo, minDepth, maxDepth, child)
		plan = t.matchApplyNodeFilter(to, expandTo, plan)
		if destRebinding {
			plan = t.appendEqSelection(toVar, syntheticTo, plan)
		}
		return plan
	}

	var plan LogicalPlan
	if optional {
		plan = NewOptionalExpand(fromVar, relVar, relTypes, dir, expandTo, child)
	} else {
		plan = NewExpand(fromVar, relVar, relTypes, dir, expandTo, child)
	}

	plan = t.matchApplyNodeFilter(to, expandTo, plan)
	if destRebinding {
		plan = t.appendEqSelection(toVar, syntheticTo, plan)
	}
	return plan
}

// appendEqSelection wraps plan in a Selection comparing two variables for
// equality. It is used to enforce the "destination already bound" join
// semantics in matchExpandStepBound: after expanding into a synthetic variable
// the row stream is filtered to keep only rows where the synthetic value
// equals the previously bound value.
func (t *translator) appendEqSelection(boundVar, syntheticVar string, plan LogicalPlan) LogicalPlan {
	eq := &ast.BinaryOp{
		Operator: "=",
		Left:     &ast.Variable{Name: boundVar},
		Right:    &ast.Variable{Name: syntheticVar},
	}
	return NewSelectionExpr(eq.String(), eq, plan)
}

// matchApplyNodeFilter wraps plan with Selection operators for label and
// property constraints declared on the destination node pattern.
func (t *translator) matchApplyNodeFilter(np *ast.NodePattern, nodeVar string, plan LogicalPlan) LogicalPlan {
	for _, lbl := range np.Labels {
		pred := fmt.Sprintf("%s:%s", nodeVar, lbl)
		plan = NewSelection(pred, plan)
	}
	if np.Properties != nil {
		plan = buildPropertySelection(nodeVar, np.Properties, plan)
	}
	return plan
}
