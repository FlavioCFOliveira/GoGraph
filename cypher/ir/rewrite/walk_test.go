package rewrite_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir/rewrite"
)

// ─────────────────────────────────────────────────────────────────────────────
// WalkAndReplace — coverage of all operator constructors in replaceChildren
// ─────────────────────────────────────────────────────────────────────────────

// replaceScan is a simple fn that replaces AllNodesScan(v) with
// NodeByLabelScan(v, "X"). Used to trigger the "child changed" path in every
// unary operator.
func replaceScan(p ir.LogicalPlan) (ir.LogicalPlan, bool) {
	if a, ok := p.(*ir.AllNodesScan); ok {
		return ir.NewNodeByLabelScan(a.NodeVar, "X"), true
	}
	return p, false
}

// identity fn — never changes anything.
func identityFn(p ir.LogicalPlan) (ir.LogicalPlan, bool) { return p, false }

func TestWalkNilPlan(t *testing.T) {
	out, changed := rewrite.WalkAndReplace(nil, identityFn)
	if out != nil || changed {
		t.Errorf("expected (nil, false), got (%v, %v)", out, changed)
	}
}

func TestWalkLeafOperators(t *testing.T) {
	leaves := []ir.LogicalPlan{
		ir.NewArgument([]string{"n"}),
		ir.NewAllNodesScan("n"),
		ir.NewNodeByLabelScan("n", "Person"),
		ir.NewNodeByIndexSeek("n", "Person", "name", "Alice"),
		ir.NewNodeByIndexRangeScan("n", "Person", "age",
			&ir.Bound{Value: "18", Inclusive: true}, nil),
	}
	for _, leaf := range leaves {
		out, changed := rewrite.WalkAndReplace(leaf, identityFn)
		if changed {
			t.Errorf("%T: expected no change for identity fn", leaf)
		}
		if out != leaf {
			t.Errorf("%T: output should be same pointer", leaf)
		}
	}
}

func TestWalkExpand(t *testing.T) {
	e := ir.NewExpand("n", "r", []string{"KNOWS"}, ir.DirectionOutgoing, "m", scan("n"))
	out, changed := rewrite.WalkAndReplace(e, replaceScan)
	if !changed {
		t.Fatal("expected change after scan replacement inside Expand")
	}
	exp, ok := out.(*ir.Expand)
	if !ok {
		t.Fatal("result must be Expand")
	}
	assertType(t, exp.Child, "NodeByLabelScan")
}

func TestWalkOptionalExpand(t *testing.T) {
	oe := ir.NewOptionalExpand("n", "r", nil, ir.DirectionBoth, "m", scan("n"))
	out, changed := rewrite.WalkAndReplace(oe, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	oexp, ok := out.(*ir.OptionalExpand)
	if !ok {
		t.Fatal("result must be OptionalExpand")
	}
	assertType(t, oexp.Child, "NodeByLabelScan")
}

func TestWalkVarLengthExpand(t *testing.T) {
	vle := ir.NewVarLengthExpand("n", "r", nil, ir.DirectionIncoming, "m", 1, 3, scan("n"))
	out, changed := rewrite.WalkAndReplace(vle, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	v, ok := out.(*ir.VarLengthExpand)
	if !ok {
		t.Fatal("result must be VarLengthExpand")
	}
	assertType(t, v.Child, "NodeByLabelScan")
}

func TestWalkProjectEndpoints(t *testing.T) {
	pe := ir.NewProjectEndpoints("r", "s", "e", scan("n"))
	out, changed := rewrite.WalkAndReplace(pe, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	p, ok := out.(*ir.ProjectEndpoints)
	if !ok {
		t.Fatal("result must be ProjectEndpoints")
	}
	assertType(t, p.Child, "NodeByLabelScan")
}

func TestWalkSelection(t *testing.T) {
	sel := ir.NewSelection("n.x > 0", scan("n"))
	out, changed := rewrite.WalkAndReplace(sel, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Selection")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkProjection(t *testing.T) {
	items := []ir.ProjectionItem{{Name: "n", Expression: "n"}}
	proj := ir.NewProjection(items, scan("n"))
	out, changed := rewrite.WalkAndReplace(proj, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Projection")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkEagerAggregation(t *testing.T) {
	agg := ir.NewEagerAggregation([]string{"n"}, nil, scan("n"))
	out, changed := rewrite.WalkAndReplace(agg, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "EagerAggregation")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkSort(t *testing.T) {
	srt := ir.NewSort([]ir.SortItem{{Expression: "n.age"}}, scan("n"))
	out, changed := rewrite.WalkAndReplace(srt, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Sort")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkTop(t *testing.T) {
	top := ir.NewTop([]ir.SortItem{{Expression: "n.age"}}, 10, scan("n"))
	out, changed := rewrite.WalkAndReplace(top, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Top")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkLimit(t *testing.T) {
	lim := ir.NewLimit(5, scan("n"))
	out, changed := rewrite.WalkAndReplace(lim, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Limit")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkSkip(t *testing.T) {
	sk := ir.NewSkip(2, scan("n"))
	out, changed := rewrite.WalkAndReplace(sk, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Skip")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkDistinct(t *testing.T) {
	d := ir.NewDistinct(scan("n"))
	out, changed := rewrite.WalkAndReplace(d, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Distinct")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkEager(t *testing.T) {
	e := ir.NewEager(scan("n"))
	out, changed := rewrite.WalkAndReplace(e, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Eager")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkUnwindWithChild(t *testing.T) {
	u := ir.NewUnwind("[1,2,3]", "x", scan("n"))
	out, changed := rewrite.WalkAndReplace(u, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Unwind")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkUnwindNilChild(t *testing.T) {
	u := ir.NewUnwind("[1,2,3]", "x", nil)
	out, changed := rewrite.WalkAndReplace(u, replaceScan)
	if changed {
		t.Error("nil-child Unwind should not change")
	}
	if out != u {
		t.Error("output should be same pointer")
	}
}

func TestWalkProduceResults(t *testing.T) {
	pr := ir.NewProduceResults([]string{"n"}, scan("n"))
	out, changed := rewrite.WalkAndReplace(pr, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "ProduceResults")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkCreateNode(t *testing.T) {
	cn := ir.NewCreateNode("m", []string{"L"}, "", scan("n"))
	out, changed := rewrite.WalkAndReplace(cn, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "CreateNode")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkCreateRelationship(t *testing.T) {
	cr := ir.NewCreateRelationship("a", "b", "r", "R", "", scan("n"))
	out, changed := rewrite.WalkAndReplace(cr, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "CreateRelationship")
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkSetProperty(t *testing.T) {
	sp := ir.NewSetProperty("n", "name", "'x'", scan("n"))
	out, changed := rewrite.WalkAndReplace(sp, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkSetLabels(t *testing.T) {
	sl := ir.NewSetLabels("n", []string{"L"}, scan("n"))
	out, changed := rewrite.WalkAndReplace(sl, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkRemoveProperty(t *testing.T) {
	rp := ir.NewRemoveProperty("n", "age", scan("n"))
	out, changed := rewrite.WalkAndReplace(rp, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkRemoveLabels(t *testing.T) {
	rl := ir.NewRemoveLabels("n", []string{"L"}, scan("n"))
	out, changed := rewrite.WalkAndReplace(rl, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkDeleteNode(t *testing.T) {
	dn := ir.NewDeleteNode("n", scan("n"))
	out, changed := rewrite.WalkAndReplace(dn, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkDeleteRelationship(t *testing.T) {
	dr := ir.NewDeleteRelationship("r", scan("n"))
	out, changed := rewrite.WalkAndReplace(dr, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkDetachDelete(t *testing.T) {
	dd := ir.NewDetachDelete("n", scan("n"))
	out, changed := rewrite.WalkAndReplace(dd, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkMerge(t *testing.T) {
	m := ir.NewMerge("(n)", nil, nil, []string{"n"}, scan("n"))
	out, changed := rewrite.WalkAndReplace(m, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkUnion(t *testing.T) {
	u := ir.NewUnion(scan("a"), scan("b"))
	out, changed := rewrite.WalkAndReplace(u, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Union")
}

func TestWalkUnionAll(t *testing.T) {
	u := ir.NewUnionAll(scan("a"), scan("b"))
	out, changed := rewrite.WalkAndReplace(u, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "UnionAll")
}

func TestWalkApply(t *testing.T) {
	a := ir.NewApply(scan("a"), scan("b"))
	out, changed := rewrite.WalkAndReplace(a, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "Apply")
}

func TestWalkSemiApply(t *testing.T) {
	sa := ir.NewSemiApply(scan("a"), scan("b"))
	out, changed := rewrite.WalkAndReplace(sa, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "SemiApply")
}

func TestWalkAntiSemiApply(t *testing.T) {
	asa := ir.NewAntiSemiApply(scan("a"), scan("b"))
	out, changed := rewrite.WalkAndReplace(asa, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "AntiSemiApply")
}

func TestWalkRollUpApply(t *testing.T) {
	rua := ir.NewRollUpApply(scan("a"), scan("b"), "list")
	out, changed := rewrite.WalkAndReplace(rua, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, out, "RollUpApply")
}

func TestWalkProcedureCallWithChild(t *testing.T) {
	pc := ir.NewProcedureCall(nil, "myProc", nil, []string{"x"}, scan("n"))
	out, changed := rewrite.WalkAndReplace(pc, replaceScan)
	if !changed {
		t.Fatal("expected change")
	}
	assertType(t, child0(out), "NodeByLabelScan")
}

func TestWalkProcedureCallNilChild(t *testing.T) {
	pc := ir.NewProcedureCall(nil, "myProc", nil, []string{"x"}, nil)
	out, changed := rewrite.WalkAndReplace(pc, identityFn)
	if changed {
		t.Error("nil-child ProcedureCall should not change")
	}
	if out != pc {
		t.Error("output should be same pointer")
	}
}

// Verify no change is reported when the fn doesn't fire on non-leaf nodes
// that haven't had their children changed.
func TestWalkNoChangeWhenChildUnchanged(t *testing.T) {
	// fn only fires on NodeByLabelScan; AllNodesScan won't match.
	fn := func(p ir.LogicalPlan) (ir.LogicalPlan, bool) {
		if _, ok := p.(*ir.NodeByLabelScan); ok {
			return ir.NewAllNodesScan("replaced"), true
		}
		return p, false
	}
	lim := ir.NewLimit(5, scan("n")) // scan("n") = AllNodesScan, not NodeByLabelScan
	out, changed := rewrite.WalkAndReplace(lim, fn)
	if changed {
		t.Error("expected no change")
	}
	if out != lim {
		t.Error("output should be same pointer")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Additional Apply-type: no-change path when left unchanged
// ─────────────────────────────────────────────────────────────────────────────

func TestWalkApplyNoChange(t *testing.T) {
	a := ir.NewApply(
		ir.NewNodeByLabelScan("a", "A"),
		ir.NewNodeByLabelScan("b", "B"),
	)
	out, changed := rewrite.WalkAndReplace(a, identityFn)
	if changed {
		t.Error("expected no change")
	}
	if out != a {
		t.Error("output should be same pointer")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Registry.Rules returns registered rules in order
// ─────────────────────────────────────────────────────────────────────────────

func TestRegistryOrder(t *testing.T) {
	reg := &rewrite.Registry{}
	reg.Register(noopRule{})
	reg.Register(rewrite.PredicatePushdown{})
	rules := reg.Rules()
	if len(rules) != 2 {
		t.Fatalf("len(Rules) = %d, want 2", len(rules))
	}
	if rules[0].Name() != "noop" {
		t.Errorf("rules[0].Name() = %q, want %q", rules[0].Name(), "noop")
	}
	if rules[1].Name() != "PredicatePushdown" {
		t.Errorf("rules[1].Name() = %q, want %q", rules[1].Name(), "PredicatePushdown")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Additional eager tests for branch coverage
// ─────────────────────────────────────────────────────────────────────────────

func TestEagerInsertionCreateRelAfterMatch(t *testing.T) {
	cr := ir.NewCreateRelationship("a", "b", "r", "R", "",
		ir.NewExpand("a", "e", nil, ir.DirectionOutgoing, "b", scan("a")))

	rule := rewrite.EagerInsertion{}
	out, changed := rule.Apply(cr)
	if !changed {
		t.Fatal("expected Eager before CreateRelationship")
	}
	outCR, ok := out.(*ir.CreateRelationship)
	if !ok {
		t.Fatal("result must be CreateRelationship")
	}
	assertType(t, outCR.Child, "Eager")
}

func TestEagerInsertionCreateRelIdempotent(t *testing.T) {
	cr := ir.NewCreateRelationship("a", "b", "r", "R", "",
		ir.NewEager(scan("a")))

	rule := rewrite.EagerInsertion{}
	_, changed := rule.Apply(cr)
	if changed {
		t.Error("Eager must not be double-inserted for CreateRelationship")
	}
}

func TestEagerInsertionDeleteNodeIdempotent(t *testing.T) {
	dn := ir.NewDeleteNode("n", ir.NewEager(scan("n")))
	rule := rewrite.EagerInsertion{}
	_, changed := rule.Apply(dn)
	if changed {
		t.Error("Eager must not be double-inserted for DeleteNode")
	}
}

func TestEagerInsertionDetachDeleteIdempotent(t *testing.T) {
	dd := ir.NewDetachDelete("n", ir.NewEager(scan("n")))
	rule := rewrite.EagerInsertion{}
	_, changed := rule.Apply(dd)
	if changed {
		t.Error("Eager must not be double-inserted for DetachDelete")
	}
}

func TestEagerInsertionDeleteRelIdempotent(t *testing.T) {
	dr := ir.NewDeleteRelationship("r", ir.NewEager(scan("n")))
	rule := rewrite.EagerInsertion{}
	_, changed := rule.Apply(dr)
	if changed {
		t.Error("Eager must not be double-inserted for DeleteRelationship")
	}
}

func TestEagerInsertionDeleteRelNoScan(t *testing.T) {
	dr := ir.NewDeleteRelationship("r", ir.NewArgument([]string{"r"}))
	rule := rewrite.EagerInsertion{}
	_, changed := rule.Apply(dr)
	if changed {
		t.Error("Eager must not be inserted when no read in subtree")
	}
}

func TestEagerInsertionDetachDeleteNoScan(t *testing.T) {
	dd := ir.NewDetachDelete("n", ir.NewArgument([]string{"n"}))
	rule := rewrite.EagerInsertion{}
	_, changed := rule.Apply(dd)
	if changed {
		t.Error("Eager must not be inserted when no read in subtree")
	}
}

func TestEagerInsertionNonWriteOp(t *testing.T) {
	sel := ir.NewSelection("n.x > 0", scan("n"))
	rule := rewrite.EagerInsertion{}
	_, changed := rule.Apply(sel)
	if changed {
		t.Error("EagerInsertion must not fire on non-write operators")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Additional projection pushdown tests
// ─────────────────────────────────────────────────────────────────────────────

func TestFusionDoubleProjectionNoFusion(t *testing.T) {
	// Outer item has a computed expression (not equal to Name) and Name is not
	// in the inner's output: bail out.
	innerItems := []ir.ProjectionItem{{Name: "n", Expression: "n"}}
	outerItems := []ir.ProjectionItem{
		{Name: "computed", Expression: "n.age + 1"},
	}
	inner := ir.NewProjection(innerItems, scan("n"))
	outer := ir.NewProjection(outerItems, inner)

	rule := rewrite.FusionRules{}
	_, changed := rule.Apply(outer)
	if changed {
		t.Error("fusion must not fire when outer item not in inner scope")
	}
}

func TestProjectionPushdownNoChildProjection(t *testing.T) {
	// ProduceResults whose child is not a Projection: no change.
	pr := ir.NewProduceResults([]string{"n"}, scan("n"))
	rule := rewrite.ProjectionPushdown{}
	_, changed := rule.Apply(pr)
	if changed {
		t.Error("no change expected when child is not a Projection")
	}
}
