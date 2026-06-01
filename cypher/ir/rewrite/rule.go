// Package rewrite provides a rule-based logical-plan rewrite/optimisation
// framework for the Cypher IR.
//
// Usage:
//
//	reg := &rewrite.Registry{}
//	reg.Register(rewrite.PredicatePushdown{})
//	reg.Register(rewrite.ProjectionPushdown{})
//	reg.Register(rewrite.EagerInsertion{})
//	reg.Register(rewrite.FusionRules{})
//
//	driver := rewrite.NewDriver(reg)
//	optimised, count := driver.Run(ctx, plan)
//
// Concurrency: Registry and Driver are not safe for concurrent mutation.
// Read-only access (Run) on a fully constructed Driver is safe for concurrent
// use.
package rewrite

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// Rule is the interface implemented by every rewrite rule.
//
// Apply accepts a logical plan node and returns the (possibly rewritten)
// replacement node together with a boolean reporting whether any change was
// made. Apply must not modify the input node in place; it must return a new
// node when a rewrite occurs.
//
// Apply is called bottom-up by WalkAndReplace: by the time it is invoked on a
// node, all descendants have already been processed.
type Rule interface {
	// Name returns a stable, unique identifier used for logging and selective
	// disabling via WithDisabledRules.
	Name() string
	// Apply attempts to rewrite plan. It returns the (possibly new) plan and
	// true when a rewrite was performed, or the original plan and false when no
	// rewrite applies.
	Apply(plan ir.LogicalPlan) (ir.LogicalPlan, bool)
}

// ─────────────────────────────────────────────────────────────────────────────
// Context-level rule disabling
// ─────────────────────────────────────────────────────────────────────────────

type disabledRulesKey struct{}

// WithDisabledRules returns a derived context in which the named rules are
// disabled. The Driver skips disabled rules on every iteration.
func WithDisabledRules(ctx context.Context, names ...string) context.Context {
	existing, _ := ctx.Value(disabledRulesKey{}).(map[string]struct{})
	merged := make(map[string]struct{}, len(existing)+len(names))
	for k := range existing {
		merged[k] = struct{}{}
	}
	for _, n := range names {
		merged[n] = struct{}{}
	}
	return context.WithValue(ctx, disabledRulesKey{}, merged)
}

// IsDisabled reports whether the rule identified by name is disabled in ctx.
func IsDisabled(ctx context.Context, name string) bool {
	disabled, _ := ctx.Value(disabledRulesKey{}).(map[string]struct{})
	_, ok := disabled[name]
	return ok
}

// ─────────────────────────────────────────────────────────────────────────────
// Registry
// ─────────────────────────────────────────────────────────────────────────────

// Registry is an ordered collection of rewrite rules. Rules are applied in
// registration order during each fixed-point iteration.
type Registry struct {
	rules []Rule
}

// Register appends rule to the registry.
func (r *Registry) Register(rule Rule) {
	r.rules = append(r.rules, rule)
}

// Rules returns the registered rules in registration order. The returned slice
// must not be modified by the caller.
func (r *Registry) Rules() []Rule {
	return r.rules
}

// ─────────────────────────────────────────────────────────────────────────────
// Driver
// ─────────────────────────────────────────────────────────────────────────────

const defaultMaxIter = 16

// Driver runs all rules registered in a Registry to a fixed point.
//
// The driver iterates at most maxIter times (default 16). On each iteration it
// applies every enabled rule across the full plan tree using WalkAndReplace. If
// no rule fires during an iteration the loop terminates early (fixed-point
// convergence).
type Driver struct {
	registry *Registry
	maxIter  int
}

// NewDriver creates a Driver backed by the given Registry with the default
// maximum iteration count (16).
func NewDriver(reg *Registry) *Driver {
	return &Driver{registry: reg, maxIter: defaultMaxIter}
}

// Run applies all enabled rules to plan until no further rewrites are possible
// or the iteration limit is reached.
//
// It returns the final (optimised) plan and the total number of individual rule
// applications performed across all iterations.
func (d *Driver) Run(ctx context.Context, plan ir.LogicalPlan) (result ir.LogicalPlan, applied int) {
	totalApplied := 0
	for i := range d.maxIter {
		iterApplied := 0
		for _, rule := range d.registry.Rules() {
			if IsDisabled(ctx, rule.Name()) {
				continue
			}
			var changed bool
			plan, changed = WalkAndReplace(plan, rule.Apply)
			if changed {
				iterApplied++
			}
		}
		totalApplied += iterApplied
		if iterApplied == 0 {
			break
		}
		_ = i // suppress unused-variable lint when loop body references i implicitly
	}
	return plan, totalApplied
}

// ─────────────────────────────────────────────────────────────────────────────
// WalkAndReplace
// ─────────────────────────────────────────────────────────────────────────────

// WalkAndReplace traverses the plan tree bottom-up, calling fn on each node.
// When fn returns (newNode, true) the node is replaced by newNode in the tree.
// The returned bool reports whether any replacement occurred anywhere in the
// tree.
//
// WalkAndReplace handles the full operator set defined in github.com/FlavioCFOliveira/GoGraph/cypher/ir by
// reconstructing each operator with its (potentially rewritten) children.
func WalkAndReplace(plan ir.LogicalPlan, fn func(ir.LogicalPlan) (ir.LogicalPlan, bool)) (ir.LogicalPlan, bool) {
	if plan == nil {
		return nil, false
	}

	// First recurse into children (bottom-up), then apply fn to the current node.
	newPlan, childChanged := replaceChildren(plan, fn)

	replaced, nodeChanged := fn(newPlan)
	return replaced, childChanged || nodeChanged
}

// replaceChildren returns a copy of plan with each child replaced by its
// walked version. Returns (plan, false) when no child changed.
func replaceChildren(plan ir.LogicalPlan, fn func(ir.LogicalPlan) (ir.LogicalPlan, bool)) (ir.LogicalPlan, bool) { //nolint:cyclop,gocyclo // dispatches over every operator type; splitting would add noise
	switch p := plan.(type) {
	// ── leaf operators ───────────────────────────────────────────────────────
	case *ir.Argument,
		*ir.AllNodesScan,
		*ir.NodeByLabelScan,
		*ir.NodeByIndexSeek,
		*ir.NodeByIndexRangeScan:
		return plan, false

	// ── unary operators ──────────────────────────────────────────────────────
	case *ir.Expand:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewExpand(p.FromVar, p.RelVar, p.RelTypes, p.Direction, p.ToVar, child), true

	case *ir.OptionalExpand:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewOptionalExpand(p.FromVar, p.RelVar, p.RelTypes, p.Direction, p.ToVar, child), true

	case *ir.VarLengthExpand:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		vle := ir.NewVarLengthExpand(p.FromVar, p.RelVar, p.RelTypes, p.Direction, p.ToVar, p.MinDepth, p.MaxDepth, child)
		vle.PathVar = p.PathVar
		return vle, true

	case *ir.ProjectEndpoints:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewProjectEndpoints(p.RelVar, p.StartVar, p.EndVar, child), true

	case *ir.Selection:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewSelection(p.Predicate, child), true

	case *ir.Projection:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewProjection(p.Items, child), true

	case *ir.EagerAggregation:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewEagerAggregation(p.GroupBy, p.Aggregates, child), true

	case *ir.Sort:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewSort(p.SortItems, child), true

	case *ir.Top:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewTop(p.SortItems, p.Limit, child), true

	case *ir.Limit:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewLimit(p.Count, child), true

	case *ir.Skip:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewSkip(p.Count, child), true

	case *ir.Distinct:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewDistinct(child), true

	case *ir.Eager:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewEager(child), true

	case *ir.Unwind:
		if p.Child == nil {
			return plan, false
		}
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewUnwind(p.ListExpression, p.ElementVar, child), true

	case *ir.ProduceResults:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewProduceResults(p.Columns, child), true

	// ── write operators (unary) ──────────────────────────────────────────────
	case *ir.CreateNode:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewCreateNode(p.NodeVar, p.Labels, p.Properties, child), true

	case *ir.CreateRelationship:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewCreateRelationship(p.StartVar, p.EndVar, p.RelVar, p.RelType, p.Properties, child), true

	case *ir.SetProperty:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewSetProperty(p.EntityVar, p.PropertyKey, p.Value, child), true

	case *ir.SetLabels:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewSetLabels(p.NodeVar, p.Labels, child), true

	case *ir.RemoveProperty:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewRemoveProperty(p.EntityVar, p.PropertyKey, child), true

	case *ir.RemoveLabels:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewRemoveLabels(p.NodeVar, p.Labels, child), true

	case *ir.DeleteNode:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewDeleteNode(p.NodeVar, child), true

	case *ir.DeleteRelationship:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewDeleteRelationship(p.RelVar, child), true

	case *ir.DetachDelete:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewDetachDelete(p.NodeVar, child), true

	case *ir.Merge:
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewMerge(p.Pattern, p.OnCreate, p.OnMatch, p.BoundVars, child), true

	// ── binary operators ─────────────────────────────────────────────────────
	case *ir.Union:
		left, lc := WalkAndReplace(p.Left, fn)
		right, rc := WalkAndReplace(p.Right, fn)
		if !lc && !rc {
			return plan, false
		}
		return ir.NewUnion(left, right), true

	case *ir.UnionAll:
		left, lc := WalkAndReplace(p.Left, fn)
		right, rc := WalkAndReplace(p.Right, fn)
		if !lc && !rc {
			return plan, false
		}
		return ir.NewUnionAll(left, right), true

	case *ir.Apply:
		outer, oc := WalkAndReplace(p.Outer, fn)
		inner, ic := WalkAndReplace(p.Inner, fn)
		if !oc && !ic {
			return plan, false
		}
		return ir.NewApply(outer, inner), true

	case *ir.CorrelatedApply:
		outer, oc := WalkAndReplace(p.Outer, fn)
		inner, ic := WalkAndReplace(p.Inner, fn)
		if !oc && !ic {
			return plan, false
		}
		// Preserve the original ArgTag so the inner Argument leaf still resolves
		// to the same exec.Argument instance under physical build.
		return ir.NewCorrelatedApplyWithTag(outer, inner, p.ArgTag), true

	case *ir.OptionalApply:
		outer, oc := WalkAndReplace(p.Outer, fn)
		inner, ic := WalkAndReplace(p.Inner, fn)
		if !oc && !ic {
			return plan, false
		}
		return ir.NewOptionalApplyWithTag(outer, inner, p.ArgTag), true

	case *ir.SemiApply:
		outer, oc := WalkAndReplace(p.Outer, fn)
		inner, ic := WalkAndReplace(p.Inner, fn)
		if !oc && !ic {
			return plan, false
		}
		// Preserve the original ArgTag so the inner Argument leaf still resolves
		// to the same exec.Argument instance under physical build.
		return ir.NewSemiApplyWithTag(outer, inner, p.ArgTag), true

	case *ir.AntiSemiApply:
		outer, oc := WalkAndReplace(p.Outer, fn)
		inner, ic := WalkAndReplace(p.Inner, fn)
		if !oc && !ic {
			return plan, false
		}
		return ir.NewAntiSemiApplyWithTag(outer, inner, p.ArgTag), true

	case *ir.RollUpApply:
		outer, oc := WalkAndReplace(p.Outer, fn)
		inner, ic := WalkAndReplace(p.Inner, fn)
		if !oc && !ic {
			return plan, false
		}
		return ir.NewRollUpApply(outer, inner, p.CollectVar), true

	case *ir.ProcedureCall:
		if p.Child == nil {
			return plan, false
		}
		child, changed := WalkAndReplace(p.Child, fn)
		if !changed {
			return plan, false
		}
		return ir.NewProcedureCall(p.Namespace, p.Name, p.Arguments, p.YieldVars, child), true

	default:
		// Unknown operator type — return as-is; do not panic (defensive).
		return plan, false
	}
}
