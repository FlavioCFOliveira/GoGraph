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
func (t *translator) createClause(c *ast.Create, child LogicalPlan) (LogicalPlan, error) {
	plan := child
	for _, pp := range c.Pattern.Paths {
		var err error
		plan, err = t.createPathPattern(pp, plan)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// createPathPattern translates a single path pattern in a CREATE clause.
// The anchor node is translated first, then each (relationship, node) hop is
// translated left-to-right, stacking operators.
func (t *translator) createPathPattern(pp *ast.PathPattern, child LogicalPlan) (LogicalPlan, error) {
	if pp == nil || pp.Head == nil {
		return child, nil
	}
	plan := child
	el := pp.Head

	// Translate the anchor node.
	if el.Node != nil {
		plan = t.createNode(el.Node, plan)
	}

	el = el.Next
	for el != nil {
		if el.Relationship != nil && el.Node != nil {
			plan = t.createRelationship(el.Relationship, el.Node, plan)
		}
		el = el.Next
	}
	return plan, nil
}

// createNode emits a CreateNode operator for the given NodePattern.
//
// Anonymous nodes (no variable in the pattern) receive a synthetic internal
// variable name of the form "__anon_N" so that downstream CreateRelationship
// operators can resolve their endpoint columns from the schema.
func (t *translator) createNode(np *ast.NodePattern, child LogicalPlan) LogicalPlan {
	nodeVar := ""
	if np.Variable != nil {
		nodeVar = *np.Variable
	}
	if nodeVar == "" {
		nodeVar = t.freshAnonVar()
	}
	labels := make([]string, len(np.Labels))
	copy(labels, np.Labels)
	props := ""
	if np.Properties != nil {
		props = np.Properties.String()
	}
	return NewCreateNode(nodeVar, labels, props, child)
}

// createRelationship emits CreateNode(destination) then CreateRelationship
// linking the start-node variable (taken from the driving plan) to the new
// destination node.
//
// Anonymous endpoints (no variable in the pattern) receive synthetic internal
// names from [translator.createNode] so the executor can resolve them from the
// schema. The end-var is read from the destination node plan after construction
// to ensure the synthetic name is consistent.
func (t *translator) createRelationship(rp *ast.RelationshipPattern, to *ast.NodePattern, child LogicalPlan) LogicalPlan {
	startVar := firstVar(child)
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
	// Create destination node first; its var (real or synthetic) becomes endVar.
	nodePlan := t.createNode(to, child)
	endVar := firstVar(nodePlan) // picks up synthetic __anon_N for anonymous nodes
	return NewCreateRelationship(startVar, endVar, relVar, relType, props, nodePlan)
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
