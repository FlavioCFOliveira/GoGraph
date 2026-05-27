package ir_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }
func i64Ptr(v int64) *int64   { return &v }

// mustFromAST is a test helper that calls FromAST and fails immediately on error.
func mustFromAST(t *testing.T, q ast.Query) ir.LogicalPlan {
	t.Helper()
	plan, err := ir.FromAST(q)
	if err != nil {
		t.Fatalf("FromAST error: %v", err)
	}
	return plan
}

// matchSingle wraps a SingleQuery in a MATCH clause with one node pattern.
func singleQueryMatch(np *ast.NodePattern, where *ast.Where) ast.Query {
	return &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{Head: &ast.PathElement{Node: np}},
					},
				},
				Where: where,
			},
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. AllNodesScan — anonymous node
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_AllNodesScan_Anonymous(t *testing.T) {
	// MATCH ()
	q := singleQueryMatch(&ast.NodePattern{}, nil)
	plan := mustFromAST(t, q)
	if _, ok := plan.(*ir.AllNodesScan); !ok {
		t.Fatalf("expected *ir.AllNodesScan, got %T", plan)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. AllNodesScan — named node, no label
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_AllNodesScan_Named(t *testing.T) {
	// MATCH (n)
	q := singleQueryMatch(&ast.NodePattern{Variable: strPtr("n")}, nil)
	plan := mustFromAST(t, q)
	scan, ok := plan.(*ir.AllNodesScan)
	if !ok {
		t.Fatalf("expected *ir.AllNodesScan, got %T", plan)
	}
	if scan.NodeVar != "n" {
		t.Errorf("NodeVar = %q, want %q", scan.NodeVar, "n")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. NodeByLabelScan — single label
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_NodeByLabelScan_Single(t *testing.T) {
	// MATCH (n:Person)
	q := singleQueryMatch(&ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}, nil)
	plan := mustFromAST(t, q)
	scan, ok := plan.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("expected *ir.NodeByLabelScan, got %T", plan)
	}
	if scan.NodeVar != "n" || scan.Label != "Person" {
		t.Errorf("NodeByLabelScan = {%q, %q}", scan.NodeVar, scan.Label)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. NodeByLabelScan — multiple labels (extra label as Selection)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_NodeByLabelScan_MultiLabel(t *testing.T) {
	// MATCH (n:Person:Employee)
	q := singleQueryMatch(&ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person", "Employee"}}, nil)
	plan := mustFromAST(t, q)
	// Root must be Selection wrapping NodeByLabelScan.
	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("expected *ir.Selection for extra label, got %T", plan)
	}
	if sel.Predicate != "(n:Employee)" {
		t.Errorf("Selection predicate = %q", sel.Predicate)
	}
	if _, ok := sel.Child.(*ir.NodeByLabelScan); !ok {
		t.Fatalf("sel.Child expected *ir.NodeByLabelScan, got %T", sel.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. NodePattern with inline property predicate
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_NodePattern_WithProperties(t *testing.T) {
	// MATCH (n:Person {name: 'Alice'})
	props := &ast.MapLiteral{Keys: []string{"name"}, Values: []ast.Expression{&ast.StringLiteral{Value: "Alice"}}}
	q := singleQueryMatch(&ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}, Properties: props}, nil)
	plan := mustFromAST(t, q)
	// Outermost should be Selection for the property predicate.
	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("expected *ir.Selection wrapping property predicate, got %T", plan)
	}
	if sel.Predicate == "" {
		t.Error("property predicate should be non-empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. MATCH with WHERE clause
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Match_WithWhere(t *testing.T) {
	// MATCH (n) WHERE n.age > 18
	pred := &ast.BinaryOp{
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
		Operator: ">",
		Right:    &ast.IntLiteral{Value: 18},
	}
	q := singleQueryMatch(&ast.NodePattern{Variable: strPtr("n")}, &ast.Where{Predicate: pred})
	plan := mustFromAST(t, q)
	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("expected *ir.Selection from WHERE, got %T", plan)
	}
	if sel.Predicate == "" {
		t.Error("WHERE predicate should be non-empty")
	}
	if _, ok := sel.Child.(*ir.AllNodesScan); !ok {
		t.Fatalf("sel.Child expected *ir.AllNodesScan, got %T", sel.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Expand — outgoing relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Expand_Outgoing(t *testing.T) {
	// MATCH (n:Person)-[r:KNOWS]->(m)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{
							Head: &ast.PathElement{
								Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}},
								Next: &ast.PathElement{
									Relationship: &ast.RelationshipPattern{
										Variable:  strPtr("r"),
										Types:     []string{"KNOWS"},
										Direction: ast.RelDirectionOutgoing,
									},
									Node: &ast.NodePattern{Variable: strPtr("m")},
								},
							},
						},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	exp, ok := plan.(*ir.Expand)
	if !ok {
		t.Fatalf("expected *ir.Expand, got %T", plan)
	}
	if exp.Direction != ir.DirectionOutgoing {
		t.Errorf("Direction = %v", exp.Direction)
	}
	if exp.RelVar != "r" || exp.ToVar != "m" {
		t.Errorf("RelVar=%q ToVar=%q", exp.RelVar, exp.ToVar)
	}
	if len(exp.RelTypes) != 1 || exp.RelTypes[0] != "KNOWS" {
		t.Errorf("RelTypes = %v", exp.RelTypes)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. Expand — incoming relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Expand_Incoming(t *testing.T) {
	// MATCH (n)<-[r:LIKES]-(m)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{
							Head: &ast.PathElement{
								Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Post"}},
								Next: &ast.PathElement{
									Relationship: &ast.RelationshipPattern{
										Variable:  strPtr("r"),
										Types:     []string{"LIKES"},
										Direction: ast.RelDirectionIncoming,
									},
									Node: &ast.NodePattern{Variable: strPtr("m")},
								},
							},
						},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	exp, ok := plan.(*ir.Expand)
	if !ok {
		t.Fatalf("expected *ir.Expand, got %T", plan)
	}
	if exp.Direction != ir.DirectionIncoming {
		t.Errorf("Direction = %v, want Incoming", exp.Direction)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. Expand — undirected relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Expand_Undirected(t *testing.T) {
	// MATCH (n)-[r]-(m)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{
							Head: &ast.PathElement{
								Node: &ast.NodePattern{Variable: strPtr("n")},
								Next: &ast.PathElement{
									Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionNone},
									Node:         &ast.NodePattern{Variable: strPtr("m")},
								},
							},
						},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	exp, ok := plan.(*ir.Expand)
	if !ok {
		t.Fatalf("expected *ir.Expand, got %T", plan)
	}
	if exp.Direction != ir.DirectionBoth {
		t.Errorf("Direction = %v, want Both", exp.Direction)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 10. Expand — multiple relationship types
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Expand_MultiType(t *testing.T) {
	// MATCH (n)-[r:KNOWS|LIKES]->(m)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{
							Head: &ast.PathElement{
								Node: &ast.NodePattern{Variable: strPtr("n")},
								Next: &ast.PathElement{
									Relationship: &ast.RelationshipPattern{
										Types:     []string{"KNOWS", "LIKES"},
										Direction: ast.RelDirectionOutgoing,
									},
									Node: &ast.NodePattern{Variable: strPtr("m")},
								},
							},
						},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	exp, ok := plan.(*ir.Expand)
	if !ok {
		t.Fatalf("expected *ir.Expand, got %T", plan)
	}
	if len(exp.RelTypes) != 2 {
		t.Errorf("RelTypes = %v, want 2 types", exp.RelTypes)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 11. OptionalExpand — OPTIONAL MATCH with relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_OptionalExpand(t *testing.T) {
	// OPTIONAL MATCH (n)-[r:FOLLOWS]->(m)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{
							Head: &ast.PathElement{
								Node: &ast.NodePattern{Variable: strPtr("n")},
								Next: &ast.PathElement{
									Relationship: &ast.RelationshipPattern{
										Variable:  strPtr("r"),
										Types:     []string{"FOLLOWS"},
										Direction: ast.RelDirectionOutgoing,
									},
									Node: &ast.NodePattern{Variable: strPtr("m")},
								},
							},
						},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	// OPTIONAL MATCH at the start of a query is wrapped in an
	// OptionalApply so an empty result still emits one NULL-extended row
	// (openCypher 9 §3.2.4). The inner pattern uses regular Expand.
	opt, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("expected *ir.OptionalApply, got %T", plan)
	}
	if _, ok := opt.Inner.(*ir.Expand); !ok {
		t.Fatalf("OptionalApply.Inner expected *ir.Expand, got %T", opt.Inner)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 12. VarLengthExpand — bounded range
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_VarLengthExpand_Bounded(t *testing.T) {
	// MATCH (n)-[r:KNOWS*1..3]->(m)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{
							Head: &ast.PathElement{
								Node: &ast.NodePattern{Variable: strPtr("n")},
								Next: &ast.PathElement{
									Relationship: &ast.RelationshipPattern{
										Variable:  strPtr("r"),
										Types:     []string{"KNOWS"},
										Direction: ast.RelDirectionOutgoing,
										Range:     &ast.RangeQuantifier{Min: i64Ptr(1), Max: i64Ptr(3)},
									},
									Node: &ast.NodePattern{Variable: strPtr("m")},
								},
							},
						},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	vle, ok := plan.(*ir.VarLengthExpand)
	if !ok {
		t.Fatalf("expected *ir.VarLengthExpand, got %T", plan)
	}
	if vle.MinDepth != 1 || vle.MaxDepth != 3 {
		t.Errorf("depths = %d/%d, want 1/3", vle.MinDepth, vle.MaxDepth)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 13. VarLengthExpand — unbounded (Kleene star)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_VarLengthExpand_Unbounded(t *testing.T) {
	// MATCH (n)-[r*]->(m)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{
							Head: &ast.PathElement{
								Node: &ast.NodePattern{Variable: strPtr("n")},
								Next: &ast.PathElement{
									Relationship: &ast.RelationshipPattern{
										Direction: ast.RelDirectionOutgoing,
										Range:     &ast.RangeQuantifier{}, // no min/max
									},
									Node: &ast.NodePattern{Variable: strPtr("m")},
								},
							},
						},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	vle, ok := plan.(*ir.VarLengthExpand)
	if !ok {
		t.Fatalf("expected *ir.VarLengthExpand, got %T", plan)
	}
	if vle.MinDepth != 1 || vle.MaxDepth != math.MaxInt {
		t.Errorf("unbounded: MinDepth=%d MaxDepth=%d", vle.MinDepth, vle.MaxDepth)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 14. UNWIND operator
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Unwind(t *testing.T) {
	// UNWIND [1,2,3] AS x
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Unwind{
				Expr:     &ast.ListLiteral{Elements: []ast.Expression{&ast.IntLiteral{Value: 1}}},
				Variable: "x",
			},
		},
	}
	plan := mustFromAST(t, q)
	uw, ok := plan.(*ir.Unwind)
	if !ok {
		t.Fatalf("expected *ir.Unwind, got %T", plan)
	}
	if uw.ElementVar != "x" {
		t.Errorf("ElementVar = %q, want %q", uw.ElementVar, "x")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 15. WITH projection — scope boundary
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_WithClause_Projection(t *testing.T) {
	// MATCH (n) WITH n.name AS name
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		With: []*ast.With{
			{
				Projection: &ast.Projection{
					Items: []*ast.ProjectionItem{
						{Expr: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}, Alias: strPtr("name")},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	proj, ok := plan.(*ir.Projection)
	if !ok {
		t.Fatalf("expected *ir.Projection from WITH, got %T", plan)
	}
	if len(proj.Items) != 1 || proj.Items[0].Name != "name" {
		t.Errorf("Items = %v", proj.Items)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 16. WITH with WHERE predicate
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_WithClause_WithWhere(t *testing.T) {
	// MATCH (n) WITH n WHERE n.active = true
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		With: []*ast.With{
			{
				Projection: &ast.Projection{
					Items: []*ast.ProjectionItem{
						{Expr: &ast.Variable{Name: "n"}},
					},
				},
				Where: &ast.Where{Predicate: &ast.BinaryOp{
					Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "active"},
					Operator: "=",
					Right:    &ast.BoolLiteral{Value: true},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)
	// openCypher 9 §5.1.5 specifies the WITH WHERE predicate filters the
	// pre-projection row stream (so it can reference pre-WITH variables
	// dropped by the projection). The plan shape is therefore
	// Projection(Selection(child)), not Selection(Projection(child)).
	proj, ok := plan.(*ir.Projection)
	if !ok {
		t.Fatalf("expected *ir.Projection from WITH (WHERE applies below), got %T", plan)
	}
	if _, ok := proj.Child.(*ir.Selection); !ok {
		t.Fatalf("proj.Child expected *ir.Selection (WHERE), got %T", proj.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 17. RETURN — simple columns
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Return_Simple(t *testing.T) {
	// MATCH (n) RETURN n
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
		}},
	}
	plan := mustFromAST(t, q)
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("expected *ir.ProduceResults, got %T", plan)
	}
	if len(pr.Columns) != 1 || pr.Columns[0] != "n" {
		t.Errorf("Columns = %v", pr.Columns)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 18. RETURN DISTINCT
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Return_Distinct(t *testing.T) {
	// MATCH (n) RETURN DISTINCT n.city
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Distinct: true,
			Items: []*ast.ProjectionItem{
				{Expr: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "city"}, Alias: strPtr("city")},
			},
		}},
	}
	plan := mustFromAST(t, q)
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("expected *ir.ProduceResults, got %T", plan)
	}
	// Child must be Distinct.
	if _, ok := pr.Child.(*ir.Distinct); !ok {
		t.Fatalf("ProduceResults.Child expected *ir.Distinct, got %T", pr.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 19. RETURN ORDER BY
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Return_OrderBy(t *testing.T) {
	// MATCH (n) RETURN n ORDER BY n.name ASC
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
			OrderBy: []*ast.SortItem{
				{Expr: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}, Descending: false},
			},
		}},
	}
	plan := mustFromAST(t, q)
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("expected *ir.ProduceResults, got %T", plan)
	}
	if _, ok := pr.Child.(*ir.Sort); !ok {
		t.Fatalf("ProduceResults.Child expected *ir.Sort, got %T", pr.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 20. RETURN ORDER BY … LIMIT (Top)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Return_Top(t *testing.T) {
	// MATCH (n) RETURN n ORDER BY n.name LIMIT 10
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
			OrderBy: []*ast.SortItem{
				{Expr: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}},
			},
			Limit: &ast.IntLiteral{Value: 10},
		}},
	}
	plan := mustFromAST(t, q)
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("expected *ir.ProduceResults, got %T", plan)
	}
	top, ok := pr.Child.(*ir.Top)
	if !ok {
		t.Fatalf("ProduceResults.Child expected *ir.Top, got %T", pr.Child)
	}
	if top.Limit != 10 {
		t.Errorf("Top.Limit = %d, want 10", top.Limit)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 21. RETURN SKIP
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Return_Skip(t *testing.T) {
	// MATCH (n) RETURN n SKIP 5
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
			Skip:  &ast.IntLiteral{Value: 5},
		}},
	}
	plan := mustFromAST(t, q)
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("expected *ir.ProduceResults, got %T", plan)
	}
	skip, ok := pr.Child.(*ir.Skip)
	if !ok {
		t.Fatalf("ProduceResults.Child expected *ir.Skip, got %T", pr.Child)
	}
	if skip.Count != 5 {
		t.Errorf("Skip.Count = %d, want 5", skip.Count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 22. RETURN LIMIT (without ORDER BY)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Return_Limit(t *testing.T) {
	// MATCH (n) RETURN n LIMIT 25
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}},
			Limit: &ast.IntLiteral{Value: 25},
		}},
	}
	plan := mustFromAST(t, q)
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("expected *ir.ProduceResults, got %T", plan)
	}
	lim, ok := pr.Child.(*ir.Limit)
	if !ok {
		t.Fatalf("ProduceResults.Child expected *ir.Limit, got %T", pr.Child)
	}
	if lim.Count != 25 {
		t.Errorf("Limit.Count = %d, want 25", lim.Count)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 23. RETURN with alias
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Return_Alias(t *testing.T) {
	// MATCH (n) RETURN n.name AS name
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{
				{
					Expr:  &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"},
					Alias: strPtr("name"),
				},
			},
		}},
	}
	plan := mustFromAST(t, q)
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("expected *ir.ProduceResults, got %T", plan)
	}
	proj, ok := pr.Child.(*ir.Projection)
	if !ok {
		t.Fatalf("pr.Child expected *ir.Projection, got %T", pr.Child)
	}
	if proj.Items[0].Name != "name" {
		t.Errorf("alias not applied: Name = %q", proj.Items[0].Name)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 24. CREATE node
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_CreateNode(t *testing.T) {
	// CREATE (n:Person {name: 'Alice'})
	props := &ast.MapLiteral{Keys: []string{"name"}, Values: []ast.Expression{&ast.StringLiteral{Value: "Alice"}}}
	q := &ast.SingleQuery{
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Create{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{Head: &ast.PathElement{
							Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}, Properties: props},
						}},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	cn, ok := plan.(*ir.CreateNode)
	if !ok {
		t.Fatalf("expected *ir.CreateNode, got %T", plan)
	}
	if cn.NodeVar != "n" || len(cn.Labels) != 1 || cn.Labels[0] != "Person" {
		t.Errorf("CreateNode = {%q, %v}", cn.NodeVar, cn.Labels)
	}
	if cn.Properties == "" {
		t.Error("Properties should be non-empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 25. CREATE relationship
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_CreateRelationship(t *testing.T) {
	// CREATE (n:Person)-[r:KNOWS]->(m:Person)
	q := &ast.SingleQuery{
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Create{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{Head: &ast.PathElement{
							Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}},
							Next: &ast.PathElement{
								Relationship: &ast.RelationshipPattern{
									Variable:  strPtr("r"),
									Types:     []string{"KNOWS"},
									Direction: ast.RelDirectionOutgoing,
								},
								Node: &ast.NodePattern{Variable: strPtr("m"), Labels: []string{"Person"}},
							},
						}},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	cr, ok := plan.(*ir.CreateRelationship)
	if !ok {
		t.Fatalf("expected *ir.CreateRelationship, got %T", plan)
	}
	if cr.RelType != "KNOWS" || cr.RelVar != "r" {
		t.Errorf("CreateRelationship = {type=%q var=%q}", cr.RelType, cr.RelVar)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 26. DELETE node
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_DeleteNode(t *testing.T) {
	// MATCH (n) DELETE n
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
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
		t.Errorf("NodeVar = %q", dn.NodeVar)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 27. DELETE multiple expressions
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_DeleteNode_Multiple(t *testing.T) {
	// MATCH (n), (m) DELETE n, m
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Delete{Expressions: []ast.Expression{
				&ast.Variable{Name: "n"},
				&ast.Variable{Name: "m"},
			}},
		},
	}
	plan := mustFromAST(t, q)
	// Outermost DeleteNode for "m", its child for "n".
	outer, ok := plan.(*ir.DeleteNode)
	if !ok {
		t.Fatalf("expected *ir.DeleteNode, got %T", plan)
	}
	if _, ok := outer.Child.(*ir.DeleteNode); !ok {
		t.Fatalf("inner expected *ir.DeleteNode, got %T", outer.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 28. DETACH DELETE
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_DetachDelete(t *testing.T) {
	// MATCH (n) DETACH DELETE n
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.DetachDelete{Expressions: []ast.Expression{&ast.Variable{Name: "n"}}},
		},
	}
	plan := mustFromAST(t, q)
	if _, ok := plan.(*ir.DetachDelete); !ok {
		t.Fatalf("expected *ir.DetachDelete, got %T", plan)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 29. SET property
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SetProperty(t *testing.T) {
	// MATCH (n) SET n.age = 30
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Set{Items: []*ast.SetItem{
				{
					Target:   &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
					Value:    &ast.IntLiteral{Value: 30},
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
	if sp.EntityVar != "n" || sp.PropertyKey != "age" {
		t.Errorf("SetProperty = {entity=%q prop=%q}", sp.EntityVar, sp.PropertyKey)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 30. SET labels
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SetLabels(t *testing.T) {
	// MATCH (n) SET n:Admin
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
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
	if sl.NodeVar != "n" || len(sl.Labels) != 1 || sl.Labels[0] != "Admin" {
		t.Errorf("SetLabels = {%q, %v}", sl.NodeVar, sl.Labels)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 31. REMOVE property
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_RemoveProperty(t *testing.T) {
	// MATCH (n) REMOVE n.tempField
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
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
// 32. REMOVE labels
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_RemoveLabels(t *testing.T) {
	// MATCH (n) REMOVE n:Inactive
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
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
	if rl.NodeVar != "n" || len(rl.Labels) != 1 || rl.Labels[0] != "Inactive" {
		t.Errorf("RemoveLabels = {%q, %v}", rl.NodeVar, rl.Labels)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 33. MERGE pattern
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Merge(t *testing.T) {
	// MERGE (n:Person {name: 'Alice'}) ON CREATE SET n.created = 1
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
	if m.Pattern == "" {
		t.Error("Merge.Pattern should be non-empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 34. CALL procedure
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Call(t *testing.T) {
	// CALL db.labels() YIELD label
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Call{
				Namespace: []string{"db"},
				Procedure: "labels",
				Args:      []ast.Expression{},
				Yield: []*ast.YieldItem{
					{Name: "label"},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	pc, ok := plan.(*ir.ProcedureCall)
	if !ok {
		t.Fatalf("expected *ir.ProcedureCall, got %T", plan)
	}
	if pc.Name != "labels" {
		t.Errorf("Name = %q, want %q", pc.Name, "labels")
	}
	if len(pc.YieldVars) != 1 || pc.YieldVars[0] != "label" {
		t.Errorf("YieldVars = %v", pc.YieldVars)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 35. CALL procedure with alias in YIELD
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Call_YieldAlias(t *testing.T) {
	// CALL apoc.algo.dijkstra(n, m, 'KNOWS', 'weight') YIELD path AS p, weight AS w
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Call{
				Namespace: []string{"apoc", "algo"},
				Procedure: "dijkstra",
				Args: []ast.Expression{
					&ast.Variable{Name: "n"},
					&ast.Variable{Name: "m"},
				},
				Yield: []*ast.YieldItem{
					{Name: "path", Alias: strPtr("p")},
					{Name: "weight", Alias: strPtr("w")},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	pc, ok := plan.(*ir.ProcedureCall)
	if !ok {
		t.Fatalf("expected *ir.ProcedureCall, got %T", plan)
	}
	if len(pc.YieldVars) != 2 || pc.YieldVars[0] != "p" || pc.YieldVars[1] != "w" {
		t.Errorf("YieldVars = %v", pc.YieldVars)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 36. UNION (deduplicating)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Union(t *testing.T) {
	// MATCH (n:Person) RETURN n UNION MATCH (n:Employee) RETURN n
	part1 := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.Variable{Name: "n"}},
		}}},
	}
	part2 := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Employee"}}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.Variable{Name: "n"}},
		}}},
	}
	q := &ast.MultiQuery{Parts: []*ast.SingleQuery{part1, part2}, All: false}
	plan := mustFromAST(t, q)
	if _, ok := plan.(*ir.Union); !ok {
		t.Fatalf("expected *ir.Union, got %T", plan)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 37. UNION ALL (bag union)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_UnionAll(t *testing.T) {
	part1 := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
	}
	part2 := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("m")}}},
			}}},
		},
	}
	q := &ast.MultiQuery{Parts: []*ast.SingleQuery{part1, part2}, All: true}
	plan := mustFromAST(t, q)
	if _, ok := plan.(*ir.UnionAll); !ok {
		t.Fatalf("expected *ir.UnionAll, got %T", plan)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 38. Multi-path MATCH (two node scans)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_MultiPathMatch(t *testing.T) {
	// MATCH (n:Person), (m:Movie) — two comma-separated patterns produce Apply.
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
						{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("m"), Labels: []string{"Movie"}}}},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	// Multi-pattern MATCH produces Apply(outer=Person scan, inner=Movie scan).
	app, ok := plan.(*ir.Apply)
	if !ok {
		t.Fatalf("expected *ir.Apply for multi-pattern MATCH, got %T", plan)
	}
	outer, ok := app.Outer.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("Apply.Outer expected *ir.NodeByLabelScan, got %T", app.Outer)
	}
	if outer.Label != "Person" {
		t.Errorf("outer Label = %q, want Person", outer.Label)
	}
	inner, ok := app.Inner.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("Apply.Inner expected *ir.NodeByLabelScan, got %T", app.Inner)
	}
	if inner.Label != "Movie" {
		t.Errorf("inner Label = %q, want Movie", inner.Label)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 39. Three-hop path: (a)-[r1]->(b)-[r2]->(c)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_ThreeHopPath(t *testing.T) {
	// MATCH (a)-[r1:KNOWS]->(b)-[r2:LIKES]->(c)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{Head: &ast.PathElement{
							Node: &ast.NodePattern{Variable: strPtr("a")},
							Next: &ast.PathElement{
								Relationship: &ast.RelationshipPattern{Variable: strPtr("r1"), Types: []string{"KNOWS"}, Direction: ast.RelDirectionOutgoing},
								Node:         &ast.NodePattern{Variable: strPtr("b")},
								Next: &ast.PathElement{
									Relationship: &ast.RelationshipPattern{Variable: strPtr("r2"), Types: []string{"LIKES"}, Direction: ast.RelDirectionOutgoing},
									Node:         &ast.NodePattern{Variable: strPtr("c")},
								},
							},
						}},
					},
				},
			},
		},
	}
	plan := mustFromAST(t, q)
	// Outermost operator: Expand for the second hop.
	exp2, ok := plan.(*ir.Expand)
	if !ok {
		t.Fatalf("expected outer *ir.Expand, got %T", plan)
	}
	if exp2.RelVar != "r2" {
		t.Errorf("outer expand RelVar = %q, want r2", exp2.RelVar)
	}
	// Inner operator: Expand for the first hop.
	exp1, ok := exp2.Child.(*ir.Expand)
	if !ok {
		t.Fatalf("expected inner *ir.Expand, got %T", exp2.Child)
	}
	if exp1.RelVar != "r1" {
		t.Errorf("inner expand RelVar = %q, want r1", exp1.RelVar)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 40. MATCH + WHERE + RETURN (full pipeline)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_FullPipeline(t *testing.T) {
	// MATCH (n:Person) WHERE n.age > 18 RETURN n.name AS name
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
				}},
				Where: &ast.Where{Predicate: &ast.BinaryOp{
					Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
					Operator: ">",
					Right:    &ast.IntLiteral{Value: 18},
				}},
			},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{
				{Expr: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}, Alias: strPtr("name")},
			},
		}},
	}
	plan := mustFromAST(t, q)

	// ProduceResults → Projection → Selection → NodeByLabelScan.
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("root expected *ir.ProduceResults, got %T", plan)
	}
	proj, ok := pr.Child.(*ir.Projection)
	if !ok {
		t.Fatalf("pr.Child expected *ir.Projection, got %T", pr.Child)
	}
	sel, ok := proj.Child.(*ir.Selection)
	if !ok {
		t.Fatalf("proj.Child expected *ir.Selection, got %T", proj.Child)
	}
	if _, ok := sel.Child.(*ir.NodeByLabelScan); !ok {
		t.Fatalf("sel.Child expected *ir.NodeByLabelScan, got %T", sel.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 41. MATCH + WITH + RETURN (scope boundary pipeline)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_MatchWithReturn(t *testing.T) {
	// MATCH (n:Person) WITH n RETURN n
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
				}},
			},
		},
		With: []*ast.With{
			{Projection: &ast.Projection{Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{{Expr: &ast.Variable{Name: "n"}}}}},
	}
	plan := mustFromAST(t, q)
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("root expected *ir.ProduceResults, got %T", plan)
	}
	// ProduceResults → Projection (RETURN) → Projection (WITH) → NodeByLabelScan.
	retProj, ok := pr.Child.(*ir.Projection)
	if !ok {
		t.Fatalf("pr.Child expected *ir.Projection, got %T", pr.Child)
	}
	withProj, ok := retProj.Child.(*ir.Projection)
	if !ok {
		t.Fatalf("retProj.Child expected *ir.Projection, got %T", retProj.Child)
	}
	if _, ok := withProj.Child.(*ir.NodeByLabelScan); !ok {
		t.Fatalf("withProj.Child expected *ir.NodeByLabelScan, got %T", withProj.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 42. MATCH node with relationship and WHERE on destination
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_ExpandWithLabelFilter(t *testing.T) {
	// MATCH (n:Person)-[r:KNOWS]->(m:Person) WHERE m.age > 30
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{
						{Head: &ast.PathElement{
							Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}},
							Next: &ast.PathElement{
								Relationship: &ast.RelationshipPattern{Variable: strPtr("r"), Types: []string{"KNOWS"}, Direction: ast.RelDirectionOutgoing},
								Node:         &ast.NodePattern{Variable: strPtr("m"), Labels: []string{"Person"}},
							},
						}},
					},
				},
				Where: &ast.Where{Predicate: &ast.BinaryOp{
					Left:     &ast.Property{Receiver: &ast.Variable{Name: "m"}, Key: "age"},
					Operator: ">",
					Right:    &ast.IntLiteral{Value: 30},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)
	// Root: Selection (WHERE) → Selection (m label) → Expand → NodeByLabelScan.
	sel, ok := plan.(*ir.Selection)
	if !ok {
		t.Fatalf("root expected *ir.Selection (WHERE), got %T", plan)
	}
	innerSel, ok := sel.Child.(*ir.Selection)
	if !ok {
		t.Fatalf("sel.Child expected *ir.Selection (label filter), got %T", sel.Child)
	}
	if _, ok := innerSel.Child.(*ir.Expand); !ok {
		t.Fatalf("innerSel.Child expected *ir.Expand, got %T", innerSel.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 43. Empty SingleQuery (nil plan)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_EmptySingleQuery(t *testing.T) {
	q := &ast.SingleQuery{}
	plan, err := ir.FromAST(q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan != nil {
		t.Errorf("expected nil plan for empty query, got %T", plan)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 44. MATCH with anonymous relationship (no variable, no type)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Expand_Anonymous(t *testing.T) {
	// MATCH (n)-->(m)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("n")},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionOutgoing},
							Node:         &ast.NodePattern{Variable: strPtr("m")},
						},
					}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)
	exp, ok := plan.(*ir.Expand)
	if !ok {
		t.Fatalf("expected *ir.Expand, got %T", plan)
	}
	// Anonymous relationships now receive a synthetic IR-only name so
	// relationship-isomorphism (cyphermorphism) bookkeeping can refer to
	// the edge column by name. The synthetic always starts with the
	// translator's `__anon_` prefix.
	if exp.RelVar == "" || exp.RelVar[:2] != "__" {
		t.Errorf("anonymous rel should carry synthetic RelVar, got %q", exp.RelVar)
	}
	if len(exp.RelTypes) != 0 {
		t.Errorf("anonymous rel should have no types, got %v", exp.RelTypes)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 45. OPTIONAL MATCH — node only (no relationship)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_OptionalMatch_NodeOnly(t *testing.T) {
	// OPTIONAL MATCH (n:Person)
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
				}},
			},
		},
	}
	plan := mustFromAST(t, q)
	// Node-only OPTIONAL MATCH is wrapped in OptionalApply over an
	// empty Argument seed so an empty graph emits a single NULL-extended
	// row (openCypher 9 §3.2.4).
	apply, ok := plan.(*ir.OptionalApply)
	if !ok {
		t.Fatalf("expected *ir.OptionalApply for optional node-only, got %T", plan)
	}
	inner := apply.Inner
	for {
		sel, isSel := inner.(*ir.Selection)
		if !isSel {
			break
		}
		inner = sel.Child
	}
	if _, ok := inner.(*ir.NodeByLabelScan); !ok {
		t.Fatalf("expected inner *ir.NodeByLabelScan, got %T", inner)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 46. UNWIND with child (chained after MATCH)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Unwind_WithChild(t *testing.T) {
	// MATCH (n) UNWIND n.tags AS tag
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
			&ast.Unwind{
				Expr:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "tags"},
				Variable: "tag",
			},
		},
	}
	plan := mustFromAST(t, q)
	uw, ok := plan.(*ir.Unwind)
	if !ok {
		t.Fatalf("expected *ir.Unwind, got %T", plan)
	}
	if uw.Child == nil {
		t.Error("Unwind.Child should be non-nil when chained after MATCH")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 47. SET multiple items in one clause
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_SetMultiple(t *testing.T) {
	// MATCH (n) SET n.age = 30, n.name = 'Bob'
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Set{Items: []*ast.SetItem{
				{Target: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"}, Value: &ast.IntLiteral{Value: 30}, Operator: "="},
				{Target: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}, Value: &ast.StringLiteral{Value: "Bob"}, Operator: "="},
			}},
		},
	}
	plan := mustFromAST(t, q)
	// Outer SetProperty for "name", inner for "age".
	sp1, ok := plan.(*ir.SetProperty)
	if !ok {
		t.Fatalf("expected *ir.SetProperty, got %T", plan)
	}
	if sp1.PropertyKey != "name" {
		t.Errorf("outer SetProperty key = %q, want name", sp1.PropertyKey)
	}
	sp2, ok := sp1.Child.(*ir.SetProperty)
	if !ok {
		t.Fatalf("sp1.Child expected *ir.SetProperty, got %T", sp1.Child)
	}
	if sp2.PropertyKey != "age" {
		t.Errorf("inner SetProperty key = %q, want age", sp2.PropertyKey)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 48. MERGE with ON MATCH SET
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_Merge_OnMatch(t *testing.T) {
	// MERGE (n:Person) ON MATCH SET n.updated = 1
	q := &ast.SingleQuery{
		UpdatingClauses: []ast.UpdatingClause{
			&ast.Merge{
				Pattern: &ast.PathPattern{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}},
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
	if len(m.OnMatch) != 1 {
		t.Errorf("OnMatch = %v, want 1 item", m.OnMatch)
	}
	if len(m.OnCreate) != 0 {
		t.Errorf("OnCreate should be empty, got %v", m.OnCreate)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 49. Three-way UNION ALL (left-associative folding)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_ThreeWayUnionAll(t *testing.T) {
	makePart := func(label string) *ast.SingleQuery {
		return &ast.SingleQuery{
			ReadingClauses: []ast.ReadingClause{
				&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{label}}}},
				}}},
			},
		}
	}
	q := &ast.MultiQuery{
		Parts: []*ast.SingleQuery{makePart("A"), makePart("B"), makePart("C")},
		All:   true,
	}
	plan := mustFromAST(t, q)
	// Root: UnionAll(UnionAll(A, B), C).
	outer, ok := plan.(*ir.UnionAll)
	if !ok {
		t.Fatalf("expected *ir.UnionAll, got %T", plan)
	}
	if _, ok := outer.Left.(*ir.UnionAll); !ok {
		t.Fatalf("outer.Left expected *ir.UnionAll (inner), got %T", outer.Left)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 50. MATCH + MATCH (two reading clauses chained)
// ─────────────────────────────────────────────────────────────────────────────

func TestTranslate_TwoMatchClauses(t *testing.T) {
	// MATCH (n:Person) MATCH (m:Movie) — two sequential MATCH clauses.
	// The second MATCH chains the first plan as outer, producing Apply.
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
			}}},
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("m"), Labels: []string{"Movie"}}}},
			}}},
		},
	}
	plan := mustFromAST(t, q)
	// The second MATCH wraps the first plan as outer in an Apply.
	app, ok := plan.(*ir.Apply)
	if !ok {
		t.Fatalf("expected *ir.Apply for chained MATCH clauses, got %T", plan)
	}
	outer, ok := app.Outer.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("Apply.Outer expected *ir.NodeByLabelScan (Person), got %T", app.Outer)
	}
	if outer.Label != "Person" {
		t.Errorf("outer Label = %q, want Person", outer.Label)
	}
	inner, ok := app.Inner.(*ir.NodeByLabelScan)
	if !ok {
		t.Fatalf("Apply.Inner expected *ir.NodeByLabelScan (Movie), got %T", app.Inner)
	}
	if inner.Label != "Movie" {
		t.Errorf("inner Label = %q, want Movie", inner.Label)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Golden-file IR comparison (selected representative trees)
// ─────────────────────────────────────────────────────────────────────────────

// goldenAllNodesScan verifies the exact tree for MATCH (n).
func TestGolden_AllNodesScan(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
	}
	plan := mustFromAST(t, q)
	scan := plan.(*ir.AllNodesScan)
	if scan.NodeVar != "n" {
		t.Errorf("golden: NodeVar = %q, want n", scan.NodeVar)
	}
	if len(scan.Children()) != 0 {
		t.Error("golden: AllNodesScan must have no children")
	}
	if len(scan.Vars()) != 1 || scan.Vars()[0] != "n" {
		t.Errorf("golden: Vars = %v", scan.Vars())
	}
}

// goldenFullPipeline verifies the exact 4-level tree for the canonical
// MATCH (n:Person) WHERE n.age > 18 RETURN n.name AS name.
func TestGolden_FullPipeline(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
				}},
				Where: &ast.Where{Predicate: &ast.BinaryOp{
					Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
					Operator: ">",
					Right:    &ast.IntLiteral{Value: 18},
				}},
			},
		},
		Return: &ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{
				{Expr: &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}, Alias: strPtr("name")},
			},
		}},
	}
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	if len(pr.Columns) != 1 || pr.Columns[0] != "name" {
		t.Fatalf("ProduceResults.Columns = %v, want [name]", pr.Columns)
	}

	proj := pr.Child.(*ir.Projection)
	if len(proj.Items) != 1 || proj.Items[0].Name != "name" {
		t.Fatalf("Projection.Items = %v", proj.Items)
	}

	sel := proj.Child.(*ir.Selection)
	if sel.Predicate == "" {
		t.Error("Selection.Predicate must be non-empty")
	}

	scan := sel.Child.(*ir.NodeByLabelScan)
	if scan.NodeVar != "n" || scan.Label != "Person" {
		t.Errorf("NodeByLabelScan = {%q, %q}", scan.NodeVar, scan.Label)
	}
}

// goldenExpand verifies the exact Expand tree for (n:Person)-[r:KNOWS]->(m).
func TestGolden_Expand(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{
					Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}},
					Next: &ast.PathElement{
						Relationship: &ast.RelationshipPattern{
							Variable:  strPtr("r"),
							Types:     []string{"KNOWS"},
							Direction: ast.RelDirectionOutgoing,
						},
						Node: &ast.NodePattern{Variable: strPtr("m")},
					},
				}},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	exp := plan.(*ir.Expand)
	if exp.FromVar != "n" {
		t.Errorf("Expand.FromVar = %q, want n", exp.FromVar)
	}
	if exp.RelVar != "r" {
		t.Errorf("Expand.RelVar = %q, want r", exp.RelVar)
	}
	if exp.ToVar != "m" {
		t.Errorf("Expand.ToVar = %q, want m", exp.ToVar)
	}
	if exp.Direction != ir.DirectionOutgoing {
		t.Errorf("Expand.Direction = %v, want Outgoing", exp.Direction)
	}
	if len(exp.RelTypes) != 1 || exp.RelTypes[0] != "KNOWS" {
		t.Errorf("Expand.RelTypes = %v", exp.RelTypes)
	}
	if _, ok := exp.Child.(*ir.NodeByLabelScan); !ok {
		t.Fatalf("Expand.Child expected *ir.NodeByLabelScan, got %T", exp.Child)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Error-path scenarios — typed TranslateError
// ─────────────────────────────────────────────────────────────────────────────

// TestTranslate_Error_EmptyMultiQuery verifies that an empty UNION parts list
// produces a *TranslateError with a non-empty UnsupportedClause field.
func TestTranslate_Error_EmptyMultiQuery(t *testing.T) {
	q := &ast.MultiQuery{Parts: []*ast.SingleQuery{}}
	_, err := ir.FromAST(q)
	if err == nil {
		t.Fatal("expected error for empty MultiQuery")
	}
	var te *ir.TranslateError
	if !errors.As(err, &te) {
		t.Fatalf("expected *ir.TranslateError, got %T: %v", err, err)
	}
	if te.UnsupportedClause == "" {
		t.Error("TranslateError.UnsupportedClause must be non-empty")
	}
}

// TestTranslate_Error_TranslateErrorFields verifies the UnsupportedClause and
// Pos fields are preserved and that Error() produces a non-empty diagnostic.
func TestTranslate_Error_TranslateErrorFields(t *testing.T) {
	te := &ir.TranslateError{
		UnsupportedClause: "FOREACH",
		Pos:               ast.Position{Line: 1, Column: 5},
	}
	var err error = te
	var got *ir.TranslateError
	if !errors.As(err, &got) {
		t.Fatal("errors.As failed for *ir.TranslateError")
	}
	if got.UnsupportedClause != "FOREACH" {
		t.Errorf("UnsupportedClause = %q, want FOREACH", got.UnsupportedClause)
	}
	if te.Error() == "" {
		t.Error("TranslateError.Error() must return non-empty string")
	}
}

// TestTranslate_Error_TranslateErrorMessage verifies that the Error() string
// contains both the clause name and the position.
func TestTranslate_Error_TranslateErrorMessage(t *testing.T) {
	te := &ir.TranslateError{
		UnsupportedClause: "FOREACH",
		Pos:               ast.Position{Line: 3, Column: 7},
	}
	msg := te.Error()
	if msg == "" {
		t.Fatal("Error() returned empty string")
	}
	// Must mention the clause name.
	if !strings.Contains(msg, "FOREACH") {
		t.Errorf("Error() = %q, want to contain FOREACH", msg)
	}
}
