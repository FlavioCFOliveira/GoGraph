package sema_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func ptr(s string) *string { return &s }

// pos builds a synthetic Position for test nodes.
func pos(line, col uint32) ast.Position {
	return ast.Position{Line: line, Column: col}
}

// varExpr returns a Variable expression node.
func varExpr(name string) *ast.Variable {
	return &ast.Variable{Name: name, Pos: pos(1, 1)}
}

// varExprAt returns a Variable expression with explicit position.
func varExprAt(name string, p ast.Position) *ast.Variable {
	return &ast.Variable{Name: name, Pos: p}
}

// singleNode builds a SingleQuery wrapping the given slices.
func singleNode(
	reading []ast.ReadingClause,
	withs []*ast.With,
	updating []ast.UpdatingClause,
	ret *ast.Return,
) *ast.SingleQuery {
	return &ast.SingleQuery{
		ReadingClauses:  reading,
		With:            withs,
		UpdatingClauses: updating,
		Return:          ret,
	}
}

// matchNode builds a MATCH clause introducing the named node variable.
func matchNode(varName string) *ast.Match {
	return &ast.Match{
		Pattern: &ast.Pattern{
			Paths: []*ast.PathPattern{
				{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr(varName), Pos: pos(1, 7)},
					},
				},
			},
		},
	}
}

// matchRel builds a MATCH (a)-[r]->(b) with given variable names.
func matchRel(nodeA, relVar, nodeB string) *ast.Match {
	return &ast.Match{
		Pattern: &ast.Pattern{
			Paths: []*ast.PathPattern{
				{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr(nodeA), Pos: pos(1, 7)},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{
								Variable:  ptr(relVar),
								Direction: ast.RelDirectionOutgoing,
								Pos:       pos(1, 10),
							},
							Node: &ast.NodePattern{Variable: ptr(nodeB), Pos: pos(1, 16)},
						},
					},
				},
			},
		},
	}
}

// returnVar builds a RETURN clause projecting a single variable.
func returnVar(varName string) *ast.Return {
	return &ast.Return{
		Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{
				{Expr: varExpr(varName)},
			},
		},
	}
}

// returnVarAt builds a RETURN clause with an explicit position for the variable.
func returnVarAt(varName string, p ast.Position) *ast.Return {
	return &ast.Return{
		Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{
				{Expr: varExprAt(varName, p)},
			},
		},
	}
}

// withProject builds a WITH clause projecting a single variable (no alias).
func withProject(varName string) *ast.With {
	return &ast.With{
		Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{
				{Expr: varExpr(varName)},
			},
		},
	}
}

// withAlias builds a WITH clause projecting expr AS alias.
func withAlias(expr ast.Expression, alias string) *ast.With {
	return &ast.With{
		Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{
				{Expr: expr, Alias: ptr(alias)},
			},
		},
	}
}

// assertErrors checks that Analyse returns exactly the expected number of
// errors, all of the expected kind (if kindWant != "").
func assertErrors(t *testing.T, q ast.Query, wantCount int, kindWant sema.ErrorKind) {
	t.Helper()
	errs := sema.Analyse(q)
	if len(errs) != wantCount {
		t.Fatalf("want %d error(s), got %d: %v", wantCount, len(errs), errs)
	}
	if kindWant != "" {
		for _, e := range errs {
			if e.Kind != kindWant {
				t.Errorf("want kind %q, got %q (%s)", kindWant, e.Kind, e.Message)
			}
		}
	}
}

func assertClean(t *testing.T, q ast.Query) {
	t.Helper()
	if errs := sema.Analyse(q); len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Positive cases (clean queries)
// ─────────────────────────────────────────────────────────────────────────────

func TestClean_MatchReturn(t *testing.T) {
	// MATCH (n) RETURN n
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		returnVar("n"),
	)
	assertClean(t, q)
}

func TestClean_MatchRelReturn(t *testing.T) {
	// MATCH (a)-[r]->(b) RETURN a, r, b
	q := singleNode(
		[]ast.ReadingClause{matchRel("a", "r", "b")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: varExpr("a")},
			{Expr: varExpr("r")},
			{Expr: varExpr("b")},
		}}},
	)
	assertClean(t, q)
}

func TestClean_UnwindReturn(t *testing.T) {
	// UNWIND [1,2,3] AS x RETURN x
	q := singleNode(
		[]ast.ReadingClause{
			&ast.Unwind{
				Expr:     &ast.ListLiteral{Elements: []ast.Expression{&ast.IntLiteral{Value: 1}}},
				Variable: "x",
				Pos:      pos(1, 1),
			},
		},
		nil, nil,
		returnVar("x"),
	)
	assertClean(t, q)
}

func TestClean_WithBoundaryForwards(t *testing.T) {
	// MATCH (n) WITH n RETURN n
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		[]*ast.With{withProject("n")},
		nil,
		returnVar("n"),
	)
	assertClean(t, q)
}

func TestClean_WithAlias(t *testing.T) {
	// MATCH (n) WITH n AS m RETURN m
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		[]*ast.With{withAlias(varExpr("n"), "m")},
		nil,
		returnVar("m"),
	)
	assertClean(t, q)
}

func TestClean_CreateNode(t *testing.T) {
	// CREATE (n) RETURN n
	q := singleNode(
		nil, nil,
		[]ast.UpdatingClause{
			&ast.Create{
				Pattern: &ast.Pattern{
					Paths: []*ast.PathPattern{{
						Head: &ast.PathElement{
							Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(1, 8)},
						},
					}},
				},
			},
		},
		returnVar("n"),
	)
	assertClean(t, q)
}

func TestClean_ReturnStar(t *testing.T) {
	// MATCH (n) RETURN *
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{All: true}},
	)
	assertClean(t, q)
}

func TestClean_CallYield(t *testing.T) {
	// CALL db.labels() YIELD label RETURN label
	q := singleNode(
		[]ast.ReadingClause{
			&ast.Call{
				Namespace: []string{"db"},
				Procedure: "labels",
				Args:      []ast.Expression{},
				Yield: []*ast.YieldItem{
					{Name: "label", Pos: pos(1, 20)},
				},
			},
		},
		nil, nil,
		returnVar("label"),
	)
	assertClean(t, q)
}

func TestClean_MultipleMatchClauses(t *testing.T) {
	// MATCH (a) MATCH (b) RETURN a, b — both vars in scope
	q := singleNode(
		[]ast.ReadingClause{matchNode("a"), matchNode("b")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: varExpr("a")},
			{Expr: varExpr("b")},
		}}},
	)
	assertClean(t, q)
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Negative scenarios — UNDEFINED_VAR
// ─────────────────────────────────────────────────────────────────────────────

func TestNeg_ReturnUndefinedVar(t *testing.T) {
	// RETURN x  — x never introduced
	q := singleNode(nil, nil, nil, returnVar("x"))
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_WhereRefsUndefined(t *testing.T) {
	// MATCH (n) WHERE y.name = 'Alice' RETURN n
	q := singleNode(
		[]ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
					Head: &ast.PathElement{Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(1, 7)}},
				}}},
				Where: &ast.Where{
					Predicate: &ast.BinaryOp{
						Left:     &ast.Property{Receiver: varExpr("y"), Key: "name"},
						Operator: "=",
						Right:    &ast.StringLiteral{Value: "Alice"},
					},
				},
			},
		},
		nil, nil,
		returnVar("n"),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_WithDropsVar(t *testing.T) {
	// MATCH (n) WITH 1 AS one RETURN n  — n dropped by WITH
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		[]*ast.With{withAlias(&ast.IntLiteral{Value: 1}, "one")},
		nil,
		returnVarAt("n", pos(3, 8)),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_WithSourceUndefined(t *testing.T) {
	// WITH x RETURN x  — x not in scope before WITH
	q := singleNode(
		nil,
		[]*ast.With{withProject("x")},
		nil,
		returnVar("x"),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_WithDropsSecondVar(t *testing.T) {
	// MATCH (a) MATCH (b) WITH a RETURN b  — b dropped by WITH
	q := singleNode(
		[]ast.ReadingClause{matchNode("a"), matchNode("b")},
		[]*ast.With{withProject("a")},
		nil,
		returnVarAt("b", pos(4, 8)),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_UnwindSourceUndefined(t *testing.T) {
	// UNWIND xs AS x RETURN x  — xs never defined
	q := singleNode(
		[]ast.ReadingClause{
			&ast.Unwind{
				Expr:     varExprAt("xs", pos(1, 8)),
				Variable: "x",
				Pos:      pos(1, 1),
			},
		},
		nil, nil,
		returnVar("x"),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_ReturnTwoUndefined(t *testing.T) {
	// RETURN a, b  — both undefined
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: varExprAt("a", pos(1, 8))},
			{Expr: varExprAt("b", pos(1, 11))},
		}}},
	)
	assertErrors(t, q, 2, sema.KindUndefinedVar)
}

func TestNeg_DeleteUndefined(t *testing.T) {
	// DELETE x  — x never introduced
	q := singleNode(
		nil, nil,
		[]ast.UpdatingClause{
			&ast.Delete{Expressions: []ast.Expression{varExprAt("x", pos(1, 8))}},
		},
		nil,
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_DetachDeleteUndefined(t *testing.T) {
	// DETACH DELETE y
	q := singleNode(
		nil, nil,
		[]ast.UpdatingClause{
			&ast.DetachDelete{Expressions: []ast.Expression{varExprAt("y", pos(1, 15))}},
		},
		nil,
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_SetTargetUndefined(t *testing.T) {
	// SET x.age = 30  — x not in scope
	q := singleNode(
		nil, nil,
		[]ast.UpdatingClause{
			&ast.Set{Items: []*ast.SetItem{
				{
					Target:   &ast.Property{Receiver: varExprAt("x", pos(1, 5)), Key: "age"},
					Value:    &ast.IntLiteral{Value: 30},
					Operator: "=",
				},
			}},
		},
		nil,
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_SetValueUndefined(t *testing.T) {
	// MATCH (n) SET n.age = missing.value  — missing not in scope
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil,
		[]ast.UpdatingClause{
			&ast.Set{Items: []*ast.SetItem{
				{
					Target:   &ast.Property{Receiver: varExpr("n"), Key: "age"},
					Value:    &ast.Property{Receiver: varExprAt("missing", pos(1, 22)), Key: "value"},
					Operator: "=",
				},
			}},
		},
		nil,
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_RemoveTargetUndefined(t *testing.T) {
	// REMOVE z.prop  — z not in scope
	q := singleNode(
		nil, nil,
		[]ast.UpdatingClause{
			&ast.Remove{Items: []*ast.RemoveItem{
				{Target: &ast.Property{Receiver: varExprAt("z", pos(1, 8)), Key: "prop"}},
			}},
		},
		nil,
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_OrderByUndefined(t *testing.T) {
	// MATCH (n) RETURN n ORDER BY x
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{
			Items:   []*ast.ProjectionItem{{Expr: varExpr("n")}},
			OrderBy: []*ast.SortItem{{Expr: varExprAt("x", pos(1, 28))}},
		}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_SkipUndefined(t *testing.T) {
	// MATCH (n) RETURN n SKIP missing
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: varExpr("n")}},
			Skip:  varExprAt("missing", pos(1, 20)),
		}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_LimitUndefined(t *testing.T) {
	// MATCH (n) RETURN n LIMIT ghost
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{{Expr: varExpr("n")}},
			Limit: varExprAt("ghost", pos(1, 21)),
		}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_BinaryOpUndefined(t *testing.T) {
	// MATCH (n) RETURN n.age + unknown
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.BinaryOp{
				Left:     &ast.Property{Receiver: varExpr("n"), Key: "age"},
				Operator: "+",
				Right:    varExprAt("unknown", pos(1, 24)),
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_UnaryOpUndefined(t *testing.T) {
	// RETURN NOT ghost
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.UnaryOp{Operator: "NOT", Operand: varExprAt("ghost", pos(1, 12))}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_FunctionArgUndefined(t *testing.T) {
	// RETURN size(missing)
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.FunctionInvocation{
				Name: "size",
				Args: []ast.Expression{varExprAt("missing", pos(1, 13))},
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_CaseSubjectUndefined(t *testing.T) {
	// RETURN CASE ghost WHEN 1 THEN 'a' END
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.CaseExpression{
				Subject: varExprAt("ghost", pos(1, 13)),
				Alternatives: []*ast.CaseAlternative{
					{Condition: &ast.IntLiteral{Value: 1}, Consequent: &ast.StringLiteral{Value: "a"}},
				},
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_CaseConditionUndefined(t *testing.T) {
	// RETURN CASE WHEN ghost = 1 THEN 'a' END
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.CaseExpression{
				Alternatives: []*ast.CaseAlternative{
					{
						Condition:  &ast.BinaryOp{Left: varExprAt("ghost", pos(1, 18)), Operator: "=", Right: &ast.IntLiteral{Value: 1}},
						Consequent: &ast.StringLiteral{Value: "a"},
					},
				},
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_ListLiteralUndefined(t *testing.T) {
	// RETURN [a, b]  — neither defined
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.ListLiteral{Elements: []ast.Expression{
				varExprAt("a", pos(1, 9)),
				varExprAt("b", pos(1, 12)),
			}}},
		}}},
	)
	assertErrors(t, q, 2, sema.KindUndefinedVar)
}

func TestNeg_SubscriptUndefined(t *testing.T) {
	// RETURN xs[0]  — xs not in scope
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.SubscriptExpr{
				Expr:  varExprAt("xs", pos(1, 8)),
				Index: &ast.IntLiteral{Value: 0},
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_SliceUndefined(t *testing.T) {
	// RETURN xs[0..ghost]
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.SliceExpr{
				Expr: varExprAt("xs", pos(1, 8)),
				From: &ast.IntLiteral{Value: 0},
				To:   varExprAt("ghost", pos(1, 13)),
			}},
		}}},
	)
	// xs AND ghost are both undefined
	assertErrors(t, q, 2, sema.KindUndefinedVar)
}

func TestNeg_ListComprehensionSourceUndefined(t *testing.T) {
	// RETURN [x IN missing | x]
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.ListComprehension{
				Variable:   "x",
				Source:     varExprAt("missing", pos(1, 12)),
				Projection: varExpr("x"),
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_ListComprehensionLoopVarLeaks(t *testing.T) {
	// MATCH (n) WITH [x IN n.tags | x] AS tags RETURN x
	// x is a comprehension-local variable — RETURN x must fail.
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		[]*ast.With{withAlias(
			&ast.ListComprehension{
				Variable:   "x",
				Source:     &ast.Property{Receiver: varExpr("n"), Key: "tags"},
				Projection: varExpr("x"),
			},
			"tags",
		)},
		nil,
		returnVarAt("x", pos(3, 8)),
	)
	// After WITH only "tags" survives; "x" was comprehension-local.
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_MergeOnCreateSetUndefined(t *testing.T) {
	// MERGE (n:Person) ON CREATE SET ghost.prop = 1
	q := singleNode(
		nil, nil,
		[]ast.UpdatingClause{
			&ast.Merge{
				Pattern: &ast.PathPattern{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(1, 7), Labels: []string{"Person"}},
					},
				},
				OnCreate: []*ast.SetItem{
					{
						Target:   &ast.Property{Receiver: varExprAt("ghost", pos(1, 35)), Key: "prop"},
						Value:    &ast.IntLiteral{Value: 1},
						Operator: "=",
					},
				},
			},
		},
		nil,
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_CallArgUndefined(t *testing.T) {
	// CALL db.index.seek(missing) YIELD id
	q := singleNode(
		[]ast.ReadingClause{
			&ast.Call{
				Namespace: []string{"db", "index"},
				Procedure: "seek",
				Args:      []ast.Expression{varExprAt("missing", pos(1, 19))},
				Yield:     []*ast.YieldItem{{Name: "id", Pos: pos(1, 34)}},
			},
		},
		nil, nil,
		returnVar("id"),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_WithWhereUndefined(t *testing.T) {
	// MATCH (n) WITH n WHERE ghost = 1 RETURN n
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		[]*ast.With{
			{
				Projection: &ast.Projection{Items: []*ast.ProjectionItem{{Expr: varExpr("n")}}},
				Where: &ast.Where{
					Predicate: &ast.BinaryOp{
						Left:     varExprAt("ghost", pos(1, 22)),
						Operator: "=",
						Right:    &ast.IntLiteral{Value: 1},
					},
				},
			},
		},
		nil,
		returnVar("n"),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_MapLiteralValueUndefined(t *testing.T) {
	// RETURN {key: ghost}
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.MapLiteral{
				Keys:   []string{"key"},
				Values: []ast.Expression{varExprAt("ghost", pos(1, 15))},
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_MapProjectionSubjectUndefined(t *testing.T) {
	// RETURN ghost {.name}
	q := singleNode(nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.MapProjection{
				Subject: varExprAt("ghost", pos(1, 8)),
				Items:   []*ast.MapProjectionItem{{Key: "name"}},
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_ExistsSubqueryUndefined(t *testing.T) {
	// MATCH (n) RETURN EXISTS { (n)-[r]->(missing) }
	// 'missing' is the node var — it IS introduced by the pattern, so no error there.
	// But if the WHERE references an undefined var that is fine to test separately.
	// Instead test that the subquery WHERE catches an undefined outer-scope ref.
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.ExistsSubquery{
				Query: &ast.SingleQuery{
					ReadingClauses: []ast.ReadingClause{
						&ast.Match{
							Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
								Head: &ast.PathElement{Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(2, 1)}},
							}}},
							Where: &ast.Where{
								Predicate: &ast.BinaryOp{
									Left:     varExprAt("ghost", pos(2, 20)),
									Operator: "=",
									Right:    &ast.IntLiteral{Value: 1},
								},
							},
						},
					},
					Return: &ast.Return{Projection: &ast.Projection{All: true}},
				},
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_CountSubqueryUndefined(t *testing.T) {
	// COUNT { (n)-[r]->(ghost) WHERE ghost2.x = 1 }
	q := singleNode(
		nil, nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.CountSubquery{
				Query: &ast.SingleQuery{
					ReadingClauses: []ast.ReadingClause{
						&ast.Match{
							Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
								Head: &ast.PathElement{
									Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(1, 2)},
									Next: &ast.PathElement{
										Relationship: &ast.RelationshipPattern{Variable: ptr("r"), Direction: ast.RelDirectionOutgoing, Pos: pos(1, 5)},
										Node:         &ast.NodePattern{Variable: ptr("m"), Pos: pos(1, 9)},
									},
								},
							}}},
							Where: &ast.Where{
								Predicate: &ast.BinaryOp{
									Left:     varExprAt("ghost2", pos(1, 20)),
									Operator: "=",
									Right:    &ast.IntLiteral{Value: 1},
								},
							},
						},
					},
				},
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Negative scenarios — REDECLARATION
// ─────────────────────────────────────────────────────────────────────────────

func TestNeg_RedeclareViaUnwind(t *testing.T) {
	// MATCH (x) UNWIND [1] AS x  — x already in scope
	q := singleNode(
		[]ast.ReadingClause{
			matchNode("x"),
			&ast.Unwind{
				Expr:     &ast.ListLiteral{Elements: []ast.Expression{&ast.IntLiteral{Value: 1}}},
				Variable: "x",
				Pos:      pos(2, 1),
			},
		},
		nil, nil, nil,
	)
	assertErrors(t, q, 1, sema.KindRedeclaration)
}

func TestNeg_RedeclareViaYield(t *testing.T) {
	// MATCH (n) CALL db.proc() YIELD n — n already in scope
	q := singleNode(
		[]ast.ReadingClause{
			matchNode("n"),
			&ast.Call{
				Namespace: []string{"db"},
				Procedure: "proc",
				Args:      []ast.Expression{},
				Yield:     []*ast.YieldItem{{Name: "n", Pos: pos(2, 25)}},
			},
		},
		nil, nil, nil,
	)
	assertErrors(t, q, 1, sema.KindRedeclaration)
}

func TestNeg_WithAliasShadowsExisting(t *testing.T) {
	// MATCH (n) MATCH (m) WITH n AS m — 'm' would be redeclared in new scope
	// After WITH, the new scope has only "m". But if there are two projections
	// that produce the same alias it is a redeclaration in the post-WITH scope.
	q := singleNode(
		[]ast.ReadingClause{matchNode("n"), matchNode("m")},
		[]*ast.With{
			{
				Projection: &ast.Projection{
					Items: []*ast.ProjectionItem{
						{Expr: varExpr("n"), Alias: ptr("dup")},
						{Expr: varExpr("m"), Alias: ptr("dup"), Pos: pos(1, 20)},
					},
				},
			},
		},
		nil, nil,
	)
	assertErrors(t, q, 1, sema.KindRedeclaration)
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. WITH boundary detailed scenarios
// ─────────────────────────────────────────────────────────────────────────────

func TestWithBoundary_OnlyAliasVisible(t *testing.T) {
	// MATCH (a) MATCH (b) WITH b AS renamed RETURN renamed  — ok
	q := singleNode(
		[]ast.ReadingClause{matchNode("a"), matchNode("b")},
		[]*ast.With{withAlias(varExpr("b"), "renamed")},
		nil,
		returnVar("renamed"),
	)
	assertClean(t, q)
}

func TestWithBoundary_OriginalDropped(t *testing.T) {
	// MATCH (a) MATCH (b) WITH b AS renamed RETURN a  — a dropped
	q := singleNode(
		[]ast.ReadingClause{matchNode("a"), matchNode("b")},
		[]*ast.With{withAlias(varExpr("b"), "renamed")},
		nil,
		returnVarAt("a", pos(4, 8)),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestWithBoundary_ChainedWith(t *testing.T) {
	// MATCH (n) WITH n AS m WITH m AS k RETURN k  — each WITH boundary ok
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		[]*ast.With{
			withAlias(varExpr("n"), "m"),
			withAlias(varExpr("m"), "k"),
		},
		nil,
		returnVar("k"),
	)
	assertClean(t, q)
}

func TestWithBoundary_ChainedWithBreaks(t *testing.T) {
	// MATCH (n) WITH n AS m WITH m AS k RETURN n  — n dropped at first WITH
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		[]*ast.With{
			withAlias(varExpr("n"), "m"),
			withAlias(varExpr("m"), "k"),
		},
		nil,
		returnVarAt("n", pos(4, 8)),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. UNWIND variable introduction
// ─────────────────────────────────────────────────────────────────────────────

func TestUnwind_IntroducesVar(t *testing.T) {
	// UNWIND range(1,10) AS i RETURN i  — i defined after UNWIND
	q := singleNode(
		[]ast.ReadingClause{
			&ast.Unwind{
				Expr:     &ast.FunctionInvocation{Name: "range", Args: []ast.Expression{&ast.IntLiteral{Value: 1}, &ast.IntLiteral{Value: 10}}},
				Variable: "i",
				Pos:      pos(1, 1),
			},
		},
		nil, nil,
		returnVar("i"),
	)
	assertClean(t, q)
}

func TestUnwind_VarNotVisibleBeforeUnwind(t *testing.T) {
	// RETURN i  — UNWIND comes after RETURN in clause order (degenerate AST)
	// i.e. the return clause references i but no UNWIND has been processed yet.
	// Since we build the AST manually with RETURN in the return field and no
	// reading clauses, i is simply undefined.
	q := singleNode(nil, nil, nil, returnVarAt("i", pos(1, 8)))
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. UNION — independent scope per branch
// ─────────────────────────────────────────────────────────────────────────────

func TestUnion_IndependentScopes(t *testing.T) {
	// MATCH (n) RETURN n  UNION  MATCH (m) RETURN m — each branch is clean
	q := &ast.MultiQuery{
		Parts: []*ast.SingleQuery{
			singleNode([]ast.ReadingClause{matchNode("n")}, nil, nil, returnVar("n")),
			singleNode([]ast.ReadingClause{matchNode("m")}, nil, nil, returnVar("m")),
		},
	}
	assertClean(t, q)
}

func TestUnion_ErrorInSecondBranch(t *testing.T) {
	// MATCH (n) RETURN n  UNION  RETURN ghost
	q := &ast.MultiQuery{
		Parts: []*ast.SingleQuery{
			singleNode([]ast.ReadingClause{matchNode("n")}, nil, nil, returnVar("n")),
			singleNode(nil, nil, nil, returnVarAt("ghost", pos(3, 8))),
		},
	}
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. EXISTS / COUNT subquery scope
// ─────────────────────────────────────────────────────────────────────────────

func TestExistsSubquery_OuterVarVisible(t *testing.T) {
	// MATCH (n) RETURN EXISTS { (n)-[r]->(m) }  — n visible inside
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.ExistsSubquery{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(2, 2)},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Variable: ptr("r"), Direction: ast.RelDirectionOutgoing, Pos: pos(2, 5)},
							Node:         &ast.NodePattern{Variable: ptr("m"), Pos: pos(2, 9)},
						},
					},
				}}},
			}},
		}}},
	)
	// n is already in scope — re-use is fine; no errors.
	assertClean(t, q)
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. ScopeError implements error interface
// ─────────────────────────────────────────────────────────────────────────────

func TestScopeError_ErrorString(t *testing.T) {
	e := &sema.ScopeError{
		Kind:    sema.KindUndefinedVar,
		Pos:     ast.Position{Line: 3, Column: 7},
		Message: `undefined variable "x"`,
	}
	got := e.Error()
	if got == "" {
		t.Fatal("Error() must return non-empty string")
	}
	// Must mention position and kind.
	for _, want := range []string{"3:7", "UNDEFINED_VAR"} {
		if got != "" {
			found := false
			for i := 0; i+len(want) <= len(got); i++ {
				if got[i:i+len(want)] == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Error() = %q, missing %q", got, want)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. Additional coverage scenarios
// ─────────────────────────────────────────────────────────────────────────────

func TestClean_OptionalMatch(t *testing.T) {
	// OPTIONAL MATCH (n) RETURN n
	q := singleNode(
		[]ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
					Head: &ast.PathElement{Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(1, 16)}},
				}}},
			},
		},
		nil, nil,
		returnVar("n"),
	)
	assertClean(t, q)
}

func TestNeg_OptionalMatchWhereUndefined(t *testing.T) {
	// OPTIONAL MATCH (n) WHERE ghost.x = 1 RETURN n
	q := singleNode(
		[]ast.ReadingClause{
			&ast.OptionalMatch{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
					Head: &ast.PathElement{Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(1, 16)}},
				}}},
				Where: &ast.Where{
					Predicate: &ast.BinaryOp{
						Left:     varExprAt("ghost", pos(1, 25)),
						Operator: "=",
						Right:    &ast.IntLiteral{Value: 1},
					},
				},
			},
		},
		nil, nil,
		returnVar("n"),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestClean_MergeOnMatchSet(t *testing.T) {
	// MERGE (n:Person) ON MATCH SET n.count = 1
	q := singleNode(
		nil, nil,
		[]ast.UpdatingClause{
			&ast.Merge{
				Pattern: &ast.PathPattern{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(1, 7), Labels: []string{"Person"}},
					},
				},
				OnMatch: []*ast.SetItem{
					{
						Target:   &ast.Property{Receiver: varExpr("n"), Key: "count"},
						Value:    &ast.IntLiteral{Value: 1},
						Operator: "=",
					},
				},
			},
		},
		nil,
	)
	assertClean(t, q)
}

func TestNeg_MergeOnMatchSetUndefined(t *testing.T) {
	// MERGE (n:Person) ON MATCH SET ghost.count = 1
	q := singleNode(
		nil, nil,
		[]ast.UpdatingClause{
			&ast.Merge{
				Pattern: &ast.PathPattern{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(1, 7), Labels: []string{"Person"}},
					},
				},
				OnMatch: []*ast.SetItem{
					{
						Target:   &ast.Property{Receiver: varExprAt("ghost", pos(1, 33)), Key: "count"},
						Value:    &ast.IntLiteral{Value: 1},
						Operator: "=",
					},
				},
			},
		},
		nil,
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestClean_RelationshipVarInReturn(t *testing.T) {
	// MATCH (a)-[r]->(b) RETURN r — r is a relationship var introduced by MATCH
	q := singleNode(
		[]ast.ReadingClause{matchRel("a", "r", "b")},
		nil, nil,
		returnVar("r"),
	)
	assertClean(t, q)
}

func TestNeg_RelVarAfterWith(t *testing.T) {
	// MATCH (a)-[r]->(b) WITH a RETURN r  — r dropped by WITH
	q := singleNode(
		[]ast.ReadingClause{matchRel("a", "r", "b")},
		[]*ast.With{withProject("a")},
		nil,
		returnVarAt("r", pos(3, 8)),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestClean_PatternComprehension(t *testing.T) {
	// MATCH (n) RETURN [(n)-[r]->(m) | m.name]
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.PatternComprehension{
				Pattern: &ast.PathPattern{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(2, 2)},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Variable: ptr("r"), Direction: ast.RelDirectionOutgoing, Pos: pos(2, 5)},
							Node:         &ast.NodePattern{Variable: ptr("m"), Pos: pos(2, 9)},
						},
					},
				},
				Projection: &ast.Property{Receiver: varExpr("m"), Key: "name"},
			}},
		}}},
	)
	assertClean(t, q)
}

func TestNeg_PatternComprehensionProjectionUndefined(t *testing.T) {
	// MATCH (n) RETURN [(n)-[r]->(m) | ghost.name]
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.PatternComprehension{
				Pattern: &ast.PathPattern{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(2, 2)},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Variable: ptr("r"), Direction: ast.RelDirectionOutgoing, Pos: pos(2, 5)},
							Node:         &ast.NodePattern{Variable: ptr("m"), Pos: pos(2, 9)},
						},
					},
				},
				Projection: &ast.Property{Receiver: varExprAt("ghost", pos(2, 15)), Key: "name"},
			}},
		}}},
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestClean_WithProjectionLiteral(t *testing.T) {
	// WITH 42 AS forty  — expression is a literal (non-Variable, no alias needed)
	// Actually test that a non-variable expression in WITH without alias produces
	// no name and therefore cannot be referenced: RETURN forty should be clean.
	q := singleNode(
		nil,
		[]*ast.With{withAlias(&ast.IntLiteral{Value: 42}, "forty")},
		nil,
		returnVar("forty"),
	)
	assertClean(t, q)
}

func TestNeg_WithProjectionNoAlias_Literal(t *testing.T) {
	// `WITH 1 RETURN *` violates two openCypher rules simultaneously:
	//   - WITH item is neither a bare Variable nor aliased
	//     (NoExpressionAlias, §5.1.2);
	//   - RETURN * with empty post-WITH scope (NoVariablesInScope,
	//     §3.3.2).
	// Both are now reported at compile time. Previously the analyser
	// silently accepted this shape (TestClean_WithProjectionNoAlias_Literal).
	q := singleNode(
		nil,
		[]*ast.With{
			{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
				{Expr: &ast.IntLiteral{Value: 1}},
			}}},
		},
		nil,
		&ast.Return{Projection: &ast.Projection{All: true}},
	)
	// Both kinds are expected; assertErrors only checks the first kind
	// pattern uniformly, so verify count + presence of each kind
	// manually here.
	errs := sema.Analyse(q)
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %d: %v", len(errs), errs)
	}
	seen := map[sema.ErrorKind]bool{}
	for _, e := range errs {
		seen[e.Kind] = true
	}
	if !seen[sema.KindNoExpressionAlias] {
		t.Errorf("expected NoExpressionAlias in errors, got %v", errs)
	}
	if !seen[sema.KindNoVariablesInScope] {
		t.Errorf("expected NoVariablesInScope in errors, got %v", errs)
	}
}

func TestClean_ReadingClauseWithNode(t *testing.T) {
	// Exercises the *ast.With branch in readingClause by placing a With node
	// directly inside ReadingClauses (as the ANTLR visitor may produce it).
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			matchNode("n"),
			withProject("n"), // With as a ReadingClause
		},
		Return: returnVar("n"),
	}
	assertClean(t, q)
}

func TestClean_ReadingClauseReturnNode(t *testing.T) {
	// Exercises *ast.Return inside ReadingClauses.
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			matchNode("n"),
			returnVar("n"), // Return as a ReadingClause
		},
	}
	assertClean(t, q)
}

func TestClean_ReadingClauseWhereNode(t *testing.T) {
	// Exercises *ast.Where inside ReadingClauses.
	q := &ast.SingleQuery{
		ReadingClauses: []ast.ReadingClause{
			matchNode("n"),
			&ast.Where{Predicate: varExpr("n")},
		},
	}
	assertClean(t, q)
}

func TestClean_CallYieldAlias(t *testing.T) {
	// CALL db.proc() YIELD name AS n RETURN n
	q := singleNode(
		[]ast.ReadingClause{
			&ast.Call{
				Namespace: []string{"db"},
				Procedure: "proc",
				Args:      []ast.Expression{},
				Yield: []*ast.YieldItem{
					{Name: "name", Alias: ptr("n"), Pos: pos(1, 20)},
				},
			},
		},
		nil, nil,
		returnVar("n"),
	)
	assertClean(t, q)
}

func TestClean_CallWithWhereOnYield(t *testing.T) {
	// CALL db.proc() YIELD n WHERE n.val > 0 RETURN n
	q := singleNode(
		[]ast.ReadingClause{
			&ast.Call{
				Namespace: []string{"db"},
				Procedure: "proc",
				Args:      []ast.Expression{},
				Yield: []*ast.YieldItem{
					{Name: "n", Pos: pos(1, 20)},
				},
				Where: &ast.Where{
					Predicate: &ast.BinaryOp{
						Left:     &ast.Property{Receiver: varExpr("n"), Key: "val"},
						Operator: ">",
						Right:    &ast.IntLiteral{Value: 0},
					},
				},
			},
		},
		nil, nil,
		returnVar("n"),
	)
	assertClean(t, q)
}

func TestClean_PathPatternVar(t *testing.T) {
	// MATCH p = (a)-[r]->(b) RETURN p
	q := singleNode(
		[]ast.ReadingClause{
			&ast.Match{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
					Variable: ptr("p"),
					Pos:      pos(1, 7),
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("a"), Pos: pos(1, 11)},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Variable: ptr("r"), Direction: ast.RelDirectionOutgoing, Pos: pos(1, 14)},
							Node:         &ast.NodePattern{Variable: ptr("b"), Pos: pos(1, 18)},
						},
					},
				}}},
			},
		},
		nil, nil,
		returnVar("p"),
	)
	assertClean(t, q)
}

func TestClean_RelReuseInSecondMatch(t *testing.T) {
	// MATCH (a)-[r]->(b) MATCH (b)-[s]->(c) RETURN a, r, b, s, c
	// b is reused across two MATCH patterns — should not be a redeclaration.
	m1 := matchRel("a", "r", "b")
	m2 := &ast.Match{
		Pattern: &ast.Pattern{
			Paths: []*ast.PathPattern{{
				Head: &ast.PathElement{
					Node: &ast.NodePattern{Variable: ptr("b"), Pos: pos(2, 7)},
					Next: &ast.PathElement{
						Relationship: &ast.RelationshipPattern{Variable: ptr("s"), Direction: ast.RelDirectionOutgoing, Pos: pos(2, 10)},
						Node:         &ast.NodePattern{Variable: ptr("c"), Pos: pos(2, 14)},
					},
				},
			}},
		},
	}
	q := singleNode(
		[]ast.ReadingClause{m1, m2},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: varExpr("a")},
			{Expr: varExpr("r")},
			{Expr: varExpr("b")},
			{Expr: varExpr("s")},
			{Expr: varExpr("c")},
		}}},
	)
	assertClean(t, q)
}

func TestClean_CountSubquery_PatternForm(t *testing.T) {
	// MATCH (n) RETURN COUNT { (n)-[r]->(m) }
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.CountSubquery{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(2, 2)},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Variable: ptr("r"), Direction: ast.RelDirectionOutgoing, Pos: pos(2, 5)},
							Node:         &ast.NodePattern{Variable: ptr("m"), Pos: pos(2, 9)},
						},
					},
				}}},
			}},
		}}},
	)
	assertClean(t, q)
}

func TestClean_ExistsSubquery_PatternForm(t *testing.T) {
	// MATCH (n) RETURN EXISTS { (n)-[r]->(m) }  — pattern form (no Query)
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{
			{Expr: &ast.ExistsSubquery{
				Pattern: &ast.Pattern{Paths: []*ast.PathPattern{{
					Head: &ast.PathElement{
						Node: &ast.NodePattern{Variable: ptr("n"), Pos: pos(2, 2)},
						Next: &ast.PathElement{
							Relationship: &ast.RelationshipPattern{Variable: ptr("r"), Direction: ast.RelDirectionOutgoing, Pos: pos(2, 5)},
							Node:         &ast.NodePattern{Variable: ptr("m"), Pos: pos(2, 9)},
						},
					},
				}}},
			}},
		}}},
	)
	assertClean(t, q)
}

// ScopeLeakError is currently not emitted by the analyser but the exported
// constructor must compile and produce a well-formed error.
func TestScopeLeakError_Constructs(t *testing.T) {
	e := sema.ScopeLeakError("x", ast.Position{Line: 5, Column: 3})
	if e == nil {
		t.Fatal("ScopeLeakError must return non-nil")
	}
	if e.Kind != sema.KindScopeLeak {
		t.Errorf("want SCOPE_LEAK, got %q", e.Kind)
	}
	if e.Error() == "" {
		t.Fatal("ScopeError.Error() must return non-empty string for SCOPE_LEAK")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 10. Scope unit tests
// ─────────────────────────────────────────────────────────────────────────────

func TestScope_DefineAndLookup(t *testing.T) {
	s := sema.NewScope()
	if err := s.Define("x", pos(1, 1), "node"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sym, ok := s.Lookup("x")
	if !ok {
		t.Fatal("expected Lookup to find 'x'")
	}
	if sym.Name != "x" {
		t.Errorf("want name 'x', got %q", sym.Name)
	}
}

func TestScope_Redeclaration(t *testing.T) {
	s := sema.NewScope()
	_ = s.Define("x", pos(1, 1), "node")
	err := s.Define("x", pos(2, 1), "node")
	if err == nil {
		t.Fatal("expected redeclaration error")
	}
	if err.Kind != sema.KindRedeclaration {
		t.Errorf("want REDECLARATION, got %q", err.Kind)
	}
}

func TestScope_ChildInherits(t *testing.T) {
	parent := sema.NewScope()
	_ = parent.Define("n", pos(1, 1), "node")
	child := parent.Child()
	if sym, ok := child.Lookup("n"); !ok || sym.Name != "n" {
		t.Fatal("child did not inherit parent symbol")
	}
}

func TestScope_ChildDoesNotPollute(t *testing.T) {
	parent := sema.NewScope()
	child := parent.Child()
	_ = child.Define("inner", pos(1, 1), "node")
	if _, ok := parent.Lookup("inner"); ok {
		t.Fatal("child definition leaked to parent")
	}
}

func TestScope_LookupLocal(t *testing.T) {
	parent := sema.NewScope()
	_ = parent.Define("n", pos(1, 1), "node")
	child := parent.Child()
	if _, ok := child.LookupLocal("n"); ok {
		t.Fatal("LookupLocal should not walk parent chain")
	}
	_ = child.Define("n", pos(2, 1), "node")
	if _, ok := child.LookupLocal("n"); !ok {
		t.Fatal("LookupLocal should find locally defined symbol")
	}
}

func TestScope_Names(t *testing.T) {
	s := sema.NewScope()
	_ = s.Define("a", pos(1, 1), "node")
	_ = s.Define("b", pos(1, 5), "node")
	names := s.Names()
	if len(names) != 2 {
		t.Errorf("want 2 names, got %d: %v", len(names), names)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 11. WITH ORDER BY scope-checking
// ─────────────────────────────────────────────────────────────────────────────

// withSortBy builds a WITH clause projecting a variable with an ORDER BY sort
// item on the given expression.
func withSortBy(projected string, sortExpr ast.Expression) *ast.With {
	return &ast.With{
		Projection: &ast.Projection{
			Items: []*ast.ProjectionItem{
				{Expr: varExpr(projected)},
			},
			OrderBy: []*ast.SortItem{
				{Expr: sortExpr},
			},
		},
	}
}

func TestClean_WithOrderBy_ProjectedVar(t *testing.T) {
	// MATCH (a) WITH a ORDER BY a RETURN a  — a is projected, ORDER BY a is clean
	q := singleNode(
		[]ast.ReadingClause{matchNode("a")},
		[]*ast.With{withSortBy("a", varExpr("a"))},
		nil,
		returnVar("a"),
	)
	assertClean(t, q)
}

func TestClean_WithOrderBy_PreWithVar(t *testing.T) {
	// MATCH (a) WITH a AS m WITH m ORDER BY a RETURN m
	// After first WITH, scope is {m}. Second WITH projects m; ORDER BY a checks
	// pre-second-WITH scope which only has {m} — a is gone.
	// But this test checks a DIFFERENT case: one WITH where ORDER BY references
	// the pre-WITH source variable.
	//
	// MATCH (a) MATCH (b) WITH a ORDER BY b RETURN a
	// Pre-WITH scope: {a, b}. b IS in pre-WITH scope → clean.
	q := singleNode(
		[]ast.ReadingClause{matchNode("a"), matchNode("b")},
		[]*ast.With{withSortBy("a", varExprAt("b", pos(1, 20)))},
		nil,
		returnVar("a"),
	)
	assertClean(t, q)
}

func TestNeg_WithOrderBy_OutOfScope(t *testing.T) {
	// MATCH (a) WITH a, b WITH a ORDER BY c
	// After first WITH, scope is {a, b}. Second WITH projects only a;
	// ORDER BY c — c is not in pre-second-WITH scope {a, b}.
	q := singleNode(
		[]ast.ReadingClause{matchNode("a"), matchNode("b")},
		[]*ast.With{
			// First WITH: project both a and b
			{
				Projection: &ast.Projection{
					Items: []*ast.ProjectionItem{
						{Expr: varExpr("a")},
						{Expr: varExpr("b")},
					},
				},
			},
			// Second WITH: project only a, ORDER BY c (undefined)
			withSortBy("a", varExprAt("c", pos(3, 10))),
		},
		nil,
		returnVar("a"),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_WithOrderBy_NeverDefined(t *testing.T) {
	// WITH 1 AS a ORDER BY ghost — ghost never defined anywhere
	q := singleNode(
		nil,
		[]*ast.With{
			{
				Projection: &ast.Projection{
					Items: []*ast.ProjectionItem{
						{Expr: &ast.IntLiteral{Value: 1}, Alias: ptr("a")},
					},
					OrderBy: []*ast.SortItem{
						{Expr: varExprAt("ghost", pos(1, 22))},
					},
				},
			},
		},
		nil,
		returnVar("a"),
	)
	assertErrors(t, q, 1, sema.KindUndefinedVar)
}

func TestNeg_WithOrderBy_MultipleUndefined(t *testing.T) {
	// WITH 1 AS a, 'b' AS b WITH a ORDER BY c, d
	// After first WITH: {a, b}. Second WITH projects a; ORDER BY c, d — both undefined.
	q := singleNode(
		nil,
		[]*ast.With{
			{
				Projection: &ast.Projection{
					Items: []*ast.ProjectionItem{
						{Expr: &ast.IntLiteral{Value: 1}, Alias: ptr("a")},
						{Expr: &ast.StringLiteral{Value: "b"}, Alias: ptr("b")},
					},
				},
			},
			{
				Projection: &ast.Projection{
					Items: []*ast.ProjectionItem{
						{Expr: varExpr("a")},
					},
					OrderBy: []*ast.SortItem{
						{Expr: varExprAt("c", pos(3, 10))},
						{Expr: varExprAt("d", pos(3, 13))},
					},
				},
			},
		},
		nil,
		returnVar("a"),
	)
	assertErrors(t, q, 2, sema.KindUndefinedVar)
}

func TestClean_WithOrderBy_AliasInOrderBy(t *testing.T) {
	// WITH n.name AS name ORDER BY name — alias is introduced into pre-WITH scope
	// before ORDER BY is checked, so name IS in scope.
	q := singleNode(
		[]ast.ReadingClause{matchNode("n")},
		[]*ast.With{
			{
				Projection: &ast.Projection{
					Items: []*ast.ProjectionItem{
						{
							Expr:  &ast.Property{Receiver: varExpr("n"), Key: "name"},
							Alias: ptr("name"),
							Pos:   pos(1, 8),
						},
					},
					OrderBy: []*ast.SortItem{
						{Expr: varExpr("name")},
					},
				},
			},
		},
		nil,
		returnVar("name"),
	)
	assertClean(t, q)
}
