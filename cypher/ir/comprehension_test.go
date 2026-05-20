package ir_test

// comprehension_test.go — tests for PatternComprehension → RollUpApply
// translation (task-225).

import (
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/ir"
)

// buildComprehensionQuery builds a query that has a PatternComprehension as a
// RETURN expression:
//
//	MATCH (a) RETURN [(a)-[:R]->(b) | b.name] AS names
func buildComprehensionQuery(withPredicate bool) ast.Query {
	pattern := &ast.PathPattern{
		Head: &ast.PathElement{
			Node: &ast.NodePattern{Variable: strPtr("a")},
			Next: &ast.PathElement{
				Relationship: &ast.RelationshipPattern{
					Types:     []string{"R"},
					Direction: ast.RelDirectionOutgoing,
				},
				Node: &ast.NodePattern{Variable: strPtr("b")},
			},
		},
	}
	projection := &ast.Property{Receiver: &ast.Variable{Name: "b"}, Key: "name"}

	pc := &ast.PatternComprehension{
		Pattern:    pattern,
		Projection: projection,
	}
	if withPredicate {
		pc.Predicate = &ast.BinaryOp{
			Left:     &ast.Property{Receiver: &ast.Variable{Name: "b"}, Key: "active"},
			Operator: "=",
			Right:    &ast.BoolLiteral{Value: true},
		}
	}

	return &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("a")}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: pc, Alias: strPtr("names")},
		}}},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Pattern comprehension in RETURN → RollUpApply at root of inner expression
//    The ProduceResults → Projection → RollUpApply tree is expected.
// ─────────────────────────────────────────────────────────────────────────────

func Test_Comprehension_BasicRollUpApply(t *testing.T) {
	q := buildComprehensionQuery(false)
	plan := mustFromAST(t, q)

	// ProduceResults → Projection → RollUpApply
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("expected *ir.ProduceResults, got %T", plan)
	}
	proj, ok := pr.Child.(*ir.Projection)
	if !ok {
		t.Fatalf("pr.Child expected *ir.Projection, got %T", pr.Child)
	}
	rua, ok := proj.Child.(*ir.RollUpApply)
	if !ok {
		t.Fatalf("proj.Child expected *ir.RollUpApply, got %T", proj.Child)
	}
	if rua.CollectVar != "names" {
		t.Errorf("CollectVar = %q, want names", rua.CollectVar)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Pattern comprehension with WHERE predicate inside
//    → inner plan contains Selection above the expand
// ─────────────────────────────────────────────────────────────────────────────

func Test_Comprehension_WithInternalPredicate(t *testing.T) {
	q := buildComprehensionQuery(true)
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	proj := pr.Child.(*ir.Projection)
	rua, ok := proj.Child.(*ir.RollUpApply)
	if !ok {
		t.Fatalf("proj.Child expected *ir.RollUpApply, got %T", proj.Child)
	}
	// Inner plan must contain a Selection (for the WHERE predicate inside the
	// comprehension).
	found := findSelection(rua.Inner)
	if !found {
		t.Error("RollUpApply.Inner should contain a Selection for the comprehension WHERE predicate")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. RollUpApply Vars() includes outer variable and collectVar
// ─────────────────────────────────────────────────────────────────────────────

func Test_Comprehension_VarsContainOuterAndCollect(t *testing.T) {
	q := buildComprehensionQuery(false)
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	proj := pr.Child.(*ir.Projection)
	rua := proj.Child.(*ir.RollUpApply)

	vars := rua.Vars()
	// Must contain the outer variable "a" and the collect variable "names".
	if !containsAll(vars, "a", "names") {
		t.Errorf("Vars() = %v, want to contain a and names", vars)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. RollUpApply inner plan contains an Argument leaf (correlation)
// ─────────────────────────────────────────────────────────────────────────────

func Test_Comprehension_InnerHasArgumentLeaf(t *testing.T) {
	q := buildComprehensionQuery(false)
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	proj := pr.Child.(*ir.Projection)
	rua := proj.Child.(*ir.RollUpApply)

	if !findArgument(rua.Inner) {
		t.Error("RollUpApply.Inner should contain an Argument leaf for correlation")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// findSelection reports whether any node in the plan tree is a *ir.Selection.
func findSelection(p ir.LogicalPlan) bool {
	if p == nil {
		return false
	}
	if _, ok := p.(*ir.Selection); ok {
		return true
	}
	for _, ch := range p.Children() {
		if findSelection(ch) {
			return true
		}
	}
	return false
}
