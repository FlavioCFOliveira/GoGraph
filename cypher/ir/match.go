package ir

import (
	"fmt"
	"math"

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
		// Pattern has no relationships (just a bare node like
		// `OPTIONAL MATCH (n:DoesNotExist)`): NodeScan returns zero rows
		// when no node matches, which would violate the openCypher
		// guarantee that OPTIONAL MATCH always emits at least one row
		// (NULL-extended) per driving outer row. With child==nil we
		// synthesise a SingleRow seed via an Argument leaf and wrap the
		// inner pattern in an OptionalApply, so the empty-result case
		// produces a single NULL-extended row.
		// Node-only OPTIONAL MATCH (`OPTIONAL MATCH (n)`): NodeScan
		// returns zero rows when no node matches, but openCypher 9
		// §3.2.4 requires at least one NULL-extended row. Wrap the
		// standalone pattern in an OptionalApply over an empty
		// Argument seed so the inner subtree's empty result becomes a
		// single NULL row.
		// Both node-only and relationship-bearing OPTIONAL MATCH at the
		// start of a query need an OptionalApply wrapper so an empty
		// pattern result still emits one NULL-extended row, per
		// openCypher 9 §3.2.4. The inner pattern itself uses regular
		// Expand (optional=false) because the OptionalApply now provides
		// the full-pattern NULL emission semantics; previously the
		// relationship-bearing branch returned the inner plan
		// unwrapped, so `OPTIONAL MATCH ()-[r]->()` on an empty graph
		// returned zero rows instead of one null row.
		inner, err := t.matchPattern(m.Pattern, nil, false)
		if err != nil {
			return nil, err
		}
		if m.Where != nil {
			inner, err = t.translateExistsPredicate(m.Where.Predicate, inner)
			if err != nil {
				return nil, err
			}
		}
		optTag := nextArgTag()
		seed := NewArgumentWithTag(nil, optTag)
		return NewOptionalApplyWithTag(seed, inner, optTag), nil
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

	ctx := newOptionalInnerCtx(child)

	var plan LogicalPlan
	for _, pp := range pat.Paths {
		var err error
		plan, err = t.appendOptionalInnerPath(plan, pp, child, outerArgTag, ctx)
		if err != nil {
			return nil, err
		}
	}

	// Edge case: the inner subtree did NOT consume outerArgTag (e.g. the OPTIONAL
	// MATCH's leading path has no shared variable AND we never wrapped with the
	// Argument leaf). Wrap the whole plan in a CorrelatedApply over an
	// outerArgTag Argument so the OptionalApply has a seam to drive.
	if !ctx.outerArgConsumed {
		outerLeaf := NewArgumentWithTag(childVarSlice(child), outerArgTag)
		plan = NewCorrelatedApplyWithTag(outerLeaf, plan, nextArgTag())
	}

	return plan, nil
}

// optionalInnerCtx threads the mutable bookkeeping that
// optionalInnerPattern accumulates while walking pat.Paths. It is local
// to the translator and never escapes.
type optionalInnerCtx struct {
	outerVars        map[string]struct{}
	innerBound       map[string]struct{}
	outerArgConsumed bool
}

// newOptionalInnerCtx seeds the bookkeeping with the variables bound by
// the outer (child) scope; innerBound starts identical to outerVars
// because any name visible to the outer is also visible to the first
// inner path.
func newOptionalInnerCtx(child LogicalPlan) *optionalInnerCtx {
	outerVars := map[string]struct{}{}
	if child != nil {
		for _, v := range collectAllVars(child) {
			outerVars[v] = struct{}{}
		}
	}
	innerBound := make(map[string]struct{}, len(outerVars))
	for k := range outerVars {
		innerBound[k] = struct{}{}
	}
	return &optionalInnerCtx{outerVars: outerVars, innerBound: innerBound}
}

// appendOptionalInnerPath translates one pp from the OPTIONAL MATCH and
// fuses it into the running plan. The dispatch over first-vs-subsequent
// and shared-with-outer-vs-inner now lives in dedicated helpers.
func (t *translator) appendOptionalInnerPath(
	plan LogicalPlan,
	pp *ast.PathPattern,
	child LogicalPlan,
	outerArgTag uint32,
	ctx *optionalInnerCtx,
) (LogicalPlan, error) {
	leadVar := leadingNodeVar(pp)
	_, sharedWithOuter := ctx.outerVars[leadVar]
	_, sharedWithInner := ctx.innerBound[leadVar]

	if plan == nil {
		out, err := t.firstOptionalPath(pp, child, outerArgTag, leadVar, sharedWithOuter, ctx)
		if err != nil {
			return nil, err
		}
		plan = out
	} else {
		out, err := t.subsequentOptionalPath(plan, pp, leadVar, sharedWithInner, ctx)
		if err != nil {
			return nil, err
		}
		plan = out
	}

	for _, v := range pathPatternVars(pp) {
		ctx.innerBound[v] = struct{}{}
	}
	return plan, nil
}

// firstOptionalPath handles the first path of the OPTIONAL MATCH.
// When the leading variable is bound by the outer scope, the
// Argument leaf carries outerArgTag directly; otherwise a
// CorrelatedApply over an outerArgTag Argument leaf wraps the path so
// the surrounding OptionalApply still has a seam to drive.
func (t *translator) firstOptionalPath(
	pp *ast.PathPattern,
	child LogicalPlan,
	outerArgTag uint32,
	leadVar string,
	sharedWithOuter bool,
	ctx *optionalInnerCtx,
) (LogicalPlan, error) {
	if sharedWithOuter && !ctx.outerArgConsumed {
		ctx.outerArgConsumed = true
		return t.matchPathPatternWithArg(pp, false, leadVar, outerArgTag, copyVarSet(ctx.innerBound))
	}
	p, err := t.matchPathPattern(pp, false, "", copyVarSet(ctx.innerBound))
	if err != nil {
		return nil, err
	}
	// destRebinding equality selections inside p reference outer-bound
	// variables (e.g. synthetic == b where b is bound by the surrounding
	// MATCH). Inside the inner pipeline those references resolve against
	// inner-only rows that lack outer columns — the equality always
	// returns NULL and the inner emits zero rows. Hoist these Selections
	// OUT of p and re-apply them ON TOP of the plain-Apply concat, where
	// the combined row carries both outer and inner columns and the
	// equality can resolve.
	outerVars := ctx.outerVars
	body, hoisted := peelOuterDestRebinding(p, outerVars)
	// The first path has no shared variable with the outer. Wrap the body
	// with a plain Apply over an Argument leaf carrying the outer vars.
	ctx.outerArgConsumed = true
	argLeaf := NewArgumentWithTag(childVarSlice(child), outerArgTag)
	var plan LogicalPlan = NewApply(argLeaf, body)
	// Re-apply the hoisted Selections on top of the combined row.
	for _, sel := range hoisted {
		plan = NewSelectionExpr(sel.Predicate, sel.PredicateExpr, plan)
	}
	return plan, nil
}

// peelOuterDestRebinding strips Selection nodes from the top of p whose
// PredicateExpr is an equality comparing an inner-only synthetic variable
// with an outer-bound variable (the canonical destRebinding equality).
// The returned body is p with those Selections removed; hoisted carries
// the removed nodes in top-to-bottom order so the caller can re-apply
// them on the outer plan.
//
// The walker stops at the first non-Selection (or non-matching-Selection)
// node so destRebinding equalities deeper in the tree — added at an
// inner Expand step — are not mis-peeled.
func peelOuterDestRebinding(p LogicalPlan, outerVars map[string]struct{}) (LogicalPlan, []*Selection) {
	var hoisted []*Selection
	for {
		sel, ok := p.(*Selection)
		if !ok || sel.PredicateExpr == nil {
			break
		}
		bin, ok := sel.PredicateExpr.(*ast.BinaryOp)
		if !ok || bin.Operator != "=" {
			break
		}
		if !referencesOuterVar(bin.Left, outerVars) && !referencesOuterVar(bin.Right, outerVars) {
			break
		}
		hoisted = append(hoisted, sel)
		p = sel.Child
	}
	return p, hoisted
}

// referencesOuterVar reports whether e is a Variable whose name is in
// outerVars. The check is intentionally narrow: only the immediate
// Variable form qualifies, mirroring the shape emitted by
// matchExpandStepBoundWithFrom when destRebinding fires.
func referencesOuterVar(e ast.Expression, outerVars map[string]struct{}) bool {
	v, ok := e.(*ast.Variable)
	if !ok {
		return false
	}
	_, isOuter := outerVars[v.Name]
	return isOuter
}

// subsequentOptionalPath handles the n-th (n>0) path of the OPTIONAL
// MATCH. A path that shares its leading variable with the inner-bound
// set joins via CorrelatedApply with a fresh tag; an independent path
// joins via a plain Apply (Cartesian product).
func (t *translator) subsequentOptionalPath(
	plan LogicalPlan,
	pp *ast.PathPattern,
	leadVar string,
	sharedWithInner bool,
	ctx *optionalInnerCtx,
) (LogicalPlan, error) {
	if sharedWithInner {
		tag := nextArgTag()
		p, err := t.matchPathPatternWithArg(pp, false, leadVar, tag, copyVarSet(ctx.innerBound))
		if err != nil {
			return nil, err
		}
		return NewCorrelatedApplyWithTag(plan, p, tag), nil
	}
	p, err := t.matchPathPattern(pp, false, "", copyVarSet(ctx.innerBound))
	if err != nil {
		return nil, err
	}
	return NewApply(plan, p), nil
}

// copyVarSet returns a defensive copy of a variable set.
func copyVarSet(s map[string]struct{}) map[string]struct{} {
	cp := make(map[string]struct{}, len(s))
	for k := range s {
		cp[k] = struct{}{}
	}
	return cp
}

// childVarSlice returns the cumulative set of variables introduced by child
// and every plan in its transitive subtree. Returns nil when child is nil.
//
// Several IR operators (Expand, OptionalExpand, VarLengthExpand) report only
// the variables they themselves introduce in [LogicalPlan.Vars], not their
// child's vars — so a non-recursive `child.Vars()` misses leading-bound
// variables (e.g. the node bound by the lowest NodeByLabelScan beneath a
// chain of Expands). [collectAllVars] descends the whole subtree, deduping.
func childVarSlice(child LogicalPlan) []string {
	if child == nil {
		return nil
	}
	return collectAllVars(child)
}

// collectAllVars returns the deduplicated union of [LogicalPlan.Vars] across
// plan and every plan in its transitive [LogicalPlan.Children] subtree.
//
// This is the correct seed for "what does the outer scope bind" reasoning
// (shared-variable detection in OPTIONAL MATCH and multi-pattern MATCH).
// Order preserves first-seen position to keep diagnostic output stable.
func collectAllVars(plan LogicalPlan) []string {
	if plan == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	var walk func(p LogicalPlan)
	walk = func(p LogicalPlan) {
		if p == nil {
			return
		}
		for _, v := range p.Vars() {
			if v == "" {
				continue
			}
			if _, ok := seen[v]; !ok {
				seen[v] = struct{}{}
				out = append(out, v)
			}
		}
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return out
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
		for _, v := range collectAllVars(child) {
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

		// Pure bound-rel pass-through: when every relationship and named
		// node in the path is already in scope (forwarded by an upstream
		// MATCH/WITH) and no inline filter narrows the match further, the
		// path needs no Scan/Expand — the upstream row already carries
		// every binding the pattern names.
		if isPureBoundRelPath(pp, outputVarSet(plan)) {
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
			// destRebinding equality selections inside p that reference
			// outer-bound variables would, after the fresh-schema isolation
			// applied by buildOperator's plain-Apply branch, fail to
			// resolve the outer reference (the inner row has only inner
			// columns). Hoist those Selections OUT of p and re-apply them
			// on top of the Apply, where the combined outer||inner row
			// makes the outer reference reachable.
			p, err := t.matchPathPattern(pp, optional, "", copyVarSet(boundVars))
			if err != nil {
				return nil, err
			}
			body, hoisted := peelOuterDestRebinding(p, boundVars)
			var combined LogicalPlan = NewApply(plan, body)
			for _, sel := range hoisted {
				combined = NewSelectionExpr(sel.Predicate, sel.PredicateExpr, combined)
			}
			plan = combined
		}
		for _, v := range pathPatternVars(pp) {
			boundVars[v] = struct{}{}
		}
	}
	return plan, nil
}

// setPathVarOnVLE walks the plan tree depth-first and sets PathVar on the first
// VarLengthExpand node found, so that the physical builder can allocate a
// schema slot and emit a PathValue. Only the first VarLengthExpand is tagged
// (one path variable per pattern).
func setPathVarOnVLE(plan LogicalPlan, pathVar string) {
	if plan == nil {
		return
	}
	if vle, ok := plan.(*VarLengthExpand); ok {
		if vle.PathVar == "" {
			vle.PathVar = pathVar
		}
		return
	}
	for _, child := range plan.Children() {
		setPathVarOnVLE(child, pathVar)
	}
}

// pathHasVarLength reports whether pp contains at least one variable-length
// relationship pattern (e.g. -[r*1..3]->). When true, the legacy VLE
// path-var pipeline is used for the named path; otherwise the new
// [NamedPath] operator is wrapped above the plan so the physical builder
// can reconstruct a PathValue from the alternating Expand triplets.
func pathHasVarLength(pp *ast.PathPattern) bool {
	if pp == nil {
		return false
	}
	for el := pp.Head; el != nil; el = el.Next {
		if el.Relationship != nil && el.Relationship.Range != nil {
			return true
		}
	}
	return false
}

// buildPathChain extracts the alternating node/rel description of pp into a
// canonical PathChainElement slice suitable for [NamedPath]. The first entry
// is the leading node (IsLeading=true); each subsequent entry describes a
// (relationship, destination-node) step in document order.
func buildPathChain(pp *ast.PathPattern) []PathChainElement {
	if pp == nil || pp.Head == nil {
		return nil
	}
	chain := make([]PathChainElement, 0, 4)
	head := pp.Head
	leadVar := ""
	if head.Node != nil && head.Node.Variable != nil {
		leadVar = *head.Node.Variable
	}
	chain = append(chain, PathChainElement{NodeVar: leadVar, IsLeading: true})
	for el := head.Next; el != nil; el = el.Next {
		if el.Relationship == nil || el.Node == nil {
			continue
		}
		nodeVar := ""
		if el.Node.Variable != nil {
			nodeVar = *el.Node.Variable
		}
		relVar := ""
		if el.Relationship.Variable != nil {
			relVar = *el.Relationship.Variable
		}
		relTypes := make([]string, len(el.Relationship.Types))
		copy(relTypes, el.Relationship.Types)
		chain = append(chain, PathChainElement{
			NodeVar:   nodeVar,
			RelVar:    relVar,
			RelTypes:  relTypes,
			Direction: relDirection(el.Relationship.Direction),
		})
	}
	return chain
}

// applyPathVar tags the resulting plan with the named-path variable. When pp
// contains a variable-length expansion the legacy VLE-tagging is used (the
// physical builder reconstructs a PathValue from the flat alternating list
// emitted by VarLengthExpand). Otherwise plan is wrapped with a [NamedPath]
// operator carrying the explicit alternating chain so the physical builder
// can reconstruct a PathValue from the (srcID, edgeID, dstID) triplets
// emitted by each fixed-length Expand step.
func applyPathVar(pp *ast.PathPattern, plan LogicalPlan) LogicalPlan {
	if pp == nil || pp.Variable == nil || plan == nil {
		return plan
	}
	if pathHasVarLength(pp) {
		setPathVarOnVLE(plan, *pp.Variable)
		return plan
	}
	chain := buildPathChain(pp)
	return NewNamedPath(*pp.Variable, chain, plan)
}

// leadingNodeVar returns the variable name of the path's leading node, or ""
// when the leading node is anonymous.
func leadingNodeVar(pp *ast.PathPattern) string {
	if pp == nil || pp.Head == nil || pp.Head.Node == nil || pp.Head.Node.Variable == nil {
		return ""
	}
	return *pp.Head.Node.Variable
}

// isPureBoundRelPath reports whether every relationship in pp is already in
// outputVars (so it was forwarded by an upstream WITH/MATCH) and every node
// in the pattern is anonymous-with-no-filter. Such a path adds no new
// constraints: the upstream row already binds the rel, and the anonymous
// endpoints are trivially satisfied by the rel's actual start/end.
//
// Bound endpoints are deliberately rejected — `MATCH (a)-[r]->(b)` with
// every variable in scope LOOKS like a no-op but in practice an
// upstream aggregating WITH does not always preserve the
// RelationshipValue at the column the projection later reads from, so
// the second MATCH's Expand is the channel that re-resolves r's
// metadata against the live graph. Skipping in that case surfaces the
// upstream's NULL.
//
// outputVars is restricted to the names exposed by the child plan's
// Vars() (post-Projection) rather than the cumulative boundVars set,
// because boundVars also contains stale pre-projection names. Without
// this restriction `WITH r AS r2 MATCH ()-[r2]->()` could skip the
// second MATCH when r2 is shadowed by an earlier scope.
//
// The pattern is rejected when:
//   - the path carries a path-binding variable (`p = (...)`),
//   - any relationship is unnamed, in a variable-length quantifier, or
//     carries an inline type/property predicate,
//   - any node has inline labels, properties, or a non-anonymous
//     variable.
func isPureBoundRelPath(pp *ast.PathPattern, outputVars map[string]struct{}) bool {
	if pp == nil || pp.Head == nil {
		return false
	}
	if pp.Variable != nil {
		return false
	}
	sawBoundRel := false
	for el := pp.Head; el != nil; el = el.Next {
		if el.Node != nil {
			if len(el.Node.Labels) > 0 || el.Node.Properties != nil {
				return false
			}
			if el.Node.Variable != nil {
				return false
			}
		}
		if el.Relationship != nil {
			if el.Relationship.Variable == nil {
				return false
			}
			if _, ok := outputVars[*el.Relationship.Variable]; !ok {
				return false
			}
			if el.Relationship.Range != nil {
				return false
			}
			if len(el.Relationship.Types) > 0 || el.Relationship.Properties != nil {
				return false
			}
			sawBoundRel = true
		}
	}
	return sawBoundRel
}

// outputVarSet returns the variables exposed by plan as a set.
func outputVarSet(plan LogicalPlan) map[string]struct{} {
	if plan == nil {
		return nil
	}
	vs := plan.Vars()
	out := make(map[string]struct{}, len(vs))
	for _, v := range vs {
		if v != "" {
			out[v] = struct{}{}
		}
	}
	return out
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
	// Anonymous nodes are assigned a synthetic IR-only variable so a
	// subsequent hop can reference the destination column emitted by the
	// preceding Expand (without a name, fromVar="" forces the next step to
	// scan from an empty schema key and the chain breaks — see Match3 [17]).
	// Anonymous relationships likewise get a synthetic IR-only variable so
	// the relationship-isomorphism (cyphermorphism) guard can refer to their
	// edge column by name when later hops in the same pattern need to
	// exclude already-traversed edges.
	siblingRels := []string{}
	el = el.Next
	for el != nil {
		if el.Relationship != nil && el.Node != nil {
			if el.Node.Variable == nil {
				synth := t.freshAnonVar()
				el.Node.Variable = &synth
			}
			if el.Relationship.Variable == nil {
				synth := t.freshAnonVar()
				el.Relationship.Variable = &synth
			}
			plan = t.matchExpandStepBoundWithFromSiblings(el.Relationship, el.Node, plan, optional, boundVars, prevNodeVar, siblingRels)
			prevNodeVar = *el.Node.Variable
			boundVars[*el.Node.Variable] = struct{}{}
			boundVars[*el.Relationship.Variable] = struct{}{}
			siblingRels = append(siblingRels, *el.Relationship.Variable)
		}
		el = el.Next
	}
	plan = applyPathVar(pp, plan)
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
	// Wrap label predicates in a typed AST node so the physical builder
	// evaluates them via expr.Eval — opaque string predicates fall through
	// to the always-true filter.
	if len(el.Node.Labels) > 0 {
		labels := make([]string, len(el.Node.Labels))
		copy(labels, el.Node.Labels)
		lp := &ast.LabelPredicate{
			Receiver: &ast.Variable{Name: sharedLeadVar},
			Labels:   labels,
		}
		plan = NewSelectionExpr(lp.String(), lp, plan)
	}
	if el.Node.Properties != nil {
		plan = buildPropertySelection(sharedLeadVar, el.Node.Properties, plan)
	}

	// Walk remaining (rel, node) pairs. prevNodeVar threads the previous node's
	// variable name into the next Expand step so its FromVar is correct.
	// Anonymous nodes are assigned a synthetic IR-only variable so a
	// subsequent hop can reference the destination column emitted by the
	// preceding Expand (without a name, fromVar="" forces the next step to
	// scan from an empty schema key and the chain breaks — see Match3 [17]).
	siblingRels := []string{}
	el = el.Next
	for el != nil {
		if el.Relationship != nil && el.Node != nil {
			if el.Node.Variable == nil {
				synth := t.freshAnonVar()
				el.Node.Variable = &synth
			}
			if el.Relationship.Variable == nil {
				synth := t.freshAnonVar()
				el.Relationship.Variable = &synth
			}
			plan = t.matchExpandStepBoundWithFromSiblings(el.Relationship, el.Node, plan, optional, boundVars, prevNodeVar, siblingRels)
			prevNodeVar = *el.Node.Variable
			boundVars[*el.Node.Variable] = struct{}{}
			boundVars[*el.Relationship.Variable] = struct{}{}
			siblingRels = append(siblingRels, *el.Relationship.Variable)
		}
		el = el.Next
	}
	plan = applyPathVar(pp, plan)
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

	// Extra labels: AND-filter via a typed LabelPredicate so the physical
	// builder routes them through the expression evaluator. Opaque-string
	// Selection nodes are evaluated as always-true.
	if len(np.Labels) > 1 {
		extras := make([]string, len(np.Labels)-1)
		copy(extras, np.Labels[1:])
		lp := &ast.LabelPredicate{
			Receiver: &ast.Variable{Name: nodeVar},
			Labels:   extras,
		}
		plan = NewSelectionExpr(lp.String(), lp, plan)
	}

	// Inline property predicates sit above label selections, still below any
	// WHERE predicate — the lowest legal position.
	if np.Properties != nil {
		plan = buildPropertySelection(nodeVar, np.Properties, plan)
	}

	return plan
}

// matchExpandStepBoundWithFromSiblings is the variant of
// matchExpandStepBoundWithFrom that also threads the list of sibling
// relationship variables (already bound by earlier hops in the same
// pattern) into the resulting Expand's SiblingRelVars field — the
// physical builder uses that list to enforce relationship-isomorphism
// (cyphermorphism) per openCypher 9 §3.2.2.
func (t *translator) matchExpandStepBoundWithFromSiblings(rp *ast.RelationshipPattern, to *ast.NodePattern, child LogicalPlan, optional bool, boundVars map[string]struct{}, fromVar string, siblings []string) LogicalPlan {
	plan := t.matchExpandStepBoundWithFrom(rp, to, child, optional, boundVars, fromVar)
	// Walk down through any Selection wrappers (label / property / equality
	// filters added by matchExpandStepBoundWithFrom) to find the underlying
	// Expand and stamp the sibling list on it.
	stampSiblings(plan, siblings)
	return plan
}

// stampSiblings walks a plan subtree looking for the topmost Expand and
// stamps the given sibling relationship variable list on it. Selections
// above the Expand are descended into; everything else stops the walk.
func stampSiblings(plan LogicalPlan, siblings []string) {
	for plan != nil {
		switch p := plan.(type) {
		case *Expand:
			if len(siblings) > 0 {
				cp := make([]string, len(siblings))
				copy(cp, siblings)
				p.SiblingRelVars = cp
			}
			return
		case *Selection:
			plan = p.Child
		default:
			return
		}
	}
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
		// Defaults match openCypher 9 §3.2.3.2: omitted lower bound is 1,
		// omitted upper bound is "unbounded". The IR encodes unbounded as
		// math.MaxInt so MaxDepth==0 unambiguously means "exactly 0 hops"
		// (which is a legal — though degenerate — quantifier).
		minDepth := 1
		maxDepth := math.MaxInt
		if rp.Range.Min != nil {
			minDepth = int(*rp.Range.Min)
		}
		if rp.Range.Max != nil {
			maxDepth = int(*rp.Range.Max)
		}
		var plan LogicalPlan = NewVarLengthExpand(fromVar, relVar, relTypes, dir, expandTo, minDepth, maxDepth, child)
		plan = t.matchApplyRelFilter(rp, relVar, plan)
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

	plan = t.matchApplyRelFilter(rp, relVar, plan)
	plan = t.matchApplyNodeFilter(to, expandTo, plan)
	if destRebinding {
		plan = t.appendEqSelection(toVar, syntheticTo, plan)
	}
	return plan
}

// matchApplyRelFilter wraps plan with Selection operators for property
// constraints declared inline on the relationship pattern (e.g. -[r:KNOWS
// {name: 'monkey'}]->). When the relationship variable is anonymous the
// caller in [matchPathPattern]/[matchPathPatternWithArg] has already
// assigned a synthetic name, so relVar is non-empty here whenever
// Properties is non-nil.
func (t *translator) matchApplyRelFilter(rp *ast.RelationshipPattern, relVar string, plan LogicalPlan) LogicalPlan {
	if rp.Properties == nil || relVar == "" {
		return plan
	}
	return buildPropertySelection(relVar, rp.Properties, plan)
}

// appendEqSelection wraps plan in a Selection comparing two variables for
// equality. It is used to enforce the "destination already bound" join
// semantics in matchExpandStepBoundWithFrom: after expanding into a synthetic
// variable the row stream is filtered to keep only rows where the synthetic
// value equals the previously bound value.
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
//
// Label predicates are expressed as a typed AST LabelPredicate so the
// physical builder evaluates them via expr.Eval rather than treating
// them as an opaque pass-through string (which would silently always
// match, defeating predicates like `OPTIONAL MATCH (n)-[r]-(m:NonExistent)`).
func (t *translator) matchApplyNodeFilter(np *ast.NodePattern, nodeVar string, plan LogicalPlan) LogicalPlan {
	if len(np.Labels) > 0 {
		labels := make([]string, len(np.Labels))
		copy(labels, np.Labels)
		lp := &ast.LabelPredicate{
			Receiver: &ast.Variable{Name: nodeVar},
			Labels:   labels,
		}
		plan = NewSelectionExpr(lp.String(), lp, plan)
	}
	if np.Properties != nil {
		plan = buildPropertySelection(nodeVar, np.Properties, plan)
	}
	return plan
}

// patternHasRelationships reports whether any path in pat contains at
// least one relationship hop. Used by [translateOptionalMatch] to decide
// whether a node-only OPTIONAL MATCH needs an explicit OptionalApply
// wrapper so an empty NodeScan still emits a single NULL-extended row.
func patternHasRelationships(pat *ast.Pattern) bool {
	if pat == nil {
		return false
	}
	for _, pp := range pat.Paths {
		for el := pp.Head; el != nil; el = el.Next {
			if el.Relationship != nil {
				return true
			}
		}
	}
	return false
}
