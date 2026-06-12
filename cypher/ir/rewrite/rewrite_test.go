package rewrite_test

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir/rewrite"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func scan(v string) ir.LogicalPlan { return ir.NewAllNodesScan(v) }
func labelScan(v, label string) ir.LogicalPlan {
	return ir.NewNodeByLabelScan(v, label)
}

// planType returns the concrete type name of a LogicalPlan for assertion messages.
func planType(p ir.LogicalPlan) string { //nolint:gocyclo // exhaustive type-switch helper for tests
	if p == nil {
		return "<nil>"
	}
	switch p.(type) {
	case *ir.AllNodesScan:
		return "AllNodesScan"
	case *ir.NodeByLabelScan:
		return "NodeByLabelScan"
	case *ir.Selection:
		return "Selection"
	case *ir.Projection:
		return "Projection"
	case *ir.Limit:
		return "Limit"
	case *ir.Skip:
		return "Skip"
	case *ir.Sort:
		return "Sort"
	case *ir.Distinct:
		return "Distinct"
	case *ir.Eager:
		return "Eager"
	case *ir.CreateNode:
		return "CreateNode"
	case *ir.CreateRelationship:
		return "CreateRelationship"
	case *ir.DeleteNode:
		return "DeleteNode"
	case *ir.DetachDelete:
		return "DetachDelete"
	case *ir.Merge:
		return "Merge"
	case *ir.Top:
		return "Top"
	case *ir.EagerAggregation:
		return "EagerAggregation"
	case *ir.ProduceResults:
		return "ProduceResults"
	case *ir.Unwind:
		return "Unwind"
	case *ir.Union:
		return "Union"
	case *ir.UnionAll:
		return "UnionAll"
	case *ir.Apply:
		return "Apply"
	case *ir.SemiApply:
		return "SemiApply"
	case *ir.AntiSemiApply:
		return "AntiSemiApply"
	case *ir.RollUpApply:
		return "RollUpApply"
	case *ir.ProcedureCall:
		return "ProcedureCall"
	case *ir.SetProperty:
		return "SetProperty"
	case *ir.SetLabels:
		return "SetLabels"
	case *ir.RemoveProperty:
		return "RemoveProperty"
	case *ir.RemoveLabels:
		return "RemoveLabels"
	case *ir.DeleteRelationship:
		return "DeleteRelationship"
	case *ir.VarLengthExpand:
		return "VarLengthExpand"
	case *ir.OptionalExpand:
		return "OptionalExpand"
	case *ir.Expand:
		return "Expand"
	case *ir.ProjectEndpoints:
		return "ProjectEndpoints"
	case *ir.NodeByIndexSeek:
		return "NodeByIndexSeek"
	case *ir.NodeByIndexRangeScan:
		return "NodeByIndexRangeScan"
	case *ir.Argument:
		return "Argument"
	default:
		return "unknown"
	}
}

func assertType(t *testing.T, p ir.LogicalPlan, want string) {
	t.Helper()
	if got := planType(p); got != want {
		t.Errorf("plan type: got %q, want %q", got, want)
	}
}

func child0(p ir.LogicalPlan) ir.LogicalPlan {
	ch := p.Children()
	if len(ch) == 0 {
		return nil
	}
	return ch[0]
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 227 — Rule framework + Driver
// ─────────────────────────────────────────────────────────────────────────────

// noopRule applies nothing — used to verify the driver terminates cleanly.
type noopRule struct{}

func (noopRule) Name() string                                  { return "noop" }
func (noopRule) Apply(p ir.LogicalPlan) (ir.LogicalPlan, bool) { return p, false }

// countingRule fires exactly once on the first AllNodesScan it sees.
type countingRule struct{ fired bool }

func (r *countingRule) Name() string { return "counting" }
func (r *countingRule) Apply(p ir.LogicalPlan) (ir.LogicalPlan, bool) {
	if !r.fired {
		if _, ok := p.(*ir.AllNodesScan); ok {
			r.fired = true
			return ir.NewNodeByLabelScan("n", "Person"), true
		}
	}
	return p, false
}

func TestDriverNoRules(t *testing.T) {
	plan := scan("n")
	reg := &rewrite.Registry{}
	d := rewrite.NewDriver(reg)
	out, count := d.Run(context.Background(), plan)
	if count != 0 {
		t.Errorf("applied count = %d, want 0", count)
	}
	assertType(t, out, "AllNodesScan")
}

func TestDriverNoopRule(t *testing.T) {
	plan := scan("n")
	reg := &rewrite.Registry{}
	reg.Register(noopRule{})
	d := rewrite.NewDriver(reg)
	_, count := d.Run(context.Background(), plan)
	if count != 0 {
		t.Errorf("applied count = %d, want 0", count)
	}
}

func TestDriverAppliesCountOnce(t *testing.T) {
	plan := scan("n")
	reg := &rewrite.Registry{}
	cr := &countingRule{}
	reg.Register(cr)
	d := rewrite.NewDriver(reg)
	out, count := d.Run(context.Background(), plan)
	if count == 0 {
		t.Error("expected at least one rule application")
	}
	assertType(t, out, "NodeByLabelScan")
}

func TestDriverDisableRule(t *testing.T) {
	plan := scan("n")
	reg := &rewrite.Registry{}
	cr := &countingRule{}
	reg.Register(cr)
	d := rewrite.NewDriver(reg)

	ctx := rewrite.WithDisabledRules(context.Background(), "counting")
	out, count := d.Run(ctx, plan)
	if count != 0 {
		t.Errorf("applied count = %d, want 0 (rule disabled)", count)
	}
	assertType(t, out, "AllNodesScan")
}

func TestIsDisabled(t *testing.T) {
	ctx := rewrite.WithDisabledRules(context.Background(), "foo", "bar")
	if !rewrite.IsDisabled(ctx, "foo") {
		t.Error("expected foo to be disabled")
	}
	if !rewrite.IsDisabled(ctx, "bar") {
		t.Error("expected bar to be disabled")
	}
	if rewrite.IsDisabled(ctx, "baz") {
		t.Error("expected baz to be enabled")
	}
}

// Fuzzing guard: a rule that always reports changed must still terminate within
// maxIter iterations and not loop forever.
type alwaysFireRule struct{}

func (alwaysFireRule) Name() string { return "alwaysFire" }
func (alwaysFireRule) Apply(p ir.LogicalPlan) (ir.LogicalPlan, bool) {
	// Wrap in a Distinct on every call — terminates because WalkAndReplace is
	// bottom-up and the node shape does not regress. But the driver loop will
	// keep seeing "changed" until maxIter is exhausted.
	return p, true
}

func TestDriverBoundedFixedPoint(t *testing.T) {
	plan := scan("n")
	reg := &rewrite.Registry{}
	reg.Register(alwaysFireRule{})
	d := rewrite.NewDriver(reg)
	// Must return within time budget — if the driver loops infinitely this test
	// will time out and the race detector will report nothing (timeout is the
	// failure signal here).
	_, count := d.Run(context.Background(), plan)
	// 16 iterations × 1 rule = 16 total applications.
	if count != 16 {
		t.Errorf("count = %d, want 16 (maxIter)", count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 228 — Predicate pushdown
// ─────────────────────────────────────────────────────────────────────────────

func TestPredicatePushdownPastProjection(t *testing.T) {
	// Selection(n.age > 18, Projection([n], AllNodesScan(n)))
	// → should push to Projection([n], Selection(n.age > 18, AllNodesScan(n)))
	items := []ir.ProjectionItem{{Name: "n", Expression: "n"}}
	proj := ir.NewProjection(items, scan("n"))
	sel := ir.NewSelection("n.age > 18", proj)

	rule := rewrite.PredicatePushdown{}
	out, changed := rule.Apply(sel)
	if !changed {
		t.Fatal("expected rule to fire")
	}
	assertType(t, out, "Projection")
	assertType(t, child0(out), "Selection")
	assertType(t, child0(child0(out)), "AllNodesScan")
}

// TestPredicatePushdownNotPastLimit is the gate test for task #1381.
// Selection(pred, Limit(n, X)) must NOT be pushed to Limit(n, Selection(pred, X)):
// filter-after-limit ≠ filter-before-limit (different cardinalities in openCypher).
//
// Gate semantics:
//
//	Before fix: Apply returns changed=true (pushdown fired) → test fails (asserts !changed)
//	After fix:  Apply returns changed=false (no pushdown)   → test passes
func TestPredicatePushdownNotPastLimit(t *testing.T) {
	lim := ir.NewLimit(10, scan("n"))
	sel := ir.NewSelection("n.name = 'Alice'", lim)

	rule := rewrite.PredicatePushdown{}
	out, changed := rule.Apply(sel)
	if changed {
		t.Fatal("predicate must NOT be pushed past a Limit barrier (filter-after-limit != filter-before-limit)")
	}
	// Plan must remain Selection(Limit(...)) — unchanged.
	assertType(t, out, "Selection")
	assertType(t, child0(out), "Limit")
}

// TestPredicatePushdownNotPastSkip is the gate test for task #1381.
// Selection(pred, Skip(n, X)) must NOT be pushed to Skip(n, Selection(pred, X)).
//
// Gate semantics:
//
//	Before fix: changed=true → test fails
//	After fix:  changed=false → test passes
func TestPredicatePushdownNotPastSkip(t *testing.T) {
	sk := ir.NewSkip(5, scan("n"))
	sel := ir.NewSelection("n.active = true", sk)

	rule := rewrite.PredicatePushdown{}
	out, changed := rule.Apply(sel)
	if changed {
		t.Fatal("predicate must NOT be pushed past a Skip barrier (filter-after-skip != filter-before-skip)")
	}
	assertType(t, out, "Selection")
	assertType(t, child0(out), "Skip")
}

func TestPredicatePushdownPastSort(t *testing.T) {
	srt := ir.NewSort([]ir.SortItem{{Expression: "n.age"}}, scan("n"))
	sel := ir.NewSelection("n.name IS NOT NULL", srt)

	rule := rewrite.PredicatePushdown{}
	out, changed := rule.Apply(sel)
	if !changed {
		t.Fatal("expected rule to fire")
	}
	assertType(t, out, "Sort")
	assertType(t, child0(out), "Selection")
}

func TestPredicatePushdownNotPastEager(t *testing.T) {
	eager := ir.NewEager(scan("n"))
	sel := ir.NewSelection("n.x > 0", eager)

	rule := rewrite.PredicatePushdown{}
	_, changed := rule.Apply(sel)
	if changed {
		t.Error("predicate must not be pushed past an Eager barrier")
	}
}

func TestPredicatePushdownNotPastProjectionOutOfScope(t *testing.T) {
	// Selection references 'r' which is NOT in the Projection output.
	items := []ir.ProjectionItem{{Name: "n", Expression: "n"}}
	proj := ir.NewProjection(items, scan("n"))
	sel := ir.NewSelection("r.since > 2020", proj)

	rule := rewrite.PredicatePushdown{}
	_, changed := rule.Apply(sel)
	if changed {
		t.Error("predicate referencing out-of-scope var must not be pushed")
	}
}

// TestPredicatePushdownNotPastProjectionAlias is the gate test for task #1429.
//
// The plan is:
//
//	Selection("y > 5", Projection([a.x AS y], AllNodesScan(a)))
//
// The predicate references alias 'y', which is introduced BY the Projection and
// is therefore NOT available in the Projection's input (AllNodesScan outputs
// only 'a'). The rule must guard on child.Child.Vars() (the input vars), not
// child.Vars() (the output vars), so the pushdown is correctly rejected.
//
// Gate semantics:
//
//	Before fix (uses child.Vars()): 'y' is in the projection output → pushdown
//	fires → plan becomes Projection([a.x AS y], Selection("y>5", Scan(a))), but
//	'y' is unbound inside Scan(a) → semantically wrong.
//	After fix (uses child.Child.Vars()): 'y' is NOT in Scan(a).Vars() →
//	pushdown correctly refused → changed=false.
func TestPredicatePushdownNotPastProjectionAlias(t *testing.T) {
	// Build: Selection("y > 5", Projection([a.x AS y], AllNodesScan(a)))
	// Projection introduces alias 'y' from expression 'a.x'.
	items := []ir.ProjectionItem{{Name: "y", Expression: "a.x"}}
	proj := ir.NewProjection(items, scan("a"))
	sel := ir.NewSelection("y > 5", proj)

	rule := rewrite.PredicatePushdown{}
	out, changed := rule.Apply(sel)
	if changed {
		t.Fatalf("predicate on projection-introduced alias 'y' must NOT be pushed below the Projection; got plan: %T", out)
	}
	// Plan shape must remain Selection(Projection(Scan)).
	assertType(t, out, "Selection")
	assertType(t, child0(out), "Projection")
	assertType(t, child0(child0(out)), "AllNodesScan")
}

func TestPredicatePushdownSwapSelections(t *testing.T) {
	// Selection(pred1, Selection(pred2, X)) → Selection(pred2, Selection(pred1, X))
	inner := ir.NewSelection("n.age > 18", scan("n"))
	outer := ir.NewSelection("n.name = 'x'", inner)

	rule := rewrite.PredicatePushdown{}
	out, changed := rule.Apply(outer)
	if !changed {
		t.Fatal("expected rule to fire (selection swap)")
	}
	outerSel, ok := out.(*ir.Selection)
	if !ok {
		t.Fatal("result must be a Selection")
	}
	if outerSel.Predicate != "n.age > 18" {
		t.Errorf("outer predicate = %q, want %q", outerSel.Predicate, "n.age > 18")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 229 — Projection pushdown / dead column elimination
// ─────────────────────────────────────────────────────────────────────────────

func TestProjectionPushdownDeadColumnRemoved(t *testing.T) {
	// ProduceResults([n], Projection([n, r], scan))
	// → ProduceResults([n], Projection([n], scan))
	items := []ir.ProjectionItem{
		{Name: "n", Expression: "n"},
		{Name: "r", Expression: "r"},
	}
	proj := ir.NewProjection(items, scan("n"))
	pr := ir.NewProduceResults([]string{"n"}, proj)

	rule := rewrite.ProjectionPushdown{}
	out, changed := rule.Apply(pr)
	if !changed {
		t.Fatal("expected dead column 'r' to be removed")
	}
	outPR, ok := out.(*ir.ProduceResults)
	if !ok {
		t.Fatal("result must be ProduceResults")
	}
	outProj, ok := outPR.Child.(*ir.Projection)
	if !ok {
		t.Fatal("ProduceResults child must be Projection")
	}
	if len(outProj.Items) != 1 || outProj.Items[0].Name != "n" {
		t.Errorf("projection items = %v, want [{n n}]", outProj.Items)
	}
}

func TestProjectionPushdownAllItemsDead(t *testing.T) {
	// ProduceResults([x], Projection([n, r], scan))
	// → ProduceResults([x], scan)  — all items dead, remove Projection entirely
	items := []ir.ProjectionItem{
		{Name: "n", Expression: "n"},
		{Name: "r", Expression: "r"},
	}
	proj := ir.NewProjection(items, scan("n"))
	pr := ir.NewProduceResults([]string{"x"}, proj)

	rule := rewrite.ProjectionPushdown{}
	out, changed := rule.Apply(pr)
	if !changed {
		t.Fatal("expected all-dead Projection to be removed")
	}
	outPR, ok := out.(*ir.ProduceResults)
	if !ok {
		t.Fatal("result must be ProduceResults")
	}
	assertType(t, outPR.Child, "AllNodesScan")
}

func TestProjectionPushdownNothingDead(t *testing.T) {
	// ProduceResults([n], Projection([n], scan)) — nothing to remove
	items := []ir.ProjectionItem{{Name: "n", Expression: "n"}}
	proj := ir.NewProjection(items, scan("n"))
	pr := ir.NewProduceResults([]string{"n"}, proj)

	rule := rewrite.ProjectionPushdown{}
	_, changed := rule.Apply(pr)
	if changed {
		t.Error("no change expected when all columns are live")
	}
}

func TestProjectionPushdownEmptyProjectionRemoved(t *testing.T) {
	// Projection([], scan) → scan
	proj := ir.NewProjection([]ir.ProjectionItem{}, scan("n"))

	rule := rewrite.ProjectionPushdown{}
	out, changed := rule.Apply(proj)
	if !changed {
		t.Fatal("expected empty Projection to be removed")
	}
	assertType(t, out, "AllNodesScan")
}

func TestProjectionPushdownNoProduceResults(t *testing.T) {
	// Selection above Projection — not handled by this rule
	items := []ir.ProjectionItem{{Name: "n", Expression: "n"}}
	proj := ir.NewProjection(items, scan("n"))
	sel := ir.NewSelection("n.age > 0", proj)

	rule := rewrite.ProjectionPushdown{}
	_, changed := rule.Apply(sel)
	if changed {
		t.Error("ProjectionPushdown must not fire on Selection")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 230 — Eager insertion
// ─────────────────────────────────────────────────────────────────────────────

func TestEagerInsertionMergeAlwaysEager(t *testing.T) {
	// Merge(..., scan("n")) → Merge(..., Eager(scan("n")))
	m := ir.NewMerge("(n:Person)", nil, nil, []string{"n"}, scan("n"))

	rule := rewrite.EagerInsertion{}
	out, changed := rule.Apply(m)
	if !changed {
		t.Fatal("expected Eager to be inserted before Merge")
	}
	outM, ok := out.(*ir.Merge)
	if !ok {
		t.Fatal("result must be Merge")
	}
	assertType(t, outM.Child, "Eager")
}

func TestEagerInsertionMergeIdempotent(t *testing.T) {
	// Merge(..., Eager(scan)) → no change (already eager)
	m := ir.NewMerge("(n:Person)", nil, nil, []string{"n"}, ir.NewEager(scan("n")))

	rule := rewrite.EagerInsertion{}
	_, changed := rule.Apply(m)
	if changed {
		t.Error("Eager must not be double-inserted")
	}
}

func TestEagerInsertionCreateAfterMatch(t *testing.T) {
	// CreateNode(..., scan("n")) → CreateNode(..., Eager(scan("n")))
	cn := ir.NewCreateNode("m", []string{"Person"}, "", scan("n"))

	rule := rewrite.EagerInsertion{}
	out, changed := rule.Apply(cn)
	if !changed {
		t.Fatal("expected Eager before CreateNode when subtree has a scan")
	}
	outCN, ok := out.(*ir.CreateNode)
	if !ok {
		t.Fatal("result must be CreateNode")
	}
	assertType(t, outCN.Child, "Eager")
}

func TestEagerInsertionCreateNoMatch(t *testing.T) {
	// CreateNode with no scan in subtree — no Eager needed.
	// Build a pure-create subtree: Argument leaf (no scan).
	arg := ir.NewArgument([]string{"x"})
	cn := ir.NewCreateNode("m", nil, "", arg)

	rule := rewrite.EagerInsertion{}
	_, changed := rule.Apply(cn)
	if changed {
		t.Error("Eager must not be inserted when subtree has no read operators")
	}
}

func TestEagerInsertionDeleteAfterMatch(t *testing.T) {
	// DeleteNode(..., scan("n")) → DeleteNode(..., Eager(scan("n")))
	dn := ir.NewDeleteNode("n", scan("n"))

	rule := rewrite.EagerInsertion{}
	out, changed := rule.Apply(dn)
	if !changed {
		t.Fatal("expected Eager before DeleteNode when subtree has a scan")
	}
	outDN, ok := out.(*ir.DeleteNode)
	if !ok {
		t.Fatal("result must be DeleteNode")
	}
	assertType(t, outDN.Child, "Eager")
}

func TestEagerInsertionDetachDeleteAfterMatch(t *testing.T) {
	dd := ir.NewDetachDelete("n", labelScan("n", "Person"))

	rule := rewrite.EagerInsertion{}
	out, changed := rule.Apply(dd)
	if !changed {
		t.Fatal("expected Eager before DetachDelete")
	}
	outDD, ok := out.(*ir.DetachDelete)
	if !ok {
		t.Fatal("result must be DetachDelete")
	}
	assertType(t, outDD.Child, "Eager")
}

func TestEagerInsertionDeleteRelAfterMatch(t *testing.T) {
	dr := ir.NewDeleteRelationship("r",
		ir.NewExpand("n", "r", nil, ir.DirectionOutgoing, "m", scan("n")))

	rule := rewrite.EagerInsertion{}
	out, changed := rule.Apply(dr)
	if !changed {
		t.Fatal("expected Eager before DeleteRelationship")
	}
	outDR, ok := out.(*ir.DeleteRelationship)
	if !ok {
		t.Fatal("result must be DeleteRelationship")
	}
	assertType(t, outDR.Child, "Eager")
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 231 — Distinct/Limit fusion + double-projection + redundant distinct
// ─────────────────────────────────────────────────────────────────────────────

func TestFusionDistinctLimitToTop(t *testing.T) {
	// Distinct(Limit(10, scan)) → Top([], 10, scan)
	lim := ir.NewLimit(10, scan("n"))
	dist := ir.NewDistinct(lim)

	rule := rewrite.FusionRules{}
	out, changed := rule.Apply(dist)
	if !changed {
		t.Fatal("expected Distinct+Limit → Top fusion")
	}
	top, ok := out.(*ir.Top)
	if !ok {
		t.Fatalf("result must be Top, got %s", planType(out))
	}
	if top.Limit != 10 {
		t.Errorf("Top.Limit = %d, want 10", top.Limit)
	}
	assertType(t, top.Child, "AllNodesScan")
}

func TestFusionDoubleDistinctRemoved(t *testing.T) {
	// Distinct(Distinct(scan)) → Distinct(scan)
	inner := ir.NewDistinct(scan("n"))
	outer := ir.NewDistinct(inner)

	rule := rewrite.FusionRules{}
	out, changed := rule.Apply(outer)
	if !changed {
		t.Fatal("expected outer Distinct to be removed")
	}
	assertType(t, out, "Distinct")
	assertType(t, child0(out), "AllNodesScan")
}

func TestFusionDistinctOverEagerAggregationRemoved(t *testing.T) {
	// Distinct(EagerAggregation([n], [], scan)) → EagerAggregation(...)
	agg := ir.NewEagerAggregation([]string{"n"}, nil, scan("n"))
	dist := ir.NewDistinct(agg)

	rule := rewrite.FusionRules{}
	out, changed := rule.Apply(dist)
	if !changed {
		t.Fatal("expected Distinct over EagerAggregation to be removed")
	}
	assertType(t, out, "EagerAggregation")
}

func TestFusionDistinctOverTopRemoved(t *testing.T) {
	// Distinct(Top([], 5, scan)) → Top([], 5, scan)
	top := ir.NewTop(nil, 5, scan("n"))
	dist := ir.NewDistinct(top)

	rule := rewrite.FusionRules{}
	out, changed := rule.Apply(dist)
	if !changed {
		t.Fatal("expected Distinct over Top to be removed")
	}
	assertType(t, out, "Top")
}

func TestFusionDoubleProjectionRemoved(t *testing.T) {
	// Projection([n], Projection([n, r], scan)) → Projection([n], scan)
	innerItems := []ir.ProjectionItem{
		{Name: "n", Expression: "n"},
		{Name: "r", Expression: "r"},
	}
	outerItems := []ir.ProjectionItem{
		{Name: "n", Expression: "n"},
	}
	inner := ir.NewProjection(innerItems, scan("n"))
	outer := ir.NewProjection(outerItems, inner)

	rule := rewrite.FusionRules{}
	out, changed := rule.Apply(outer)
	if !changed {
		t.Fatal("expected double Projection to be collapsed")
	}
	outProj, ok := out.(*ir.Projection)
	if !ok {
		t.Fatal("result must be Projection")
	}
	if len(outProj.Items) != 1 || outProj.Items[0].Name != "n" {
		t.Errorf("items = %v, want [{n n}]", outProj.Items)
	}
	assertType(t, outProj.Child, "AllNodesScan")
}

func TestFusionNoFusionWithoutLimit(t *testing.T) {
	// Distinct(AllNodesScan) — no fusion applies
	dist := ir.NewDistinct(scan("n"))

	rule := rewrite.FusionRules{}
	_, changed := rule.Apply(dist)
	if changed {
		t.Error("no fusion expected when child is not a Limit/Distinct/Agg/Top")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Integration: Driver + multiple rules
// ─────────────────────────────────────────────────────────────────────────────

func TestDriverIntegration(t *testing.T) {
	// Build a plan: Distinct(Limit(5, Selection(n.age>18, AllNodesScan(n))))
	// After one pass:
	//   - PredicatePushdown: no-op (nothing to push past Limit except selection
	//     is already at the leaf)
	//   - FusionRules: Distinct(Limit) → Top
	// Final shape: Top([], 5, Selection(n.age>18, AllNodesScan(n)))
	inner := ir.NewSelection("n.age > 18", scan("n"))
	lim := ir.NewLimit(5, inner)
	dist := ir.NewDistinct(lim)

	reg := &rewrite.Registry{}
	reg.Register(rewrite.PredicatePushdown{})
	reg.Register(rewrite.ProjectionPushdown{})
	reg.Register(rewrite.EagerInsertion{})
	reg.Register(rewrite.FusionRules{})

	d := rewrite.NewDriver(reg)
	out, count := d.Run(context.Background(), dist)

	if count == 0 {
		t.Error("expected at least one rule application")
	}
	assertType(t, out, "Top")
	top := out.(*ir.Top)
	if top.Limit != 5 {
		t.Errorf("Top.Limit = %d, want 5", top.Limit)
	}
	assertType(t, top.Child, "Selection")
}

func TestDriverIntegrationEager(t *testing.T) {
	// MATCH (n) CREATE (m) — CreateNode after a scan → Eager inserted
	// Plan: CreateNode("m", [], "", AllNodesScan("n"))
	plan := ir.NewCreateNode("m", nil, "", scan("n"))

	reg := &rewrite.Registry{}
	reg.Register(rewrite.EagerInsertion{})

	d := rewrite.NewDriver(reg)
	out, count := d.Run(context.Background(), plan)
	if count == 0 {
		t.Error("expected Eager insertion")
	}
	cn, ok := out.(*ir.CreateNode)
	if !ok {
		t.Fatal("result must be CreateNode")
	}
	assertType(t, cn.Child, "Eager")
	assertType(t, child0(cn.Child), "AllNodesScan")
}
