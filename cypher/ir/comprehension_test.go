package ir_test

// comprehension_test.go — tests for PatternComprehension → RollUpApply
// translation (task-225).

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
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

// ─────────────────────────────────────────────────────────────────────────────
// 5. Pattern comprehension in WITH projection — translateWith must hoist the
//    comprehension into a RollUpApply on the input side just like translateReturn.
//    Regression for Pattern2 [8]:
//        MATCH (n)-->(b) WITH [p = (n)-->() | p] AS ps, count(b) AS c RETURN ps, c
// ─────────────────────────────────────────────────────────────────────────────

func Test_Comprehension_InWith_HoistedAsRollUpApply(t *testing.T) {
	pc := &ast.PatternComprehension{
		Variable: strPtr("p"),
		Pattern: &ast.PathPattern{
			Head: &ast.PathElement{
				Node: &ast.NodePattern{Variable: strPtr("n")},
				Next: &ast.PathElement{
					Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionOutgoing},
					Node:         &ast.NodePattern{},
				},
			},
		},
		Projection: &ast.Variable{Name: "p"},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
				Head: &ast.PathElement{
					Node: &ast.NodePattern{Variable: strPtr("n")},
					Next: &ast.PathElement{
						Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionOutgoing},
						Node:         &ast.NodePattern{Variable: strPtr("b")},
					},
				},
			}}}},
			&ast.With{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
				{Expr: pc, Alias: strPtr("ps")},
				{Expr: &ast.FunctionInvocation{
					Name: "count",
					Args: []ast.Expression{&ast.Variable{Name: "b"}},
				}, Alias: strPtr("c")},
			}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.Variable{Name: "ps"}},
			{Expr: &ast.Variable{Name: "c"}},
		}}},
	}
	plan := mustFromAST(t, q)

	// The plan must contain a RollUpApply that sits BELOW the EagerAggregation
	// produced by the WITH, so the aggregate sees the synthetic ps column.
	if !findRollUpApply(plan) {
		t.Fatalf("WITH pattern comprehension should hoist into a RollUpApply, but none found in plan tree")
	}
	if !findEagerAggregation(plan) {
		t.Fatalf("WITH with count(b) should produce an EagerAggregation, but none found in plan tree")
	}
}

// findRollUpApply reports whether any node in the plan tree is a *ir.RollUpApply.
func findRollUpApply(p ir.LogicalPlan) bool {
	if p == nil {
		return false
	}
	if _, ok := p.(*ir.RollUpApply); ok {
		return true
	}
	for _, ch := range p.Children() {
		if findRollUpApply(ch) {
			return true
		}
	}
	return false
}

// findEagerAggregation reports whether any node is an EagerAggregation.
func findEagerAggregation(p ir.LogicalPlan) bool {
	if p == nil {
		return false
	}
	if _, ok := p.(*ir.EagerAggregation); ok {
		return true
	}
	for _, ch := range p.Children() {
		if findEagerAggregation(ch) {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Pattern comprehension nested inside aggregate function argument —
//    `count([p = (n)-->() | p])` must be hoisted into a RollUpApply so the
//    aggregate sees the synthetic __pc_N column rather than the raw AST node.
//    Regression for Pattern2 [6].
// ─────────────────────────────────────────────────────────────────────────────

func Test_Comprehension_InAggregateArgument_Hoisted(t *testing.T) {
	pc := &ast.PatternComprehension{
		Variable: strPtr("p"),
		Pattern: &ast.PathPattern{
			Head: &ast.PathElement{
				Node: &ast.NodePattern{Variable: strPtr("n")},
				Next: &ast.PathElement{
					Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionOutgoing, Types: []string{"HAS"}},
					Node:         &ast.NodePattern{},
				},
			},
		},
		Projection: &ast.Variable{Name: "p"},
	}
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			&ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
				Head: &ast.PathElement{Node: &ast.NodePattern{
					Variable: strPtr("n"),
					Labels:   []string{"A"},
				}},
			}}}},
		},
		Return: &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.FunctionInvocation{
				Name: "count",
				Args: []ast.Expression{pc},
			}, Alias: strPtr("c")},
		}}},
	}
	plan := mustFromAST(t, q)

	if !findRollUpApply(plan) {
		t.Fatalf("count(pattern-comprehension) should hoist the comprehension into a RollUpApply")
	}
	if !findEagerAggregation(plan) {
		t.Fatalf("count(pattern-comprehension) should produce an EagerAggregation")
	}
}
