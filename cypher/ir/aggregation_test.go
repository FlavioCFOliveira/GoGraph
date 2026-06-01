package ir_test

// aggregation_test.go — tests for aggregate-function detection (task-223).
//
// detectAggregation is an internal function exercised indirectly through the
// WITH and RETURN translation paths. Direct unit tests call FromAST with
// queries containing aggregate projections and verify the EagerAggregation
// operator is emitted with the correct grouping keys and aggregate descriptors.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// 1. RETURN count(*) AS c — top-level aggregation, no grouping key
// ─────────────────────────────────────────────────────────────────────────────

func Test_Aggregation_CountStar_NoGroupBy(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{
				Expr: &ast.FunctionInvocation{
					Name: "count",
					Args: []ast.Expression{&ast.StringLiteral{Value: "*"}},
				},
				Alias: strPtr("c"),
			},
		}}},
	}
	plan := mustFromAST(t, q)

	// ProduceResults → Projection → EagerAggregation → AllNodesScan
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		t.Fatalf("expected *ir.ProduceResults, got %T", plan)
	}
	proj, ok := pr.Child.(*ir.Projection)
	if !ok {
		t.Fatalf("pr.Child expected *ir.Projection, got %T", pr.Child)
	}
	agg, ok := proj.Child.(*ir.EagerAggregation)
	if !ok {
		t.Fatalf("proj.Child expected *ir.EagerAggregation, got %T", proj.Child)
	}
	if len(agg.GroupBy) != 0 {
		t.Errorf("GroupBy = %v, want empty (no non-aggregate items)", agg.GroupBy)
	}
	if len(agg.Aggregates) != 1 || agg.Aggregates[0].Function != "count" {
		t.Errorf("Aggregates = %v", agg.Aggregates)
	}
	if agg.Aggregates[0].OutputName != "c" {
		t.Errorf("OutputName = %q, want c", agg.Aggregates[0].OutputName)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. RETURN n.city, count(n) AS cnt — grouping key + count
// ─────────────────────────────────────────────────────────────────────────────

func Test_Aggregation_GroupByPlusCount(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n"), Labels: []string{"Person"}}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{
				Expr:  &ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "city"},
				Alias: strPtr("city"),
			},
			{
				Expr: &ast.FunctionInvocation{
					Name: "count",
					Args: []ast.Expression{&ast.Variable{Name: "n"}},
				},
				Alias: strPtr("cnt"),
			},
		}}},
	}
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	proj := pr.Child.(*ir.Projection)
	agg, ok := proj.Child.(*ir.EagerAggregation)
	if !ok {
		t.Fatalf("expected *ir.EagerAggregation, got %T", proj.Child)
	}
	if !containsAll(agg.GroupBy, "city") {
		t.Errorf("GroupBy = %v, want [city]", agg.GroupBy)
	}
	if len(agg.Aggregates) != 1 || agg.Aggregates[0].Function != "count" {
		t.Errorf("Aggregates = %v", agg.Aggregates)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. RETURN n, avg(n.age) AS avg_age — avg function
// ─────────────────────────────────────────────────────────────────────────────

func Test_Aggregation_Avg(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.Variable{Name: "n"}},
			{
				Expr: &ast.FunctionInvocation{
					Name: "avg",
					Args: []ast.Expression{&ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "age"}},
				},
				Alias: strPtr("avg_age"),
			},
		}}},
	}
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	proj := pr.Child.(*ir.Projection)
	agg, ok := proj.Child.(*ir.EagerAggregation)
	if !ok {
		t.Fatalf("expected *ir.EagerAggregation, got %T", proj.Child)
	}
	if len(agg.Aggregates) != 1 || agg.Aggregates[0].Function != "avg" {
		t.Errorf("Aggregates = %v", agg.Aggregates)
	}
	if agg.Aggregates[0].OutputName != "avg_age" {
		t.Errorf("OutputName = %q, want avg_age", agg.Aggregates[0].OutputName)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. RETURN collect(n.name) AS names — collect function
// ─────────────────────────────────────────────────────────────────────────────

func Test_Aggregation_Collect(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{
				Expr: &ast.FunctionInvocation{
					Name: "collect",
					Args: []ast.Expression{&ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "name"}},
				},
				Alias: strPtr("names"),
			},
		}}},
	}
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	proj := pr.Child.(*ir.Projection)
	agg, ok := proj.Child.(*ir.EagerAggregation)
	if !ok {
		t.Fatalf("expected *ir.EagerAggregation, got %T", proj.Child)
	}
	if len(agg.Aggregates) != 1 || agg.Aggregates[0].Function != "collect" {
		t.Errorf("Aggregates = %v", agg.Aggregates)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. RETURN n — no aggregation, plain Projection
// ─────────────────────────────────────────────────────────────────────────────

func Test_Aggregation_NoAgg_PlainProjection(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.Variable{Name: "n"}},
		}}},
	}
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	proj, ok := pr.Child.(*ir.Projection)
	if !ok {
		t.Fatalf("pr.Child expected *ir.Projection (no agg), got %T", pr.Child)
	}
	// Must NOT have an EagerAggregation underneath.
	if _, ok := proj.Child.(*ir.EagerAggregation); ok {
		t.Fatal("unexpected *ir.EagerAggregation for non-aggregate query")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. RETURN n, min(n.salary) AS min_sal, max(n.salary) AS max_sal
//    Two aggregates with one grouping key.
// ─────────────────────────────────────────────────────────────────────────────

func Test_Aggregation_MinMax(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.Variable{Name: "n"}},
			{
				Expr: &ast.FunctionInvocation{
					Name: "min",
					Args: []ast.Expression{&ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "salary"}},
				},
				Alias: strPtr("min_sal"),
			},
			{
				Expr: &ast.FunctionInvocation{
					Name: "max",
					Args: []ast.Expression{&ast.Property{Receiver: &ast.Variable{Name: "n"}, Key: "salary"}},
				},
				Alias: strPtr("max_sal"),
			},
		}}},
	}
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	proj := pr.Child.(*ir.Projection)
	agg, ok := proj.Child.(*ir.EagerAggregation)
	if !ok {
		t.Fatalf("expected *ir.EagerAggregation, got %T", proj.Child)
	}
	if len(agg.Aggregates) != 2 {
		t.Errorf("Aggregates count = %d, want 2", len(agg.Aggregates))
	}
	aggFuncs := make(map[string]bool)
	for _, a := range agg.Aggregates {
		aggFuncs[a.Function] = true
	}
	if !aggFuncs["min"] || !aggFuncs["max"] {
		t.Errorf("expected min and max in aggregates, got %v", agg.Aggregates)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. RETURN count(DISTINCT n) AS cnt — DISTINCT flag preserved
// ─────────────────────────────────────────────────────────────────────────────

func Test_Aggregation_CountDistinct(t *testing.T) {
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{
				{Head: &ast.PathElement{Node: &ast.NodePattern{Variable: strPtr("n")}}},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{
				Expr: &ast.FunctionInvocation{
					Name:     "count",
					Distinct: true,
					Args:     []ast.Expression{&ast.Variable{Name: "n"}},
				},
				Alias: strPtr("cnt"),
			},
		}}},
	}
	plan := mustFromAST(t, q)

	pr := plan.(*ir.ProduceResults)
	proj := pr.Child.(*ir.Projection)
	agg, ok := proj.Child.(*ir.EagerAggregation)
	if !ok {
		t.Fatalf("expected *ir.EagerAggregation, got %T", proj.Child)
	}
	if !agg.Aggregates[0].Distinct {
		t.Error("Distinct flag not set for count(DISTINCT n)")
	}
}
