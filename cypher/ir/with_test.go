package ir_test

// with_test.go — golden IR tests for WITH pipeline-boundary translation (task-222).

import (
	"testing"

	"gograph/cypher/ast"
	"gograph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// 1. WITH n — plain variable pass-through
//    → Projection([n], child)
// ─────────────────────────────────────────────────────────────────────────────

func Test_With_PlainVariable(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		With: []*ast.With{
			{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
				{Expr: &ast.Variable{Name: "n"}},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	proj, ok := plan.(*ir.Projection)
	if !ok {
		t.Fatalf("expected *ir.Projection, got %T", plan)
	}
	if len(proj.Items) != 1 || proj.Items[0].Name != "n" {
		t.Errorf("Items = %v, want [{n ...}]", proj.Items)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. WITH n.name AS name — renamed projection
//    → Projection([{Name:"name", Expression:"n.name"}], child)
// ─────────────────────────────────────────────────────────────────────────────

func Test_With_RenamedProjection(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		With: []*ast.With{
			{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
				{
					Expr:  &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"},
					Alias: strPtr("name"),
				},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	proj, ok := plan.(*ir.Projection)
	if !ok {
		t.Fatalf("expected *ir.Projection, got %T", plan)
	}
	if len(proj.Items) != 1 || proj.Items[0].Name != "name" {
		t.Errorf("Items = %v", proj.Items)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. WITH n WHERE n.age > 18 — Projection + Selection
//    → Selection("n.age > 18", Projection([n], child))
// ─────────────────────────────────────────────────────────────────────────────

func Test_With_WhereFilter(t *testing.T) {
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
			},
		},
		With: []*ast.With{
			{
				Projection: &ast.Projection{Items: []*ast.ProjectionItem{
					{Expr: &ast.Variable{Name: "n"}},
				}},
				Where: &ast.Where{Predicate: pred},
			},
		},
	}
	plan := mustFromAST(t, q)

	// openCypher 9 §5.1.5: WITH WHERE filters the pre-projection stream
	// (so the predicate can reference pre-WITH variables that the
	// projection drops). The plan shape is Projection(Selection(child)).
	proj, ok := plan.(*ir.Projection)
	if !ok {
		t.Fatalf("expected *ir.Projection (WHERE filters below it), got %T", plan)
	}
	sel, ok := proj.Child.(*ir.Selection)
	if !ok {
		t.Fatalf("proj.Child expected *ir.Selection (WHERE), got %T", proj.Child)
	}
	if sel.Predicate == "" {
		t.Error("WHERE Selection.Predicate must be non-empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. WITH n, count(*) AS c — aggregation detected
//    → Projection([n,c], EagerAggregation(groupBy=[n], aggs=[count(*)], child))
// ─────────────────────────────────────────────────────────────────────────────

func Test_With_Aggregation_CountStar(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		With: []*ast.With{
			{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
				{Expr: &ast.Variable{Name: "n"}},
				{
					Expr: &ast.FunctionInvocation{
						Name: "count",
						Args: []ast.Expression{&ast.StringLiteral{Value: "*"}},
					},
					Alias: strPtr("c"),
				},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	// Root: Projection wrapping EagerAggregation.
	proj, ok := plan.(*ir.Projection)
	if !ok {
		t.Fatalf("expected *ir.Projection, got %T", plan)
	}
	agg, ok := proj.Child.(*ir.EagerAggregation)
	if !ok {
		t.Fatalf("proj.Child expected *ir.EagerAggregation, got %T", proj.Child)
	}
	if !containsAll(agg.GroupBy, "n") {
		t.Errorf("GroupBy = %v, want to contain n", agg.GroupBy)
	}
	if len(agg.Aggregates) != 1 || agg.Aggregates[0].Function != "count" {
		t.Errorf("Aggregates = %v", agg.Aggregates)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. WITH n, sum(n.salary) AS total — aggregation with non-star arg
// ─────────────────────────────────────────────────────────────────────────────

func Test_With_Aggregation_Sum(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
					{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				}},
			},
		},
		With: []*ast.With{
			{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
				{Expr: &ast.Variable{Name: "n"}},
				{
					Expr: &ast.FunctionInvocation{
						Name: "sum",
						Args: []ast.Expression{&ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "salary"}},
					},
					Alias: strPtr("total"),
				},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	proj, ok := plan.(*ir.Projection)
	if !ok {
		t.Fatalf("expected *ir.Projection, got %T", plan)
	}
	agg, ok := proj.Child.(*ir.EagerAggregation)
	if !ok {
		t.Fatalf("proj.Child expected *ir.EagerAggregation, got %T", proj.Child)
	}
	if len(agg.Aggregates) != 1 {
		t.Fatalf("Aggregates count = %d, want 1", len(agg.Aggregates))
	}
	if agg.Aggregates[0].Function != "sum" {
		t.Errorf("Function = %q, want sum", agg.Aggregates[0].Function)
	}
	if agg.Aggregates[0].OutputName != "total" {
		t.Errorf("OutputName = %q, want total", agg.Aggregates[0].OutputName)
	}
	if agg.Aggregates[0].Argument == "" {
		t.Error("Argument should be non-empty for sum(n.salary)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. WITH n, m — scope boundary drops no variable (both are projected)
//    → Projection([n, m], child)
// ─────────────────────────────────────────────────────────────────────────────

func Test_With_TwoVariables(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("m")}}},
			}}},
		},
		With: []*ast.With{
			{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
				{Expr: &ast.Variable{Name: "n"}},
				{Expr: &ast.Variable{Name: "m"}},
			}}},
		},
	}
	plan := mustFromAST(t, q)

	proj, ok := plan.(*ir.Projection)
	if !ok {
		t.Fatalf("expected *ir.Projection, got %T", plan)
	}
	if len(proj.Items) != 2 {
		t.Errorf("Items count = %d, want 2", len(proj.Items))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. WITH n WHERE n.age > 18 — Vars pass-through via Selection → Projection
// ─────────────────────────────────────────────────────────────────────────────

func Test_With_WhereVarsPassthrough(t *testing.T) {
	pred := &ast.BinaryOp{
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"},
		Operator: ">",
		Right:    &ast.IntLiteral{Value: 18},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
			}}},
		},
		With: []*ast.With{{
			Projection: &ast.Projection{Items: []*ast.ProjectionItem{
				{Expr: &ast.Variable{Name: "n"}},
			}},
			Where: &ast.Where{Predicate: pred},
		}},
	}
	plan := mustFromAST(t, q)

	vars := plan.Vars()
	if !containsAll(vars, "n") {
		t.Errorf("Vars() = %v, want to contain n", vars)
	}
}
