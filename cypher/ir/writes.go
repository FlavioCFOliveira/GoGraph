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
func (t *translator) mergeClause(m *ast.Merge, child LogicalPlan) (LogicalPlan, error) {
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

// setClause translates a SET clause. Each SetItem becomes either SetLabels (for
// the `SET n:Label` form) or SetProperty (for `SET n.prop = expr` and whole-
// entity assignment forms).
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
			plan = NewSetProperty(prop.Receiver.String(), prop.Key, item.Value.String(), plan)
		} else {
			// Whole-node assignment (n = {…} or n += {…}): model as SetProperty
			// with an empty property key to signal whole-entity update until a
			// dedicated expression IR is introduced.
			plan = NewSetProperty(item.Target.String(), "", item.Value.String(), plan)
		}
	}
	return plan, nil
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
// operator stacked on top of the previous plan.
func (t *translator) deleteClause(d *ast.Delete, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, expr := range d.Expressions {
		plan = NewDeleteNode(expr.String(), plan)
	}
	return plan, nil
}

// detachDeleteClause translates a DETACH DELETE clause.
func (t *translator) detachDeleteClause(d *ast.DetachDelete, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, expr := range d.Expressions {
		plan = NewDetachDelete(expr.String(), plan)
	}
	return plan, nil
}
