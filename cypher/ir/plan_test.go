package ir_test

import (
	"reflect"
	"testing"

	"gograph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// equalVars reports whether got and want contain exactly the same elements,
// accounting for nil/empty equivalence in slices returned by leaf operators.
func equalVars(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

func assertVars(t *testing.T, plan ir.LogicalPlan, want []string) {
	t.Helper()
	got := plan.Vars()
	if !equalVars(got, want) {
		t.Errorf("Vars() = %v, want %v", got, want)
	}
}

func assertNoChildren(t *testing.T, plan ir.LogicalPlan) {
	t.Helper()
	if len(plan.Children()) != 0 {
		t.Errorf("Children() = %v, want nil/empty", plan.Children())
	}
}

func assertOneChild(t *testing.T, plan, child ir.LogicalPlan) {
	t.Helper()
	ch := plan.Children()
	if len(ch) != 1 {
		t.Fatalf("len(Children()) = %d, want 1", len(ch))
	}
	if ch[0] != child {
		t.Errorf("Children()[0] is not the expected child")
	}
}

func assertTwoChildren(t *testing.T, plan, left, right ir.LogicalPlan) {
	t.Helper()
	ch := plan.Children()
	if len(ch) != 2 {
		t.Fatalf("len(Children()) = %d, want 2", len(ch))
	}
	if ch[0] != left {
		t.Errorf("Children()[0] is not the expected left child")
	}
	if ch[1] != right {
		t.Errorf("Children()[1] is not the expected right child")
	}
}

// leafScan is a minimal leaf used as child in operator tests.
func leafScan(v string) ir.LogicalPlan {
	return ir.NewAllNodesScan(v)
}

// ─────────────────────────────────────────────────────────────────────────────
// Scan operators
// ─────────────────────────────────────────────────────────────────────────────

func TestArgument(t *testing.T) {
	t.Run("empty vars", func(t *testing.T) {
		a := ir.NewArgument(nil)
		assertNoChildren(t, a)
		assertVars(t, a, nil)
	})
	t.Run("with vars", func(t *testing.T) {
		a := ir.NewArgument([]string{"n", "r"})
		assertNoChildren(t, a)
		assertVars(t, a, []string{"n", "r"})
	})
	t.Run("deep equal round-trip", func(t *testing.T) {
		want := ir.NewArgument([]string{"x"})
		got := ir.NewArgument([]string{"x"})
		if !reflect.DeepEqual(want, got) {
			t.Errorf("DeepEqual mismatch: %v vs %v", want, got)
		}
	})
	t.Run("constructor copies slice", func(t *testing.T) {
		src := []string{"a"}
		a := ir.NewArgument(src)
		src[0] = "mutated"
		if a.Variables[0] != "a" {
			t.Error("constructor did not copy input slice")
		}
	})
}

func TestAllNodesScan(t *testing.T) {
	s := ir.NewAllNodesScan("n")
	assertNoChildren(t, s)
	assertVars(t, s, []string{"n"})

	want := ir.NewAllNodesScan("n")
	got := ir.NewAllNodesScan("n")
	if !reflect.DeepEqual(want, got) {
		t.Error("DeepEqual mismatch")
	}
}

func TestNodeByLabelScan(t *testing.T) {
	s := ir.NewNodeByLabelScan("n", "Person")
	assertNoChildren(t, s)
	assertVars(t, s, []string{"n"})
	if s.Label != "Person" {
		t.Errorf("Label = %q, want %q", s.Label, "Person")
	}

	want := ir.NewNodeByLabelScan("n", "Person")
	got := ir.NewNodeByLabelScan("n", "Person")
	if !reflect.DeepEqual(want, got) {
		t.Error("DeepEqual mismatch")
	}
}

func TestNodeByIndexSeek(t *testing.T) {
	s := ir.NewNodeByIndexSeek("n", "Person", "name", "'Alice'")
	assertNoChildren(t, s)
	assertVars(t, s, []string{"n"})
	if s.Property != "name" || s.Value != "'Alice'" {
		t.Errorf("unexpected fields: property=%q value=%q", s.Property, s.Value)
	}

	want := ir.NewNodeByIndexSeek("n", "Person", "name", "'Alice'")
	got := ir.NewNodeByIndexSeek("n", "Person", "name", "'Alice'")
	if !reflect.DeepEqual(want, got) {
		t.Error("DeepEqual mismatch")
	}
}

func TestNodeByIndexRangeScan(t *testing.T) {
	lower := &ir.Bound{Value: "18", Inclusive: true}
	upper := &ir.Bound{Value: "65", Inclusive: false}
	s := ir.NewNodeByIndexRangeScan("n", "Person", "age", lower, upper)
	assertNoChildren(t, s)
	assertVars(t, s, []string{"n"})
	if s.Min.Value != "18" || s.Max.Value != "65" {
		t.Errorf("unexpected bounds: min=%v max=%v", s.Min, s.Max)
	}

	t.Run("open lower bound", func(t *testing.T) {
		s2 := ir.NewNodeByIndexRangeScan("n", "Person", "age", nil, upper)
		if s2.Min != nil {
			t.Error("Min should be nil for open lower bound")
		}
	})

	want := ir.NewNodeByIndexRangeScan("n", "Person", "age",
		&ir.Bound{Value: "18", Inclusive: true},
		&ir.Bound{Value: "65", Inclusive: false},
	)
	got := ir.NewNodeByIndexRangeScan("n", "Person", "age",
		&ir.Bound{Value: "18", Inclusive: true},
		&ir.Bound{Value: "65", Inclusive: false},
	)
	if !reflect.DeepEqual(want, got) {
		t.Error("DeepEqual mismatch")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Traversal operators
// ─────────────────────────────────────────────────────────────────────────────

func TestExpand(t *testing.T) {
	child := leafScan("n")
	e := ir.NewExpand("n", "r", []string{"KNOWS"}, ir.DirectionOutgoing, "m", child)
	assertOneChild(t, e, child)
	assertVars(t, e, []string{"r", "m"})
	if e.Direction != ir.DirectionOutgoing {
		t.Errorf("Direction = %v, want OutGoing", e.Direction)
	}

	t.Run("copies relTypes slice", func(t *testing.T) {
		types := []string{"KNOWS"}
		e2 := ir.NewExpand("n", "r", types, ir.DirectionBoth, "m", child)
		types[0] = "MUTATED"
		if e2.RelTypes[0] != "KNOWS" {
			t.Error("constructor did not copy relTypes slice")
		}
	})

	t.Run("deep equal", func(t *testing.T) {
		want := ir.NewExpand("n", "r", []string{"KNOWS"}, ir.DirectionOutgoing, "m", ir.NewAllNodesScan("n"))
		got := ir.NewExpand("n", "r", []string{"KNOWS"}, ir.DirectionOutgoing, "m", ir.NewAllNodesScan("n"))
		if !reflect.DeepEqual(want, got) {
			t.Error("DeepEqual mismatch")
		}
	})
}

func TestOptionalExpand(t *testing.T) {
	child := leafScan("n")
	e := ir.NewOptionalExpand("n", "r", []string{"LIKES"}, ir.DirectionIncoming, "m", child)
	assertOneChild(t, e, child)
	assertVars(t, e, []string{"r", "m"})
}

func TestVarLengthExpand(t *testing.T) {
	child := leafScan("n")
	e := ir.NewVarLengthExpand("n", "rels", []string{"KNOWS"}, ir.DirectionBoth, "m", 1, 3, child)
	assertOneChild(t, e, child)
	assertVars(t, e, []string{"rels", "m"})
	if e.MinDepth != 1 || e.MaxDepth != 3 {
		t.Errorf("depths = %d/%d, want 1/3", e.MinDepth, e.MaxDepth)
	}

	t.Run("deep equal", func(t *testing.T) {
		c := ir.NewAllNodesScan("n")
		want := ir.NewVarLengthExpand("n", "rels", []string{"KNOWS"}, ir.DirectionBoth, "m", 1, 3, c)
		got := ir.NewVarLengthExpand("n", "rels", []string{"KNOWS"}, ir.DirectionBoth, "m", 1, 3, c)
		if !reflect.DeepEqual(want, got) {
			t.Error("DeepEqual mismatch")
		}
	})
}

func TestProjectEndpoints(t *testing.T) {
	child := leafScan("r")
	p := ir.NewProjectEndpoints("r", "start", "end", child)
	assertOneChild(t, p, child)
	assertVars(t, p, []string{"start", "end"})

	t.Run("only start var", func(t *testing.T) {
		p2 := ir.NewProjectEndpoints("r", "s", "", child)
		assertVars(t, p2, []string{"s"})
	})

	t.Run("only end var", func(t *testing.T) {
		p3 := ir.NewProjectEndpoints("r", "", "e", child)
		assertVars(t, p3, []string{"e"})
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Filter / projection operators
// ─────────────────────────────────────────────────────────────────────────────

func TestSelection(t *testing.T) {
	child := leafScan("n")
	s := ir.NewSelection("n.age > 18", child)
	assertOneChild(t, s, child)
	// Selection passes through child vars.
	assertVars(t, s, child.Vars())

	t.Run("deep equal", func(t *testing.T) {
		c := ir.NewAllNodesScan("n")
		want := ir.NewSelection("n.age > 18", c)
		got := ir.NewSelection("n.age > 18", c)
		if !reflect.DeepEqual(want, got) {
			t.Error("DeepEqual mismatch")
		}
	})
}

func TestProjection(t *testing.T) {
	child := leafScan("n")
	items := []ir.ProjectionItem{
		{Name: "name", Expression: "n.name"},
		{Name: "age", Expression: "n.age"},
	}
	p := ir.NewProjection(items, child)
	assertOneChild(t, p, child)
	assertVars(t, p, []string{"name", "age"})

	t.Run("copies items slice", func(t *testing.T) {
		src := []ir.ProjectionItem{{Name: "x", Expression: "n.x"}}
		p2 := ir.NewProjection(src, child)
		src[0].Name = "mutated"
		if p2.Items[0].Name != "x" {
			t.Error("constructor did not copy items slice")
		}
	})

	t.Run("deep equal", func(t *testing.T) {
		c := ir.NewAllNodesScan("n")
		want := ir.NewProjection(items, c)
		got := ir.NewProjection(items, c)
		if !reflect.DeepEqual(want, got) {
			t.Error("DeepEqual mismatch")
		}
	})
}

func TestEagerAggregation(t *testing.T) {
	child := leafScan("n")
	groupBy := []string{"n.city"}
	aggs := []ir.AggregateExpr{
		{OutputName: "cnt", Function: "count", Argument: "n", Distinct: false},
		{OutputName: "total", Function: "sum", Argument: "n.salary"},
	}
	e := ir.NewEagerAggregation(groupBy, aggs, child)
	assertOneChild(t, e, child)
	assertVars(t, e, []string{"n.city", "cnt", "total"})

	t.Run("deep equal", func(t *testing.T) {
		c := ir.NewAllNodesScan("n")
		want := ir.NewEagerAggregation(groupBy, aggs, c)
		got := ir.NewEagerAggregation(groupBy, aggs, c)
		if !reflect.DeepEqual(want, got) {
			t.Error("DeepEqual mismatch")
		}
	})
}

func TestSort(t *testing.T) {
	child := leafScan("n")
	items := []ir.SortItem{
		{Expression: "n.name", Descending: false},
		{Expression: "n.age", Descending: true},
	}
	s := ir.NewSort(items, child)
	assertOneChild(t, s, child)
	assertVars(t, s, child.Vars())
}

func TestTop(t *testing.T) {
	child := leafScan("n")
	items := []ir.SortItem{{Expression: "n.name"}}
	top := ir.NewTop(items, 10, child)
	assertOneChild(t, top, child)
	assertVars(t, top, child.Vars())
	if top.Limit != 10 {
		t.Errorf("Limit = %d, want 10", top.Limit)
	}
}

func TestLimit(t *testing.T) {
	child := leafScan("n")
	l := ir.NewLimit(25, child)
	assertOneChild(t, l, child)
	assertVars(t, l, child.Vars())
}

func TestSkip(t *testing.T) {
	child := leafScan("n")
	s := ir.NewSkip(5, child)
	assertOneChild(t, s, child)
	assertVars(t, s, child.Vars())
}

func TestDistinct(t *testing.T) {
	child := leafScan("n")
	d := ir.NewDistinct(child)
	assertOneChild(t, d, child)
	assertVars(t, d, child.Vars())
}

// ─────────────────────────────────────────────────────────────────────────────
// Set operators
// ─────────────────────────────────────────────────────────────────────────────

func TestUnion(t *testing.T) {
	left := leafScan("n")
	right := leafScan("n")
	u := ir.NewUnion(left, right)
	assertTwoChildren(t, u, left, right)
	assertVars(t, u, []string{"n"})

	t.Run("deep equal", func(t *testing.T) {
		l := ir.NewAllNodesScan("n")
		r := ir.NewAllNodesScan("n")
		want := ir.NewUnion(l, r)
		got := ir.NewUnion(l, r)
		if !reflect.DeepEqual(want, got) {
			t.Error("DeepEqual mismatch")
		}
	})
}

func TestUnionAll(t *testing.T) {
	left := leafScan("n")
	right := leafScan("n")
	u := ir.NewUnionAll(left, right)
	assertTwoChildren(t, u, left, right)
	assertVars(t, u, []string{"n"})
}

// ─────────────────────────────────────────────────────────────────────────────
// Apply-family operators
// ─────────────────────────────────────────────────────────────────────────────

func TestApply(t *testing.T) {
	outer := leafScan("n")
	arg := ir.NewArgument([]string{"n"})
	inner := ir.NewExpand("n", "r", nil, ir.DirectionOutgoing, "m", arg)
	a := ir.NewApply(outer, inner)
	assertTwoChildren(t, a, outer, inner)
	// Vars should include both outer and inner variables without duplicates.
	vars := a.Vars()
	if len(vars) == 0 {
		t.Error("Vars() returned empty slice")
	}
	// "n" must appear exactly once.
	count := 0
	for _, v := range vars {
		if v == "n" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("variable %q appears %d times in Vars(), want 1", "n", count)
	}
}

func TestSemiApply(t *testing.T) {
	outer := leafScan("n")
	inner := leafScan("m")
	s := ir.NewSemiApply(outer, inner)
	assertTwoChildren(t, s, outer, inner)
	// Only outer vars are exposed.
	assertVars(t, s, outer.Vars())
}

func TestAntiSemiApply(t *testing.T) {
	outer := leafScan("n")
	inner := leafScan("m")
	a := ir.NewAntiSemiApply(outer, inner)
	assertTwoChildren(t, a, outer, inner)
	assertVars(t, a, outer.Vars())
}

func TestRollUpApply(t *testing.T) {
	outer := leafScan("n")
	inner := leafScan("m")
	r := ir.NewRollUpApply(outer, inner, "collected")
	assertTwoChildren(t, r, outer, inner)
	vars := r.Vars()
	hasN := false
	hasColl := false
	for _, v := range vars {
		if v == "n" {
			hasN = true
		}
		if v == "collected" {
			hasColl = true
		}
	}
	if !hasN || !hasColl {
		t.Errorf("Vars() = %v, must contain 'n' and 'collected'", vars)
	}

	t.Run("no duplicate outer vars", func(t *testing.T) {
		// Outer with two vars should not produce duplicates.
		outerScan := ir.NewArgument([]string{"n", "n"})
		r2 := ir.NewRollUpApply(outerScan, inner, "c")
		seen := make(map[string]int)
		for _, v := range r2.Vars() {
			seen[v]++
		}
		for v, cnt := range seen {
			if cnt > 1 {
				t.Errorf("variable %q appears %d times in RollUpApply.Vars()", v, cnt)
			}
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Pipeline operators
// ─────────────────────────────────────────────────────────────────────────────

func TestEager(t *testing.T) {
	child := leafScan("n")
	e := ir.NewEager(child)
	assertOneChild(t, e, child)
	assertVars(t, e, child.Vars())
}

func TestUnwind(t *testing.T) {
	child := leafScan("n")
	u := ir.NewUnwind("[1,2,3]", "x", child)
	assertOneChild(t, u, child)
	assertVars(t, u, []string{"x"})

	t.Run("nil child", func(t *testing.T) {
		u2 := ir.NewUnwind("[1,2,3]", "x", nil)
		if len(u2.Children()) != 0 {
			t.Error("Unwind with nil child should return nil/empty Children()")
		}
		assertVars(t, u2, []string{"x"})
	})
}

func TestProduceResults(t *testing.T) {
	child := leafScan("n")
	p := ir.NewProduceResults([]string{"name", "age"}, child)
	assertOneChild(t, p, child)
	assertVars(t, p, []string{"name", "age"})

	t.Run("copies columns", func(t *testing.T) {
		cols := []string{"a"}
		p2 := ir.NewProduceResults(cols, child)
		cols[0] = "mutated"
		if p2.Columns[0] != "a" {
			t.Error("constructor did not copy columns slice")
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Write operators
// ─────────────────────────────────────────────────────────────────────────────

func TestCreateNode(t *testing.T) {
	child := leafScan("_")
	c := ir.NewCreateNode("n", []string{"Person"}, "{name: 'Alice'}", child)
	assertOneChild(t, c, child)
	assertVars(t, c, []string{"n"})
	if len(c.Labels) != 1 || c.Labels[0] != "Person" {
		t.Errorf("Labels = %v", c.Labels)
	}

	t.Run("copies labels", func(t *testing.T) {
		lb := []string{"X"}
		c2 := ir.NewCreateNode("n", lb, "", child)
		lb[0] = "mutated"
		if c2.Labels[0] != "X" {
			t.Error("constructor did not copy labels")
		}
	})
}

func TestCreateRelationship(t *testing.T) {
	child := leafScan("n")
	c := ir.NewCreateRelationship("n", "m", "r", "KNOWS", "{since: 2020}", child)
	assertOneChild(t, c, child)
	assertVars(t, c, []string{"r"})
	if c.RelType != "KNOWS" {
		t.Errorf("RelType = %q", c.RelType)
	}
}

func TestSetProperty(t *testing.T) {
	child := leafScan("n")
	s := ir.NewSetProperty("n", "age", "30", child)
	assertOneChild(t, s, child)
	assertVars(t, s, []string{"n"})
}

func TestSetLabels(t *testing.T) {
	child := leafScan("n")
	s := ir.NewSetLabels("n", []string{"Admin", "Staff"}, child)
	assertOneChild(t, s, child)
	assertVars(t, s, []string{"n"})
	if len(s.Labels) != 2 {
		t.Errorf("Labels = %v", s.Labels)
	}
}

func TestRemoveProperty(t *testing.T) {
	child := leafScan("n")
	r := ir.NewRemoveProperty("n", "tempField", child)
	assertOneChild(t, r, child)
	assertVars(t, r, []string{"n"})
}

func TestRemoveLabels(t *testing.T) {
	child := leafScan("n")
	r := ir.NewRemoveLabels("n", []string{"Inactive"}, child)
	assertOneChild(t, r, child)
	assertVars(t, r, []string{"n"})
}

func TestDeleteNode(t *testing.T) {
	child := leafScan("n")
	d := ir.NewDeleteNode("n", child)
	assertOneChild(t, d, child)
	assertVars(t, d, []string{"n"})
}

func TestDeleteRelationship(t *testing.T) {
	child := leafScan("r")
	d := ir.NewDeleteRelationship("r", child)
	assertOneChild(t, d, child)
	assertVars(t, d, []string{"r"})
}

func TestDetachDelete(t *testing.T) {
	child := leafScan("n")
	d := ir.NewDetachDelete("n", child)
	assertOneChild(t, d, child)
	assertVars(t, d, []string{"n"})
}

func TestMerge(t *testing.T) {
	child := leafScan("_")
	m := ir.NewMerge(
		"(n:Person {name: 'Alice'})",
		[]string{"SET n.created = timestamp()"},
		[]string{"SET n.updated = timestamp()"},
		[]string{"n"},
		child,
	)
	assertOneChild(t, m, child)
	assertVars(t, m, []string{"n"})
	if len(m.OnCreate) != 1 || len(m.OnMatch) != 1 {
		t.Errorf("OnCreate=%v OnMatch=%v", m.OnCreate, m.OnMatch)
	}

	t.Run("copies slices", func(t *testing.T) {
		oc := []string{"x"}
		om := []string{"y"}
		bv := []string{"n"}
		m2 := ir.NewMerge("pat", oc, om, bv, child)
		oc[0], om[0], bv[0] = "mut", "mut", "mut"
		if m2.OnCreate[0] != "x" || m2.OnMatch[0] != "y" || m2.BoundVars[0] != "n" {
			t.Error("constructor did not copy slices")
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Procedure call
// ─────────────────────────────────────────────────────────────────────────────

func TestProcedureCall(t *testing.T) {
	child := leafScan("n")
	p := ir.NewProcedureCall(
		[]string{"apoc", "algo"},
		"dijkstra",
		[]string{"n", "m", "KNOWS"},
		[]string{"path", "weight"},
		child,
	)
	assertOneChild(t, p, child)
	assertVars(t, p, []string{"path", "weight"})
	if p.Name != "dijkstra" {
		t.Errorf("Name = %q", p.Name)
	}

	t.Run("nil child", func(t *testing.T) {
		p2 := ir.NewProcedureCall(nil, "myProc", nil, []string{"col"}, nil)
		if len(p2.Children()) != 0 {
			t.Error("ProcedureCall with nil child should return nil/empty Children()")
		}
	})

	t.Run("copies slices", func(t *testing.T) {
		ns := []string{"a"}
		args := []string{"x"}
		yv := []string{"out"}
		p3 := ir.NewProcedureCall(ns, "proc", args, yv, nil)
		ns[0], args[0], yv[0] = "mut", "mut", "mut"
		if p3.Namespace[0] != "a" || p3.Arguments[0] != "x" || p3.YieldVars[0] != "out" {
			t.Error("constructor did not copy slices")
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Structural deep-equality on a hand-built plan
// ─────────────────────────────────────────────────────────────────────────────

// buildSamplePlan constructs a representative multi-level logical plan tree:
//
//	ProduceResults(["name","cnt"])
//	  └─ EagerAggregation(groupBy=["name"], aggs=[count(n)])
//	       └─ Selection("n.active = true")
//	            └─ NodeByLabelScan("n", "Person")
func buildSamplePlan() ir.LogicalPlan {
	scan := ir.NewNodeByLabelScan("n", "Person")
	sel := ir.NewSelection("n.active = true", scan)
	agg := ir.NewEagerAggregation(
		[]string{"name"},
		[]ir.AggregateExpr{{OutputName: "cnt", Function: "count", Argument: "n"}},
		sel,
	)
	return ir.NewProduceResults([]string{"name", "cnt"}, agg)
}

func TestSamplePlanDeepEqual(t *testing.T) {
	a := buildSamplePlan()
	b := buildSamplePlan()
	if !reflect.DeepEqual(a, b) {
		t.Error("two independently built identical plans are not DeepEqual")
	}
}

func TestSamplePlanStructure(t *testing.T) {
	root := buildSamplePlan()

	// Root is ProduceResults.
	pr, ok := root.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("root is %T, want *ir.ProduceResults", root)
	}
	if !reflect.DeepEqual(pr.Columns, []string{"name", "cnt"}) {
		t.Errorf("ProduceResults.Columns = %v", pr.Columns)
	}

	// Child is EagerAggregation.
	ea, ok := pr.Child.(*ir.EagerAggregation)
	if !ok {
		t.Fatalf("pr.Child is %T, want *ir.EagerAggregation", pr.Child)
	}
	if !reflect.DeepEqual(ea.GroupBy, []string{"name"}) {
		t.Errorf("EagerAggregation.GroupBy = %v", ea.GroupBy)
	}

	// Child is Selection.
	sel, ok := ea.Child.(*ir.Selection)
	if !ok {
		t.Fatalf("ea.Child is %T, want *ir.Selection", ea.Child)
	}
	if sel.Predicate != "n.active = true" {
		t.Errorf("Selection.Predicate = %q", sel.Predicate)
	}

	// Leaf is NodeByLabelScan.
	scan, ok := sel.Child.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("sel.Child is %T, want *ir.NodeByLabelScan", sel.Child)
	}
	if scan.NodeVar != "n" || scan.Label != "Person" {
		t.Errorf("NodeByLabelScan = {%q, %q}", scan.NodeVar, scan.Label)
	}
	if len(scan.Children()) != 0 {
		t.Error("NodeByLabelScan.Children() should be empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Direction constant values
// ─────────────────────────────────────────────────────────────────────────────

func TestDirectionValues(t *testing.T) {
	if ir.DirectionOutgoing == ir.DirectionIncoming {
		t.Error("DirectionOutgoing == DirectionIncoming")
	}
	if ir.DirectionOutgoing == ir.DirectionBoth {
		t.Error("DirectionOutgoing == DirectionBoth")
	}
	if ir.DirectionIncoming == ir.DirectionBoth {
		t.Error("DirectionIncoming == DirectionBoth")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// LogicalPlan interface satisfaction — compile-time check via assignment
// ─────────────────────────────────────────────────────────────────────────────

var _ ir.LogicalPlan = (*ir.Argument)(nil)
var _ ir.LogicalPlan = (*ir.AllNodesScan)(nil)
var _ ir.LogicalPlan = (*ir.NodeByLabelScan)(nil)
var _ ir.LogicalPlan = (*ir.NodeByIndexSeek)(nil)
var _ ir.LogicalPlan = (*ir.NodeByIndexRangeScan)(nil)
var _ ir.LogicalPlan = (*ir.Expand)(nil)
var _ ir.LogicalPlan = (*ir.OptionalExpand)(nil)
var _ ir.LogicalPlan = (*ir.VarLengthExpand)(nil)
var _ ir.LogicalPlan = (*ir.ProjectEndpoints)(nil)
var _ ir.LogicalPlan = (*ir.Selection)(nil)
var _ ir.LogicalPlan = (*ir.Projection)(nil)
var _ ir.LogicalPlan = (*ir.EagerAggregation)(nil)
var _ ir.LogicalPlan = (*ir.Sort)(nil)
var _ ir.LogicalPlan = (*ir.Top)(nil)
var _ ir.LogicalPlan = (*ir.Limit)(nil)
var _ ir.LogicalPlan = (*ir.Skip)(nil)
var _ ir.LogicalPlan = (*ir.Distinct)(nil)
var _ ir.LogicalPlan = (*ir.Union)(nil)
var _ ir.LogicalPlan = (*ir.UnionAll)(nil)
var _ ir.LogicalPlan = (*ir.Apply)(nil)
var _ ir.LogicalPlan = (*ir.SemiApply)(nil)
var _ ir.LogicalPlan = (*ir.AntiSemiApply)(nil)
var _ ir.LogicalPlan = (*ir.RollUpApply)(nil)
var _ ir.LogicalPlan = (*ir.Eager)(nil)
var _ ir.LogicalPlan = (*ir.Unwind)(nil)
var _ ir.LogicalPlan = (*ir.ProduceResults)(nil)
var _ ir.LogicalPlan = (*ir.CreateNode)(nil)
var _ ir.LogicalPlan = (*ir.CreateRelationship)(nil)
var _ ir.LogicalPlan = (*ir.SetProperty)(nil)
var _ ir.LogicalPlan = (*ir.SetLabels)(nil)
var _ ir.LogicalPlan = (*ir.RemoveProperty)(nil)
var _ ir.LogicalPlan = (*ir.RemoveLabels)(nil)
var _ ir.LogicalPlan = (*ir.DeleteNode)(nil)
var _ ir.LogicalPlan = (*ir.DeleteRelationship)(nil)
var _ ir.LogicalPlan = (*ir.DetachDelete)(nil)
var _ ir.LogicalPlan = (*ir.Merge)(nil)
var _ ir.LogicalPlan = (*ir.ProcedureCall)(nil)
