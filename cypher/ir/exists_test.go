package ir_test

// exists_test.go — tests for EXISTS / NOT EXISTS subquery translation (task-224).

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// 1. MATCH (a) WHERE EXISTS { (a)-[:R]->(b) }
//    → SemiApply(outer=AllNodesScan("a"), inner=subPlan)
// ─────────────────────────────────────────────────────────────────────────────

func Test_Exists_SemiApply_PatternForm(t *testing.T) {
	existsExpr := &ast.ExistsSubquery{
		Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
			{Head: &ast.PathElement{
				Node: &ast.NodePattern{Variable: strPtr("a")},
				Next: &ast.PathElement{
					Relationship: &ast.RelationshipPattern{
						Types:     []string{"R"},
						Direction: ast.RelDirectionOutgoing,
					},
					Node: &ast.NodePattern{Variable: strPtr("b")},
				},
			}},
		}},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("a")}}},
				}},
				Where: &ast.Where{Predicate: existsExpr},
			},
		},
	}
	plan := mustFromAST(t, q)

	sa, ok := plan.(*ir.SemiApply)
	if !ok {
		t.Fatalf("expected *ir.SemiApply, got %T", plan)
	}
	// Outer: AllNodesScan("a").
	outer, ok := sa.Outer.(*ir.AllNodesScan)
	if !ok {
		t.Fatalf("SemiApply.Outer expected *ir.AllNodesScan, got %T", sa.Outer)
	}
	if outer.NodeVar != "a" {
		t.Errorf("outer NodeVar = %q, want a", outer.NodeVar)
	}
	// Inner: the pattern expand plan (non-nil).
	if sa.Inner == nil {
		t.Fatal("SemiApply.Inner must be non-nil")
	}
	// SemiApply exposes only outer vars.
	vars := sa.Vars()
	if !containsAll(vars, "a") {
		t.Errorf("Vars() = %v, want to contain a", vars)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. MATCH (a) WHERE NOT EXISTS { (a)-[:R]->(b) }
//    → AntiSemiApply(outer=AllNodesScan("a"), inner=subPlan)
// ─────────────────────────────────────────────────────────────────────────────

func Test_Exists_AntiSemiApply_NotExists(t *testing.T) {
	existsExpr := &ast.ExistsSubquery{
		Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
			{Head: &ast.PathElement{
				Node: &ast.NodePattern{Variable: strPtr("a")},
				Next: &ast.PathElement{
					Relationship: &ast.RelationshipPattern{
						Types:     []string{"R"},
						Direction: ast.RelDirectionOutgoing,
					},
					Node: &ast.NodePattern{Variable: strPtr("b")},
				},
			}},
		}},
	}
	notExistsExpr := &ast.UnaryOp{Operator: "NOT", Operand: existsExpr}

	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("a")}}},
				}},
				Where: &ast.Where{Predicate: notExistsExpr},
			},
		},
	}
	plan := mustFromAST(t, q)

	asa, ok := plan.(*ir.AntiSemiApply)
	if !ok {
		t.Fatalf("expected *ir.AntiSemiApply, got %T", plan)
	}
	if asa.Inner == nil {
		t.Fatal("AntiSemiApply.Inner must be non-nil")
	}
	// AntiSemiApply exposes only outer vars.
	vars := asa.Vars()
	if !containsAll(vars, "a") {
		t.Errorf("Vars() = %v, want to contain a", vars)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. EXISTS with full subquery form (Query field non-nil)
//    → SemiApply with inner built from the subquery's reading clauses
// ─────────────────────────────────────────────────────────────────────────────

func Test_Exists_SemiApply_QueryForm(t *testing.T) {
	existsExpr := &ast.ExistsSubquery{
		Query: &ast.SingleQuery{
			ReadingClauses: []ast.ReadingClause{
				&ast.Match{
					Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
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
					}},
				},
			},
		},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("a")}}},
				}},
				Where: &ast.Where{Predicate: existsExpr},
			},
		},
	}
	plan := mustFromAST(t, q)

	if _, ok := plan.(*ir.SemiApply); !ok {
		t.Fatalf("expected *ir.SemiApply, got %T", plan)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Plain predicate falls back to Selection (no regression)
//    MATCH (n) WHERE n.age > 18 → Selection (not SemiApply)
// ─────────────────────────────────────────────────────────────────────────────

func Test_Exists_PlainPredicate_FallsBackToSelection(t *testing.T) {
	pred := &ast.BinaryOp{
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
		Operator: ">",
		Right:    &ast.IntLiteral{Value: 18},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
				Where: &ast.Where{Predicate: pred},
			},
		},
	}
	plan := mustFromAST(t, q)

	if _, ok := plan.(*ir.Selection); !ok {
		t.Fatalf("expected *ir.Selection for plain predicate, got %T", plan)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. SemiApply inner plan contains an Argument leaf (correlation injection)
// ─────────────────────────────────────────────────────────────────────────────

func Test_Exists_InnerPlanHasArgumentLeaf(t *testing.T) {
	existsExpr := &ast.ExistsSubquery{
		Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
			{Head: &ast.PathElement{
				Node: &ast.NodePattern{Variable: strPtr("a")},
				Next: &ast.PathElement{
					Relationship: &ast.RelationshipPattern{
						Direction: ast.RelDirectionOutgoing,
					},
					Node: &ast.NodePattern{Variable: strPtr("b")},
				},
			}},
		}},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("a")}}},
				}},
				Where: &ast.Where{Predicate: existsExpr},
			},
		},
	}
	plan := mustFromAST(t, q)

	sa := plan.(*ir.SemiApply)
	// Walk inner to find the Argument leaf.
	found := findArgument(sa.Inner)
	if !found {
		t.Error("SemiApply.Inner should contain an Argument leaf node for correlation injection")
	}
}

// findArgument walks a plan tree and reports whether an *ir.Argument leaf exists.
func findArgument(p ir.LogicalPlan) bool {
	if p == nil {
		return false
	}
	if _, ok := p.(*ir.Argument); ok {
		return true
	}
	for _, ch := range p.Children() {
		if findArgument(ch) {
			return true
		}
	}
	return false
}
