package ir_test

// writes_test.go — tests for CREATE/MERGE/SET/REMOVE/DELETE translation
// (task-226).
//
// These tests verify the write-operator translation that lives in writes.go.
// Several cases are already covered in translator_test.go (tasks 24-33). This
// file adds additional coverage for edge cases and compound write patterns.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// 1. CREATE (n:Person) — simple node with label, no properties
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_CreateNode_LabelOnly(t *testing.T) {
	q := &ast.SingleQuery{
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Create{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)

	cn, ok := plan.(*ir.CreateNode)
	if !ok {
		t.Fatalf("expected *ir.CreateNode, got %T", plan)
	}
	if cn.NodeVar != "n" {
		t.Errorf("NodeVar = %q, want n", cn.NodeVar)
	}
	if len(cn.Labels) != 1 || cn.Labels[0] != "Person" {
		t.Errorf("Labels = %v, want [Person]", cn.Labels)
	}
	if cn.Properties != "" {
		t.Errorf("Properties = %q, want empty", cn.Properties)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. CREATE (n:Person {name:"Alice"}) — node with properties
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_CreateNode_WithProperties(t *testing.T) {
	props := &ast.MapLiteral{
		Keys:   []string{"name"},
		Values: []ast.Expression{&ast.StringLiteral{Value: "Alice"}},
	}
	q := &ast.SingleQuery{
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Create{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}, Properties: props},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)

	cn, ok := plan.(*ir.CreateNode)
	if !ok {
		t.Fatalf("expected *ir.CreateNode, got %T", plan)
	}
	if cn.Properties == "" {
		t.Error("Properties must be non-empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. CREATE (a)-[:R]->(b) — relationship between two new nodes
//    → CreateRelationship wrapping two CreateNode operators
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_CreateRelationship(t *testing.T) {
	q := &ast.SingleQuery{
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Create{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("a")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{
								Variable:  strPtr("r"),
								Types:     []string{"R"},
								Direction: ast.RelDirectionOutgoing,
							},
							Node: &ast.NodePattern{Variable: strPtr("b")},
						},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)

	cr, ok := plan.(*ir.CreateRelationship)
	if !ok {
		t.Fatalf("expected *ir.CreateRelationship, got %T", plan)
	}
	if cr.RelType != "R" {
		t.Errorf("RelType = %q, want R", cr.RelType)
	}
	if cr.RelVar != "r" {
		t.Errorf("RelVar = %q, want r", cr.RelVar)
	}
	if cr.EndVar != "b" {
		t.Errorf("EndVar = %q, want b", cr.EndVar)
	}
	// Child must be CreateNode for the destination node.
	if _, ok := cr.Child.(*ir.CreateNode); !ok {
		t.Fatalf("CreateRelationship.Child expected *ir.CreateNode, got %T", cr.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. MERGE (n:Person) — no ON CREATE / ON MATCH
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_Merge_NoActions(t *testing.T) {
	q := &ast.SingleQuery{
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Merge{
				Pattern: &ast.PathPattern{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)

	m, ok := plan.(*ir.Merge)
	if !ok {
		t.Fatalf("expected *ir.Merge, got %T", plan)
	}
	if m.Pattern == "" {
		t.Error("Merge.Pattern must be non-empty")
	}
	if len(m.OnCreate) != 0 || len(m.OnMatch) != 0 {
		t.Errorf("unexpected actions: OnCreate=%v OnMatch=%v", m.OnCreate, m.OnMatch)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. MERGE … ON CREATE SET … ON MATCH SET … — both actions present
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_Merge_BothActions(t *testing.T) {
	q := &ast.SingleQuery{
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Merge{
				Pattern: &ast.PathPattern{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}},
					},
				},
				OnCreate: []*ast.SetItem{
					{
						Target:   &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "created"},
						Value:    &ast.IntLiteral{Value: 1},
						Operator: "=",
					},
				},
				OnMatch: []*ast.SetItem{
					{
						Target:   &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "updated"},
						Value:    &ast.IntLiteral{Value: 1},
						Operator: "=",
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)

	m, ok := plan.(*ir.Merge)
	if !ok {
		t.Fatalf("expected *ir.Merge, got %T", plan)
	}
	if len(m.OnCreate) != 1 {
		t.Errorf("OnCreate = %v, want 1 item", m.OnCreate)
	}
	if len(m.OnMatch) != 1 {
		t.Errorf("OnMatch = %v, want 1 item", m.OnMatch)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. SET n.name = "Bob" — single property
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_SetProperty(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Set{Items: []*ast.SetItem{
				{
					Target:   &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"},
					Value:    &ast.StringLiteral{Value: "Bob"},
					Operator: "=",
				},
			}},
		},
	}
	plan := mustFromAST(t, q)

	sp, ok := plan.(*ir.SetProperty)
	if !ok {
		t.Fatalf("expected *ir.SetProperty, got %T", plan)
	}
	if sp.EntityVar != "n" || sp.PropertyKey != "name" {
		t.Errorf("SetProperty = {%q, %q}", sp.EntityVar, sp.PropertyKey)
	}
	if sp.Value == "" {
		t.Error("Value must be non-empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. SET n:Admin — label addition
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_SetLabels(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Set{Items: []*ast.SetItem{
				{Target: &ast.Variable{Name: "n"}, Labels: []string{"Admin"}},
			}},
		},
	}
	plan := mustFromAST(t, q)

	sl, ok := plan.(*ir.SetLabels)
	if !ok {
		t.Fatalf("expected *ir.SetLabels, got %T", plan)
	}
	if sl.NodeVar != "n" {
		t.Errorf("NodeVar = %q, want n", sl.NodeVar)
	}
	if len(sl.Labels) != 1 || sl.Labels[0] != "Admin" {
		t.Errorf("Labels = %v, want [Admin]", sl.Labels)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. REMOVE n.tempField — property removal
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_RemoveProperty(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Remove{Items: []*ast.RemoveItem{
				{Target: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "tempField"}},
			}},
		},
	}
	plan := mustFromAST(t, q)

	rp, ok := plan.(*ir.RemoveProperty)
	if !ok {
		t.Fatalf("expected *ir.RemoveProperty, got %T", plan)
	}
	if rp.EntityVar != "n" || rp.PropertyKey != "tempField" {
		t.Errorf("RemoveProperty = {%q, %q}", rp.EntityVar, rp.PropertyKey)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. REMOVE n:Inactive — label removal
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_RemoveLabels(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Remove{Items: []*ast.RemoveItem{
				{Target: &ast.Variable{Name: "n"}, Labels: []string{"Inactive"}},
			}},
		},
	}
	plan := mustFromAST(t, q)

	rl, ok := plan.(*ir.RemoveLabels)
	if !ok {
		t.Fatalf("expected *ir.RemoveLabels, got %T", plan)
	}
	if len(rl.Labels) != 1 || rl.Labels[0] != "Inactive" {
		t.Errorf("Labels = %v, want [Inactive]", rl.Labels)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 10. DELETE n — single node deletion
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_DeleteNode(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Delete{Expressions: []ast.Expression{&ast.Variable{Name: "n"}}},
		},
	}
	plan := mustFromAST(t, q)

	dn, ok := plan.(*ir.DeleteNode)
	if !ok {
		t.Fatalf("expected *ir.DeleteNode, got %T", plan)
	}
	if dn.NodeVar != "n" {
		t.Errorf("NodeVar = %q, want n", dn.NodeVar)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 11. DETACH DELETE n — detach deletion
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_DetachDelete(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.DetachDelete{Expressions: []ast.Expression{&ast.Variable{Name: "n"}}},
		},
	}
	plan := mustFromAST(t, q)

	dd, ok := plan.(*ir.DetachDelete)
	if !ok {
		t.Fatalf("expected *ir.DetachDelete, got %T", plan)
	}
	if dd.NodeVar != "n" {
		t.Errorf("NodeVar = %q, want n", dd.NodeVar)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 12. SET n.age = 30, n.name = "Bob" — multiple SET items stacked
//     Outer: SetProperty("name"), Inner: SetProperty("age")
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_SetMultipleProperties(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Set{Items: []*ast.SetItem{
				{Target: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"}, Value: &ast.IntLiteral{Value: 30}, Operator: "="},
				{Target: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}, Value: &ast.StringLiteral{Value: "Bob"}, Operator: "="},
			}},
		},
	}
	plan := mustFromAST(t, q)

	outer, ok := plan.(*ir.SetProperty)
	if !ok {
		t.Fatalf("expected *ir.SetProperty (outer), got %T", plan)
	}
	if outer.PropertyKey != "name" {
		t.Errorf("outer PropertyKey = %q, want name", outer.PropertyKey)
	}
	inner, ok := outer.Child.(*ir.SetProperty)
	if !ok {
		t.Fatalf("outer.Child expected *ir.SetProperty (inner), got %T", outer.Child)
	}
	if inner.PropertyKey != "age" {
		t.Errorf("inner PropertyKey = %q, want age", inner.PropertyKey)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 13. MERGE BoundVars — pattern variables are captured
// ─────────────────────────────────────────────────────────────────────────────

func Test_Writes_Merge_BoundVars(t *testing.T) {
	q := &ast.SingleQuery{
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Merge{
				Pattern: &ast.PathPattern{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)

	m, ok := plan.(*ir.Merge)
	if !ok {
		t.Fatalf("expected *ir.Merge, got %T", plan)
	}
	if !containsAll(m.BoundVars, "n") {
		t.Errorf("BoundVars = %v, want to contain n", m.BoundVars)
	}
}
