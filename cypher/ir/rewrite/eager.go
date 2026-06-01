package rewrite

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// EagerInsertion inserts Eager barrier operators before write operators that
// read from the same graph state they modify, following the semantics of
// Green et al. 2019 §5.
//
// Rules implemented:
//
//  1. Merge always requires an Eager barrier (it both reads and writes
//     atomically; see Green 2019 §5.3).
//  2. CREATE after MATCH (i.e. CreateNode or CreateRelationship whose subtree
//     contains a scan/traversal operator) requires an Eager barrier.
//  3. DELETE (DeleteNode, DeleteRelationship, DetachDelete) after MATCH
//     requires an Eager barrier.
//  4. SET/REMOVE (SetProperty, SetLabels, RemoveProperty, RemoveLabels) are
//     non-eager unless they appear inside an iterating context (a loop). At
//     the logical-plan level detected by the presence of a scan or traversal
//     in the subtree. Since we cannot distinguish a loop at this IR level, we
//     treat SET/REMOVE conservatively: they are NOT made eager here; the rule
//     is left open for extension.
//
// Idempotence: the rule checks whether an Eager is already the immediate child
// of the write operator to avoid double-insertion.
//
// Concurrency: EagerInsertion is stateless and goroutine-safe.
type EagerInsertion struct{}

// Name implements Rule.
func (EagerInsertion) Name() string { return "EagerInsertion" }

// Apply implements Rule. It matches write operators and inserts an Eager
// barrier between the operator and its child when the child subtree contains a
// read (scan or traversal) operator.
func (EagerInsertion) Apply(plan ir.LogicalPlan) (ir.LogicalPlan, bool) { //nolint:gocyclo // type switch over 6 distinct write operators; cannot split without introducing indirection
	switch p := plan.(type) {
	// ── MERGE: always eager ──────────────────────────────────────────────────
	case *ir.Merge:
		if _, alreadyEager := p.Child.(*ir.Eager); alreadyEager {
			return plan, false
		}
		return ir.NewMerge(p.Pattern, p.OnCreate, p.OnMatch, p.BoundVars, ir.NewEager(p.Child)), true

	// ── CREATE: eager when subtree reads ────────────────────────────────────
	case *ir.CreateNode:
		if _, alreadyEager := p.Child.(*ir.Eager); alreadyEager {
			return plan, false
		}
		if !subtreeContainsRead(p.Child) {
			return plan, false
		}
		return ir.NewCreateNode(p.NodeVar, p.Labels, p.Properties, ir.NewEager(p.Child)), true

	case *ir.CreateRelationship:
		if _, alreadyEager := p.Child.(*ir.Eager); alreadyEager {
			return plan, false
		}
		if !subtreeContainsRead(p.Child) {
			return plan, false
		}
		return ir.NewCreateRelationship(p.StartVar, p.EndVar, p.RelVar, p.RelType, p.Properties, ir.NewEager(p.Child)), true

	// ── DELETE: eager when subtree reads ────────────────────────────────────
	case *ir.DeleteNode:
		if _, alreadyEager := p.Child.(*ir.Eager); alreadyEager {
			return plan, false
		}
		if !subtreeContainsRead(p.Child) {
			return plan, false
		}
		return ir.NewDeleteNode(p.NodeVar, ir.NewEager(p.Child)), true

	case *ir.DeleteRelationship:
		if _, alreadyEager := p.Child.(*ir.Eager); alreadyEager {
			return plan, false
		}
		if !subtreeContainsRead(p.Child) {
			return plan, false
		}
		return ir.NewDeleteRelationship(p.RelVar, ir.NewEager(p.Child)), true

	case *ir.DetachDelete:
		if _, alreadyEager := p.Child.(*ir.Eager); alreadyEager {
			return plan, false
		}
		if !subtreeContainsRead(p.Child) {
			return plan, false
		}
		return ir.NewDetachDelete(p.NodeVar, ir.NewEager(p.Child)), true

	default:
		return plan, false
	}
}

// subtreeContainsRead reports whether the plan tree rooted at p contains at
// least one read (scan or traversal) operator. This distinguishes
// CREATE/DELETE-only plans (no eager needed) from MATCH+write plans.
func subtreeContainsRead(plan ir.LogicalPlan) bool {
	if plan == nil {
		return false
	}
	switch plan.(type) {
	case *ir.AllNodesScan,
		*ir.NodeByLabelScan,
		*ir.NodeByIndexSeek,
		*ir.NodeByIndexRangeScan,
		*ir.Expand,
		*ir.OptionalExpand,
		*ir.VarLengthExpand:
		return true
	}
	for _, child := range plan.Children() {
		if subtreeContainsRead(child) {
			return true
		}
	}
	return false
}
