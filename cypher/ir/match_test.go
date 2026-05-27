package ir_test

// match_test.go — golden IR tests for MATCH clause translation (task-220).
//
// Each test fixes the exact logical plan tree produced by translateMatch for a
// specific MATCH variant.  Tests are named Test_Match_<variant> so they are
// discoverable independently from the broader translator suite.
//
// Golden-tree legend:
//   Apply(outer, inner)  — Cartesian product of two independent paths
//   NodeByLabelScan(var, label)
//   AllNodesScan(var)
//   Selection(pred, child)
//   Expand(from, rel, types, dir, to, child)

import (
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// 1. MATCH (n) → AllNodesScan("n")
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_AllNodesScan(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			matchNodes(&ast.NodePattern{Variable: strPtr("n")}),
		},
	}
	plan := mustFromAST(t, q)

	scan, ok := plan.(*ir.AllNodesScan)
	if !ok {
		t.Fatalf("expected *ir.AllNodesScan, got %T", plan)
	}
	if scan.NodeVar != "n" {
		t.Errorf("NodeVar = %q, want n", scan.NodeVar)
	}
	if len(scan.Children()) != 0 {
		t.Error("AllNodesScan must have no children")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. MATCH (n:Person) → NodeByLabelScan("n", "Person")
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_NodeByLabelScan(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			matchNodes(&ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}),
		},
	}
	plan := mustFromAST(t, q)

	scan, ok := plan.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("expected *ir.NodeByLabelScan, got %T", plan)
	}
	if scan.NodeVar != "n" {
		t.Errorf("NodeVar = %q, want n", scan.NodeVar)
	}
	if scan.Label != "Person" {
		t.Errorf("Label = %q, want Person", scan.Label)
	}
	if len(scan.Children()) != 0 {
		t.Error("NodeByLabelScan must have no children")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. MATCH (n:Person) WHERE n.age > 18
//    → Selection("n.age > 18", NodeByLabelScan("n","Person"))
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_NodeByLabelScan_Where(t *testing.T) {
	pred := &ast.BinaryOp{
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
		Operator: ">",
		Right:    &ast.IntLiteral{Value: 18},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: singleNodePattern("n", "Person"),
				Where:   &ast.Where{Predicate: pred},
			},
		},
	}
	plan := mustFromAST(t, q)

	// Root: Selection (WHERE).
	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("expected *ir.Selection (WHERE), got %T", plan)
	}
	if sel.Predicate == "" {
		t.Error("WHERE Selection.Predicate must be non-empty")
	}

	// Child: NodeByLabelScan.
	scan, ok := sel.Child.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("sel.Child expected *ir.NodeByLabelScan, got %T", sel.Child)
	}
	if scan.NodeVar != "n" || scan.Label != "Person" {
		t.Errorf("scan = {%q, %q}", scan.NodeVar, scan.Label)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. MATCH (a)-[:KNOWS]->(b)
//    → Expand(from="a", relTypes=["KNOWS"], dir=Outgoing, to="b",
//             child=AllNodesScan("a"))
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_Expand_Outgoing(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{
					Node: &ast.NodePattern{Variable: strPtr("a")},
					Next: &ast.PathElement{
						Relationship: &ast.RelationshipPattern{
							Types:     []string{"KNOWS"},
							Direction: ast.RelDirectionOutgoing,
						},
						Node: &ast.NodePattern{Variable: strPtr("b")},
					},
				}},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	exp, ok := plan.(*ir.Expand)
	if !ok {
		t.Fatalf("expected *ir.Expand, got %T", plan)
	}
	if exp.FromVar != "a" {
		t.Errorf("FromVar = %q, want a", exp.FromVar)
	}
	if exp.ToVar != "b" {
		t.Errorf("ToVar = %q, want b", exp.ToVar)
	}
	if exp.Direction != ir.DirectionOutgoing {
		t.Errorf("Direction = %v, want Outgoing", exp.Direction)
	}
	if len(exp.RelTypes) != 1 || exp.RelTypes[0] != "KNOWS" {
		t.Errorf("RelTypes = %v, want [KNOWS]", exp.RelTypes)
	}
	if _, ok := exp.Child.(*ir.AllNodesScan); !ok {
		t.Fatalf("Expand.Child expected *ir.AllNodesScan, got %T", exp.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. MATCH (n), (m)
//    → Apply(outer=AllNodesScan("n"), inner=AllNodesScan("m"))
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_MultiPattern_TwoAllNodesScan(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("m")}}},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	app, ok := plan.(*ir.Apply)
	if !ok {
		t.Fatalf("expected *ir.Apply for multi-pattern MATCH, got %T", plan)
	}
	outer, ok := app.Outer.(*ir.AllNodesScan)
	if !ok {
		t.Fatalf("Apply.Outer expected *ir.AllNodesScan, got %T", app.Outer)
	}
	if outer.NodeVar != "n" {
		t.Errorf("outer NodeVar = %q, want n", outer.NodeVar)
	}
	inner, ok := app.Inner.(*ir.AllNodesScan)
	if !ok {
		t.Fatalf("Apply.Inner expected *ir.AllNodesScan, got %T", app.Inner)
	}
	if inner.NodeVar != "m" {
		t.Errorf("inner NodeVar = %q, want m", inner.NodeVar)
	}
	// Apply exposes both variables.
	vars := app.Vars()
	if !containsAll(vars, "n", "m") {
		t.Errorf("Apply.Vars() = %v, want to contain n and m", vars)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. MATCH (n:Person {name: "Alice"})
//    → Selection(prop, NodeByLabelScan("n","Person"))
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_NodeWithPropertyPredicate(t *testing.T) {
	props := &ast.MapLiteral{
		Keys:   []string{"name"},
		Values: []ast.Expression{&ast.StringLiteral{Value: "Alice"}},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			matchNodes(&ast.NodePattern{
				Variable:   strPtr("n"),
				Labels:     []string{"Person"},
				Properties: props,
			}),
		},
	}
	plan := mustFromAST(t, q)

	// Outermost: Selection for property predicate.
	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("expected *ir.Selection (property), got %T", plan)
	}
	if sel.Predicate == "" {
		t.Error("property Selection.Predicate must be non-empty")
	}

	// Child: NodeByLabelScan (no further filter — property is at lowest legal position).
	scan, ok := sel.Child.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("sel.Child expected *ir.NodeByLabelScan, got %T", sel.Child)
	}
	if scan.NodeVar != "n" || scan.Label != "Person" {
		t.Errorf("scan = {%q, %q}", scan.NodeVar, scan.Label)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. MATCH (n:Person), (m:Movie)
//    → Apply(NodeByLabelScan("n","Person"), NodeByLabelScan("m","Movie"))
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_MultiPattern_TwoLabelScans(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("m"), Labels: []string{"Movie"}}}},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	app, ok := plan.(*ir.Apply)
	if !ok {
		t.Fatalf("expected *ir.Apply, got %T", plan)
	}
	outerScan, ok := app.Outer.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("Apply.Outer expected *ir.NodeByLabelScan, got %T", app.Outer)
	}
	if outerScan.Label != "Person" {
		t.Errorf("outer Label = %q, want Person", outerScan.Label)
	}
	innerScan, ok := app.Inner.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("Apply.Inner expected *ir.NodeByLabelScan, got %T", app.Inner)
	}
	if innerScan.Label != "Movie" {
		t.Errorf("inner Label = %q, want Movie", innerScan.Label)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. MATCH (n:Person), (m:Movie), (d:Director)
//    → Apply(Apply(Person, Movie), Director)
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_MultiPattern_ThreePaths_LeftAssociative(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("m"), Labels: []string{"Movie"}}}},
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("d"), Labels: []string{"Director"}}}},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	// Root: Apply(Apply(Person, Movie), Director).
	outerApp, ok := plan.(*ir.Apply)
	if !ok {
		t.Fatalf("root expected *ir.Apply, got %T", plan)
	}
	innerApp, ok := outerApp.Outer.(*ir.Apply)
	if !ok {
		t.Fatalf("outerApp.Outer expected *ir.Apply, got %T", outerApp.Outer)
	}
	personScan, ok := innerApp.Outer.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("innerApp.Outer expected *ir.NodeByLabelScan, got %T", innerApp.Outer)
	}
	if personScan.Label != "Person" {
		t.Errorf("innermost outer scan Label = %q, want Person", personScan.Label)
	}
	movieScan, ok := innerApp.Inner.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("innerApp.Inner expected *ir.NodeByLabelScan, got %T", innerApp.Inner)
	}
	if movieScan.Label != "Movie" {
		t.Errorf("inner scan Label = %q, want Movie", movieScan.Label)
	}
	directorScan, ok := outerApp.Inner.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("outerApp.Inner expected *ir.NodeByLabelScan, got %T", outerApp.Inner)
	}
	if directorScan.Label != "Director" {
		t.Errorf("outermost inner scan Label = %q, want Director", directorScan.Label)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. MATCH (n:Person:Employee)
//    → Selection("n:Employee", NodeByLabelScan("n","Person"))
//    — extra label becomes a Selection above the scan
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_MultiLabel_ExtraLabelAsSelection(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			matchNodes(&ast.NodePattern{
				Variable: strPtr("n"),
				Labels:   []string{"Person", "Employee"},
			}),
		},
	}
	plan := mustFromAST(t, q)

	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("expected *ir.Selection for extra label, got %T", plan)
	}
	if sel.Predicate != "(n:Employee)" {
		t.Errorf("Selection.Predicate = %q, want (n:Employee)", sel.Predicate)
	}
	scan, ok := sel.Child.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("sel.Child expected *ir.NodeByLabelScan, got %T", sel.Child)
	}
	if scan.Label != "Person" {
		t.Errorf("scan.Label = %q, want Person", scan.Label)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 10. MATCH (n)-[:WORKS_FOR]->(c:Company) WHERE n.active = true
//     Verifies that:
//     — label filter on destination node appears immediately above Expand
//     — WHERE filter appears at the top, above the label filter
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_Expand_WithDestLabelAndWhere(t *testing.T) {
	pred := &ast.BinaryOp{
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "active"},
		Operator: "=",
		Right:    &ast.BoolLiteral{Value: true},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("n")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{
								Types:     []string{"WORKS_FOR"},
								Direction: ast.RelDirectionOutgoing,
							},
							Node: &ast.NodePattern{Variable: strPtr("c"), Labels: []string{"Company"}},
						},
					}},
				}},
				Where: &ast.Where{Predicate: pred},
			},
		},
	}
	plan := mustFromAST(t, q)

	// Root: Selection (WHERE).
	whereSel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("root expected *ir.Selection (WHERE), got %T", plan)
	}

	// Child of WHERE: Selection (dest label "c:Company").
	labelSel, ok := whereSel.Child.(*ir.Selection)
	if !ok {
		t.Fatalf("whereSel.Child expected *ir.Selection (label), got %T", whereSel.Child)
	}
	if labelSel.Predicate != "(c:Company)" {
		t.Errorf("label Selection.Predicate = %q, want (c:Company)", labelSel.Predicate)
	}

	// Child of label filter: Expand.
	exp, ok := labelSel.Child.(*ir.Expand)
	if !ok {
		t.Fatalf("labelSel.Child expected *ir.Expand, got %T", labelSel.Child)
	}
	if exp.ToVar != "c" {
		t.Errorf("Expand.ToVar = %q, want c", exp.ToVar)
	}
	if len(exp.RelTypes) != 1 || exp.RelTypes[0] != "WORKS_FOR" {
		t.Errorf("Expand.RelTypes = %v, want [WORKS_FOR]", exp.RelTypes)
	}

	// Child of Expand: AllNodesScan for source node.
	if _, ok := exp.Child.(*ir.AllNodesScan); !ok {
		t.Fatalf("Expand.Child expected *ir.AllNodesScan, got %T", exp.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 11. MATCH (n {status: "active"})
//     Node without label but with properties:
//     → Selection(prop, AllNodesScan("n"))
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_AllNodesScan_WithProperties(t *testing.T) {
	props := &ast.MapLiteral{
		Keys:   []string{"status"},
		Values: []ast.Expression{&ast.StringLiteral{Value: "active"}},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			matchNodes(&ast.NodePattern{
				Variable:   strPtr("n"),
				Properties: props,
			}),
		},
	}
	plan := mustFromAST(t, q)

	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("expected *ir.Selection (property), got %T", plan)
	}
	if sel.Predicate == "" {
		t.Error("property Selection.Predicate must be non-empty")
	}
	if _, ok := sel.Child.(*ir.AllNodesScan); !ok {
		t.Fatalf("sel.Child expected *ir.AllNodesScan, got %T", sel.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 12. MATCH (a)-[r:KNOWS]->(b)-[s:LIKES]->(c)
//     Three-hop path — verifies left-to-right expand chaining.
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_ThreeHopExpand(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{
					Node: &ast.NodePattern{Variable: strPtr("a")},
					Next: &ast.PathElement{
						Relationship: &ast.RelationshipPattern{Variable: strPtr("r"), Types: []string{"KNOWS"}, Direction: ast.RelDirectionOutgoing},
						Node:         &ast.NodePattern{Variable: strPtr("b")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Variable: strPtr("s"), Types: []string{"LIKES"}, Direction: ast.RelDirectionOutgoing},
							Node:         &ast.NodePattern{Variable: strPtr("c")},
						},
					},
				}},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	// Root: Expand for the second hop (s:LIKES → c).
	exp2, ok := plan.(*ir.Expand)
	if !ok {
		t.Fatalf("root expected *ir.Expand (second hop), got %T", plan)
	}
	if exp2.RelVar != "s" {
		t.Errorf("outer Expand.RelVar = %q, want s", exp2.RelVar)
	}
	if exp2.ToVar != "c" {
		t.Errorf("outer Expand.ToVar = %q, want c", exp2.ToVar)
	}

	// Child: Expand for the first hop (r:KNOWS → b).
	exp1, ok := exp2.Child.(*ir.Expand)
	if !ok {
		t.Fatalf("exp2.Child expected *ir.Expand (first hop), got %T", exp2.Child)
	}
	if exp1.RelVar != "r" {
		t.Errorf("inner Expand.RelVar = %q, want r", exp1.RelVar)
	}
	if exp1.ToVar != "b" {
		t.Errorf("inner Expand.ToVar = %q, want b", exp1.ToVar)
	}

	// Leaf: AllNodesScan for anchor node a.
	if _, ok := exp1.Child.(*ir.AllNodesScan); !ok {
		t.Fatalf("exp1.Child expected *ir.AllNodesScan, got %T", exp1.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 13. MATCH (n:Person) WHERE n.age > 18 — Apply.Vars() round-trip
//     Verifies that Vars on the Selection pass-through correctly.
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_Selection_VarsPassthrough(t *testing.T) {
	pred := &ast.BinaryOp{
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
		Operator: ">",
		Right:    &ast.IntLiteral{Value: 18},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: singleNodePattern("n", "Person"),
				Where:   &ast.Where{Predicate: pred},
			},
		},
	}
	plan := mustFromAST(t, q)

	vars := plan.Vars()
	if !containsAll(vars, "n") {
		t.Errorf("plan.Vars() = %v, must contain n", vars)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 14. MATCH (n:Person {name:"Alice"}), (m:Movie {title:"Matrix"})
//     Multi-pattern with property predicates on both sides.
//     Root → Apply(Selection(prop, LabelScan), Selection(prop, LabelScan))
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_MultiPattern_WithProperties(t *testing.T) {
	aliceProps := &ast.MapLiteral{
		Keys:   []string{"name"},
		Values: []ast.Expression{&ast.StringLiteral{Value: "Alice"}},
	}
	matrixProps := &ast.MapLiteral{
		Keys:   []string{"title"},
		Values: []ast.Expression{&ast.StringLiteral{Value: "Matrix"}},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}, Properties: aliceProps}}},
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("m"), Labels: []string{"Movie"}, Properties: matrixProps}}},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	app, ok := plan.(*ir.Apply)
	if !ok {
		t.Fatalf("expected *ir.Apply, got %T", plan)
	}

	// Outer: Selection(prop) → NodeByLabelScan("n","Person").
	outerSel, ok := app.Outer.(*ir.Selection)
	if !ok {
		t.Fatalf("Apply.Outer expected *ir.Selection, got %T", app.Outer)
	}
	if _, ok := outerSel.Child.(*ir.NodeByLabelScan); !ok {
		t.Fatalf("outer sel.Child expected *ir.NodeByLabelScan, got %T", outerSel.Child)
	}

	// Inner: Selection(prop) → NodeByLabelScan("m","Movie").
	innerSel, ok := app.Inner.(*ir.Selection)
	if !ok {
		t.Fatalf("Apply.Inner expected *ir.Selection, got %T", app.Inner)
	}
	if _, ok := innerSel.Child.(*ir.NodeByLabelScan); !ok {
		t.Fatalf("inner sel.Child expected *ir.NodeByLabelScan, got %T", innerSel.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 15. MATCH (n:Person), (m:Movie) WHERE n.age > 18
//     WHERE predicate sits above the Apply for the multi-pattern.
// ─────────────────────────────────────────────────────────────────────────────

func Test_Match_MultiPattern_WithWhere(t *testing.T) {
	pred := &ast.BinaryOp{
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
		Operator: ">",
		Right:    &ast.IntLiteral{Value: 18},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("m"), Labels: []string{"Movie"}}}},
				}},
				Where: &ast.Where{Predicate: pred},
			},
		},
	}
	plan := mustFromAST(t, q)

	// Root: Selection (WHERE above the Apply).
	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("root expected *ir.Selection, got %T", plan)
	}
	if sel.Predicate == "" {
		t.Error("WHERE Selection.Predicate must be non-empty")
	}

	// Child: Apply.
	if _, ok := sel.Child.(*ir.Apply); !ok {
		t.Fatalf("sel.Child expected *ir.Apply, got %T", sel.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// matchNodes creates a minimal single-path MATCH clause with the given node.
func matchNodes(np *ast.NodePattern) *ast.Match {
	return &ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
		{Head: &ast.PathElement{Node: np}},
	}}}
}

// singleNodePattern returns a Pattern with one path containing a named node
// with the given label.
func singleNodePattern(nodeVar, label string) *ast.Pattern {
	return &ast.Pattern{Paths: []*ast.PathPattern{
		{Head: &ast.PathElement{Node: &ast.NodePattern{
			Variable: strPtr(nodeVar),
			Labels:   []string{label},
		}}},
	}}
}

// containsAll reports whether slice contains all of the provided strings.
func containsAll(slice []string, elems ...string) bool {
	set := make(map[string]struct{}, len(slice))
	for _, s := range slice {
		set[s] = struct{}{}
	}
	for _, e := range elems {
		if _, ok := set[e]; !ok {
			return false
		}
	}
	return true
}
