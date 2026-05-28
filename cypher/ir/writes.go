package ir

import "gograph/cypher/ast"

// writes.go — CREATE / MERGE / SET / REMOVE / DELETE translation.
//
// Each write clause translates to one or more write operators layered on top of
// the driving plan (child). The operators are stacked innermost-first so that
// the outermost operator in the plan tree corresponds to the last item processed.
//
// Dispatch entry-point: translator.updatingClause (translator.go).
//
// Supported mappings:
//
//   CREATE (n:Person {name:"Alice"})  → CreateNode("n", ["Person"], "{name:\"Alice\"}", child)
//   CREATE (a)-[:R]->(b)              → CreateRelationship(startVar, "b", "", "R", "", CreateNode("b", …, CreateNode("a", …, child)))
//   MERGE  (n:Person {name:"Alice"})  → Merge(pattern, onCreate, onMatch, boundVars, child)
//   SET    n.name = "Bob"             → SetProperty("n", "name", "\"Bob\"", child)
//   SET    n:Label                    → SetLabels("n", ["Label"], child)
//   REMOVE n.name                     → RemoveProperty("n", "name", child)
//   REMOVE n:Label                    → RemoveLabels("n", ["Label"], child)
//   DELETE n                          → DeleteNode("n", child)
//   DETACH DELETE n                   → DetachDelete("n", child)

// createClause translates a CREATE clause. Each path pattern is translated in
// sequence, with the output of one becoming the child of the next.
//
// A `bound` set tracks every variable already in scope at the start of this
// CREATE clause (from a leading MATCH / WITH / earlier CREATE) plus the
// variables produced by earlier patterns in the same CREATE. When a later
// pattern re-mentions an already-bound variable, the translator references it
// instead of emitting another CreateNode for the same name; this keeps
// `CREATE (a:User), (b:User), (a)-[:F]->(b), (b)-[:F]->(a)` and
// `MATCH (a),(b) CREATE (a)-[:F]->(b)` from spawning duplicate nodes or
// corrupting the schema map.
func (t *translator) createClause(c *ast.Create, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	bound := collectPlanVars(child)
	for _, pp := range c.Pattern.Paths {
		var err error
		plan, err = t.createPathPattern(pp, plan, bound)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// createPathPattern translates a single path pattern in a CREATE clause.
// The anchor node is translated first, then each (relationship, node) hop is
// translated left-to-right, stacking operators. `bound` is the running set of
// variables already in scope; it is mutated as new nodes are emitted. The
// translator threads the anchor's name forward so that re-references (where
// no new operator is pushed) still produce a correct startVar for the next
// CreateRelationship.
func (t *translator) createPathPattern(pp *ast.PathPattern, child LogicalPlan, bound map[string]struct{}) (LogicalPlan, error) {
	if pp == nil || pp.Head == nil {
		return child, nil
	}
	plan := child
	el := pp.Head

	var startVar string
	if el.Node != nil {
		startVar = t.ensureNodeVar(el.Node)
		plan = t.createNode(el.Node, plan, bound)
	}

	el = el.Next
	for el != nil {
		if el.Relationship != nil && el.Node != nil {
			endVar := t.ensureNodeVar(el.Node)
			plan = t.createRelationship(el.Relationship, el.Node, plan, bound, startVar)
			// Subsequent hops chain from the destination of this step.
			startVar = endVar
		}
		el = el.Next
	}
	return plan, nil
}

// ensureNodeVar returns the variable name written in np, allocating a fresh
// synthetic "__anon_N" for anonymous patterns and storing it back into
// np.Variable so any later read (in particular [translator.createNode]) sees
// the same name. The translator runs on a fresh AST per query so the
// mutation is local to this query's lowering pass.
func (t *translator) ensureNodeVar(np *ast.NodePattern) string {
	if np.Variable != nil {
		return *np.Variable
	}
	name := t.freshAnonVar()
	np.Variable = &name
	return name
}

// createNode emits a CreateNode operator for the given NodePattern, unless the
// variable is already bound in scope (in which case the pattern is a
// re-reference and the child plan is returned unchanged). Anonymous nodes
// have had a synthetic "__anon_N" name installed on np.Variable upstream by
// [translator.ensureNodeVar], so every anonymous occurrence resolves to a
// fresh creation here.
func (t *translator) createNode(np *ast.NodePattern, child LogicalPlan, bound map[string]struct{}) LogicalPlan {
	nodeVar := ""
	if np.Variable != nil {
		nodeVar = *np.Variable
	}
	if nodeVar == "" {
		nodeVar = t.freshAnonVar()
	} else if _, already := bound[nodeVar]; already {
		// Re-reference of a variable already bound — either from a
		// leading MATCH/WITH/CREATE, or from an earlier pattern in
		// this same CREATE clause. Do not emit another CreateNode.
		return child
	}
	bound[nodeVar] = struct{}{}
	labels := make([]string, len(np.Labels))
	copy(labels, np.Labels)
	props := ""
	if np.Properties != nil {
		props = np.Properties.String()
	}
	if np.Properties != nil {
		if _, isLiteral := allMapValuesLiteral(np.Properties); !isLiteral {
			// Property map contains non-literal expressions (variable refs,
			// property accesses, arithmetic) that cannot be parsed at plan-
			// construction time. Store the AST so the physical builder can
			// construct a per-row evaluation closure.
			return NewCreateNodeExpr(nodeVar, labels, props, np.Properties, child)
		}
	}
	return NewCreateNode(nodeVar, labels, props, child)
}

// createRelationship emits CreateRelationship linking startVar to the
// destination node. When the destination is anonymous or not yet bound,
// createNode appends a CreateNode for it; an already-bound destination is
// re-referenced. startVar is supplied by the caller (the just-mentioned
// anchor in the path pattern, or the destination of the previous hop) so the
// pattern semantics survive re-references that do not push a new operator.
func (t *translator) createRelationship(rp *ast.RelationshipPattern, to *ast.NodePattern, child LogicalPlan, bound map[string]struct{}, startVar string) LogicalPlan {
	relVar := ""
	if rp.Variable != nil {
		relVar = *rp.Variable
	}
	relType := ""
	if len(rp.Types) > 0 {
		relType = rp.Types[0]
	}
	props := ""
	if rp.Properties != nil {
		props = rp.Properties.String()
	}

	// The destination's variable name has already been installed on
	// to.Variable by [translator.ensureNodeVar] in createPathPattern;
	// createNode will not re-allocate it.
	endVar := ""
	if to.Variable != nil {
		endVar = *to.Variable
	}
	nodePlan := t.createNode(to, child, bound)
	// Honour the relationship pattern direction: `<-` swaps the endpoint
	// order so the stored edge goes from the right-hand node back to the
	// left-hand node. Direction `none` is treated as outgoing for CREATE
	// (openCypher requires directed CREATE — bidirectional CREATE is a
	// compile-time error elsewhere in the pipeline).
	if rp.Direction == ast.RelDirectionIncoming {
		startVar, endVar = endVar, startVar
	}
	if rp.Properties != nil {
		if _, isLiteral := allMapValuesLiteral(rp.Properties); !isLiteral {
			return NewCreateRelationshipExpr(startVar, endVar, relVar, relType, props, rp.Properties, nodePlan)
		}
	}
	return NewCreateRelationship(startVar, endVar, relVar, relType, props, nodePlan)
}

// collectPlanVars walks plan top-down and returns the union of every variable
// named anywhere in the subtree. The result is a conservative
// over-approximation of the bindings in scope at the output of plan; it is
// suitable for "is this name already known?" lookups in [createClause] but
// must not be used to drive correctness-sensitive scope inference, which
// requires the operator-specific Vars() contract.
func collectPlanVars(plan LogicalPlan) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(p LogicalPlan)
	walk = func(p LogicalPlan) {
		if p == nil {
			return
		}
		for _, v := range p.Vars() {
			if v != "" {
				out[v] = struct{}{}
			}
		}
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return out
}

// mergeClause translates a MERGE clause.
//
// Special-case: when the pattern is a single-hop relationship between
// two endpoint variables that are BOTH already bound by the child
// plan and the MERGE has no ON CREATE / ON MATCH actions, the
// translator emits [MergeRelationship] — a focused operator that
// checks for an existing edge and creates one only when absent. All
// other MERGE shapes (node-only, multi-hop, with ON-actions) keep
// using the node-oriented [Merge] path.
func (t *translator) mergeClause(m *ast.Merge, child LogicalPlan) (LogicalPlan, error) {
	if child != nil {
		if srcVar, dstVar, relVar, relType, relProps, ok := mergeSingleHopRel(m.Pattern); ok {
			outerVars := collectAllVars(child)
			outer := map[string]struct{}{}
			for _, v := range outerVars {
				outer[v] = struct{}{}
			}
			if _, hasSrc := outer[srcVar]; hasSrc {
				if _, hasDst := outer[dstVar]; hasDst {
					// Extract ON CREATE / ON MATCH SET items whose
					// target is the relationship variable. Other targets
					// (the endpoint nodes or unrelated names) fall back
					// to the node-only Merge path.
					onCreate, ocOk := extractRelKVActions(m.OnCreate, relVar)
					onMatch, omOk := extractRelKVActions(m.OnMatch, relVar)
					if ocOk && omOk {
						mr := NewMergeRelationshipWithActions(srcVar, dstVar, relVar, relType, onCreate, onMatch, child)
						mr.RelProps = relProps
						return mr, nil
					}
				}
			}
		}
	}
	onCreate := make([]string, len(m.OnCreate))
	for i, si := range m.OnCreate {
		onCreate[i] = si.String()
	}
	onMatch := make([]string, len(m.OnMatch))
	for i, si := range m.OnMatch {
		onMatch[i] = si.String()
	}
	boundVars := patternVars(m.Pattern)
	return NewMerge(m.Pattern.String(), onCreate, onMatch, boundVars, child), nil
}

// extractRelKVActions converts ON CREATE / ON MATCH SET items into a
// [KVAction] slice when EVERY item is one of:
//
//   - Property-on-relVar form: `SET <relVar>.<key> = <value>` — each
//     produces a single KVAction.
//   - Literal-map form on relVar: `SET <relVar> = {…}` or
//     `SET <relVar> += {…}` where the RHS is a *ast.MapLiteral whose
//     keys are identifiers and values are themselves literals — each
//     map entry produces a KVAction. Closes Merge6 [7].
//
// Returns ok=false when any item is on a different target (label
// assignment, entity copy from another bound entity, property on a
// different variable, etc.) so the caller falls back to the node-only
// Merge path. relVar="" returns ok=false unless items is empty.
func extractRelKVActions(items []*ast.SetItem, relVar string) ([]KVAction, bool) {
	if len(items) == 0 {
		return nil, true
	}
	if relVar == "" {
		return nil, false
	}
	out := make([]KVAction, 0, len(items))
	for _, item := range items {
		if item == nil || len(item.Labels) > 0 || item.Value == nil {
			return nil, false
		}
		// Property-on-relVar form.
		if prop, isProp := item.Target.(*ast.Property); isProp {
			recv, isVar := prop.Receiver.(*ast.Variable)
			if !isVar || recv.Name != relVar {
				return nil, false
			}
			out = append(out, KVAction{Key: prop.Key, Value: item.Value.String()})
			continue
		}
		// Literal-map form on relVar: SET relVar = {…} or SET relVar += {…}.
		// Decompose the map into one KVAction per key. The "+=" form is a
		// no-op for the property-merge semantics we already implement — each
		// key is either overwritten or added — so both operators flow
		// through the same per-key writes. The whole-entity REPLACE
		// semantics of "=" (drop existing properties first) is NOT
		// implemented here; only the additive subset.
		if v, isVar := item.Target.(*ast.Variable); isVar && v.Name == relVar {
			ml, isMap := item.Value.(*ast.MapLiteral)
			if !isMap {
				return nil, false
			}
			// Every value must be a compile-time literal — variable
			// references or function calls would need per-row evaluation,
			// which MergeRelationship does not yet thread through.
			allLit, _ := allMapValuesLiteral(ml)
			if !allLit {
				return nil, false
			}
			for i, key := range ml.Keys {
				if i >= len(ml.Values) {
					break
				}
				out = append(out, KVAction{Key: key, Value: ml.Values[i].String()})
			}
			continue
		}
		return nil, false
	}
	return out, true
}

// mergeSingleHopRel returns (srcVar, dstVar, relVar, relType, relProps, true)
// when pp is a single-hop directed/undirected relationship pattern with two
// named endpoints and at most one type label. relProps carries the inline
// relationship property-map source string when present, or "" when absent.
// Returns ok=false for any other shape (zero hops, multi-hop, anonymous
// endpoint, zero or multiple types). The check is intentionally narrow:
// only the canonical Merge5-style shape qualifies.
func mergeSingleHopRel(pp *ast.PathPattern) (srcVar, dstVar, relVar, relType, relProps string, ok bool) {
	if pp == nil || pp.Head == nil {
		return "", "", "", "", "", false
	}
	head := pp.Head
	if head.Node == nil || head.Node.Variable == nil {
		return "", "", "", "", "", false
	}
	step := head.Next
	if step == nil || step.Relationship == nil || step.Node == nil || step.Node.Variable == nil {
		return "", "", "", "", "", false
	}
	if step.Next != nil {
		// Multi-hop — handled by the node-only Merge path for now.
		return "", "", "", "", "", false
	}
	if len(step.Relationship.Types) != 1 {
		return "", "", "", "", "", false
	}
	if head.Node.Properties != nil || step.Node.Properties != nil ||
		len(head.Node.Labels) != 0 || len(step.Node.Labels) != 0 {
		// Re-asserting labels/properties on bound endpoints is the
		// "Fail when imposing new predicates" scenario; skip
		// translation here so the node-only Merge path can surface
		// the appropriate error.
		return "", "", "", "", "", false
	}
	rv := ""
	if step.Relationship.Variable != nil {
		rv = *step.Relationship.Variable
	}
	src := *head.Node.Variable
	dst := *step.Node.Variable
	// For outgoing direction the source is head, dst is step; for
	// incoming we swap so the edge is stored in the canonical direction.
	if step.Relationship.Direction == ast.RelDirectionIncoming {
		src, dst = dst, src
	}
	rp := ""
	if step.Relationship.Properties != nil {
		rp = step.Relationship.Properties.String()
	}
	return src, dst, rv, step.Relationship.Types[0], rp, true
}

// setClause translates a SET clause. Each SetItem becomes one of:
//   - [SetLabels] for the `SET n:Label` form.
//   - [SetProperty] for the single-property form `SET n.prop = expr`.
//   - [SetAllProperties] for the whole-entity forms `SET n = …` and
//     `SET n += …`, where the right-hand side may be another bound entity, a
//     literal map, or a parameter reference.
func (t *translator) setClause(s *ast.Set, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, item := range s.Items {
		if len(item.Labels) > 0 {
			entityVar := item.Target.String()
			labels := make([]string, len(item.Labels))
			copy(labels, item.Labels)
			plan = NewSetLabels(entityVar, labels, plan)
			continue
		}
		if prop, ok := item.Target.(*ast.Property); ok {
			plan = NewSetPropertyExpr(prop.Receiver.String(), prop.Key, item.Value.String(), item.Value, plan)
			continue
		}
		// Whole-entity assignment: `SET n = …` (replace) or `SET n += …` (merge).
		entityVar := item.Target.String()
		isReplace := item.Operator == "="
		plan = newSetAllForValue(entityVar, item.Value, isReplace, plan)
	}
	return plan, nil
}

// newSetAllForValue produces the correct SetAllProperties shape for the given
// RHS expression. Variable references become entity-copy operators; map
// literals become literal-map operators; parameter references become
// parameter-source operators. Any other expression form is conservatively
// encoded as a literal-map string so the exec layer's literal-map parser can
// attempt to handle it (and silently no-op when it cannot).
func newSetAllForValue(entityVar string, value ast.Expression, isReplace bool, child LogicalPlan) *SetAllProperties {
	switch v := value.(type) {
	case *ast.Variable:
		return NewSetAllPropertiesFromEntity(entityVar, v.Name, isReplace, child)
	case *ast.Parameter:
		return NewSetAllPropertiesFromParam(entityVar, v.Name, isReplace, child)
	case *ast.MapLiteral:
		return NewSetAllPropertiesFromMap(entityVar, v.String(), isReplace, child)
	default:
		return NewSetAllPropertiesFromMap(entityVar, value.String(), isReplace, child)
	}
}

// removeClause translates a REMOVE clause. Each RemoveItem becomes either
// RemoveLabels or RemoveProperty.
func (t *translator) removeClause(r *ast.Remove, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, item := range r.Items {
		if len(item.Labels) > 0 {
			entityVar := item.Target.String()
			labels := make([]string, len(item.Labels))
			copy(labels, item.Labels)
			plan = NewRemoveLabels(entityVar, labels, plan)
			continue
		}
		if prop, ok := item.Target.(*ast.Property); ok {
			plan = NewRemoveProperty(prop.Receiver.String(), prop.Key, plan)
		} else {
			plan = NewRemoveProperty(item.Target.String(), "", plan)
		}
	}
	return plan, nil
}

// deleteClause translates a DELETE clause. Each expression becomes a DeleteNode
// operator stacked on top of the previous plan. The parsed AST is carried
// forward so the exec layer can evaluate non-variable targets (subscripts,
// property access, etc.) per row.
func (t *translator) deleteClause(d *ast.Delete, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, e := range d.Expressions {
		plan = NewDeleteNodeExpr(e.String(), e, plan)
	}
	return plan, nil
}

// detachDeleteClause translates a DETACH DELETE clause.
func (t *translator) detachDeleteClause(d *ast.DetachDelete, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, e := range d.Expressions {
		plan = NewDetachDeleteExpr(e.String(), e, plan)
	}
	return plan, nil
}

// allMapValuesLiteral reports whether every value expression in e is a
// compile-time literal (IntLiteral, FloatLiteral, StringLiteral,
// BoolLiteral, NullLiteral, or a nested MapLiteral/ListLiteral whose
// elements are also all literals). When the answer is true the property
// map can be fully parsed at plan-construction time without a row context.
// Returns (true, true) when e is nil or not a *ast.MapLiteral.
//
// The second return value mirrors the first and is exposed for symmetry
// with callers that read both values with a single declaration.
func allMapValuesLiteral(e ast.Expression) (allLiteral, _ bool) {
	ml, ok := e.(*ast.MapLiteral)
	if !ok {
		return true, true
	}
	for _, v := range ml.Values {
		if !isLiteralExpr(v) {
			return false, false
		}
	}
	return true, true
}

// isLiteralExpr reports whether e is a compile-time Cypher literal.
func isLiteralExpr(e ast.Expression) bool {
	switch v := e.(type) {
	case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral,
		*ast.BoolLiteral, *ast.NullLiteral:
		return true
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			if !isLiteralExpr(el) {
				return false
			}
		}
		return true
	case *ast.MapLiteral:
		for _, mv := range v.Values {
			if !isLiteralExpr(mv) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
