package rewrite

import (
	"gograph/cypher/ir"
)

// ProjectionPushdown removes Projection operators whose output columns are not
// referenced by any ancestor. It operates in two complementary modes:
//
//  1. Dead-column elimination: trim ProjectionItem entries whose Name is not in
//     the required set propagated from ancestors. When all items are trimmed the
//     Projection node itself is removed.
//  2. Redundant-node removal: when a Projection node becomes empty after
//     trimming, replace it with its child.
//
// The rule is stateless. Required-column information is threaded through the
// tree via a separate top-down walk that annotates each Projection with the set
// of columns actually needed by its ancestors.
//
// Concurrency: ProjectionPushdown is stateless and goroutine-safe.
type ProjectionPushdown struct{}

// Name implements Rule.
func (ProjectionPushdown) Name() string { return "ProjectionPushdown" }

// Apply implements Rule. It matches a Projection node and eliminates dead
// columns. The required set is determined by inspecting the immediate parent
// context: since WalkAndReplace is bottom-up, we use a simpler approach —
// inspect the Projection's direct parent usage by examining what the Projection
// itself exposes and whether any of its items are referenced further up.
//
// Implementation note: because WalkAndReplace is bottom-up, by the time Apply
// is called on a Projection its child has already been rewritten. We handle the
// two standard patterns:
//
//   - Projection over Projection: the inner Projection is only needed to supply
//     columns required by the outer. Columns in the inner that are not referenced
//     by the outer are dead. This is handled by FusionRules (double-projection).
//   - ProduceResults over Projection: the ProduceResults declares the live column
//     set; we eliminate dead Projection items.
//   - Projection with zero items: replace with child.
func (ProjectionPushdown) Apply(plan ir.LogicalPlan) (ir.LogicalPlan, bool) {
	switch p := plan.(type) {
	case *ir.ProduceResults:
		return pruneProjectionUnderProduceResults(p)
	case *ir.Projection:
		return pruneEmptyProjection(p)
	default:
		return plan, false
	}
}

// pruneProjectionUnderProduceResults removes projection items that are not
// referenced by the ProduceResults columns.
func pruneProjectionUnderProduceResults(pr *ir.ProduceResults) (ir.LogicalPlan, bool) {
	proj, ok := pr.Child.(*ir.Projection)
	if !ok {
		return pr, false
	}

	required := stringSet(pr.Columns)
	var kept []ir.ProjectionItem
	for _, item := range proj.Items {
		if _, need := required[item.Name]; need {
			kept = append(kept, item)
		}
	}

	if len(kept) == len(proj.Items) {
		// Nothing changed.
		return pr, false
	}

	if len(kept) == 0 {
		// All items dead: remove the Projection entirely.
		newPR := ir.NewProduceResults(pr.Columns, proj.Child)
		return newPR, true
	}

	newProj := ir.NewProjection(kept, proj.Child)
	newPR := ir.NewProduceResults(pr.Columns, newProj)
	return newPR, true
}

// pruneEmptyProjection removes a Projection that has zero items, replacing it
// with its child.
func pruneEmptyProjection(p *ir.Projection) (ir.LogicalPlan, bool) {
	if len(p.Items) == 0 {
		return p.Child, true
	}
	return p, false
}

// stringSet converts a string slice to a set for O(1) membership testing.
func stringSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[s] = struct{}{}
	}
	return m
}
