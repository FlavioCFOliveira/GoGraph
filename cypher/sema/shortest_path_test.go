package sema_test

// shortest_path_test.go — rmp #1691 semantic-analysis tests for
// shortestPath()/allShortestPaths() bindings. The AST is built directly with
// ast.PathPattern.Shortest stamped, mirroring what the parser's pre-lex
// normaliser produces (rmp #1690), so the tests exercise sema in isolation.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
)

func int64ptr(v int64) *int64 { return &v }

// shortestMatch builds a `MATCH p = <kind>((a)-[rel]-(b))` clause. When pathVar
// is empty the binding is unnamed. rng (optional) is the relationship's range
// quantifier; relVar (optional) names the relationship. extraNode, when true,
// appends a third node so the inner pattern is a multi-segment chain.
func shortestMatch(kind ast.ShortestKind, pathVar, relVar string, rng *ast.RangeQuantifier, extraNode bool) *ast.Match {
	rel := &ast.RelationshipPattern{
		Direction: ast.RelDirectionNone,
		Range:     rng,
		Pos:       pos(1, 20),
	}
	if relVar != "" {
		rel.Variable = ptr(relVar)
	}
	tail := &ast.PathElement{
		Relationship: rel,
		Node:         &ast.NodePattern{Variable: ptr("b"), Pos: pos(1, 30)},
	}
	if extraNode {
		tail.Next = &ast.PathElement{
			Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionNone, Range: rng, Pos: pos(1, 35)},
			Node:         &ast.NodePattern{Variable: ptr("c"), Pos: pos(1, 40)},
		}
	}
	pp := &ast.PathPattern{
		Head: &ast.PathElement{
			Node: &ast.NodePattern{Variable: ptr("a"), Pos: pos(1, 10)},
			Next: tail,
		},
		Shortest: kind,
		Pos:      pos(1, 7),
	}
	if pathVar != "" {
		pp.Variable = ptr(pathVar)
	}
	return &ast.Match{Pattern: &ast.Pattern{Paths: []*ast.PathPattern{pp}}}
}

// shortestQuery wraps a shortest-path MATCH in `MATCH (a),(b) <m> RETURN p`,
// binding the endpoints first so only the shortest-path rule under test fires.
func shortestQuery(m *ast.Match) ast.Query {
	return singleNode(
		[]ast.ReadingClause{matchNode("a"), matchNode("b"), m},
		nil, nil, returnVar("p"),
	)
}

func TestShortestPath_Accept_Unbounded(t *testing.T) {
	// p = shortestPath((a)-[*]-(b)) — omitted bounds, accepted.
	rng := &ast.RangeQuantifier{Pos: pos(1, 21)}
	q := shortestQuery(shortestMatch(ast.ShortestSingle, "p", "", rng, false))
	assertClean(t, q)
}

func TestShortestPath_Accept_LowerBoundOne(t *testing.T) {
	// p = shortestPath((a)-[*1..5]-(b)) — lower bound 1 accepted.
	rng := &ast.RangeQuantifier{Min: int64ptr(1), Max: int64ptr(5), Pos: pos(1, 21)}
	q := shortestQuery(shortestMatch(ast.ShortestSingle, "p", "", rng, false))
	assertClean(t, q)
}

func TestShortestPath_Accept_LowerBoundZero(t *testing.T) {
	// p = allShortestPaths((a)-[*0..5]-(b)) — lower bound 0 accepted.
	rng := &ast.RangeQuantifier{Min: int64ptr(0), Max: int64ptr(5), Pos: pos(1, 21)}
	q := shortestQuery(shortestMatch(ast.ShortestAll, "p", "", rng, false))
	assertClean(t, q)
}

func TestShortestPath_Accept_FixedLength(t *testing.T) {
	// p = shortestPath((a)-[r]-(b)) — fixed-length (no range) accepted.
	q := shortestQuery(shortestMatch(ast.ShortestSingle, "p", "r", nil, false))
	assertClean(t, q)
}

func TestShortestPath_Reject_LowerBoundTwo(t *testing.T) {
	// p = shortestPath((a)-[*2..5]-(b)) — lower bound 2 rejected.
	rng := &ast.RangeQuantifier{Min: int64ptr(2), Max: int64ptr(5), Pos: pos(1, 21)}
	q := shortestQuery(shortestMatch(ast.ShortestSingle, "p", "", rng, false))
	assertErrors(t, q, 1, sema.KindInvalidShortestPath)
}

func TestShortestPath_Reject_MultiSegment(t *testing.T) {
	// p = shortestPath((a)-[*]-(b)-[*]-(c)) — chained inner pattern rejected.
	rng := &ast.RangeQuantifier{Pos: pos(1, 21)}
	q := shortestQuery(shortestMatch(ast.ShortestSingle, "p", "", rng, true))
	assertErrors(t, q, 1, sema.KindInvalidShortestPath)
}

func TestShortestPath_Reject_Unnamed(t *testing.T) {
	// MATCH shortestPath((a)-[*]-(b)) — no path variable, rejected.
	rng := &ast.RangeQuantifier{Pos: pos(1, 21)}
	m := shortestMatch(ast.ShortestSingle, "", "", rng, false)
	q := singleNode(
		[]ast.ReadingClause{matchNode("a"), matchNode("b"), m},
		nil, nil, returnVar("a"),
	)
	assertErrors(t, q, 1, sema.KindInvalidShortestPath)
}

func TestShortestPath_Reject_ExpressionContext(t *testing.T) {
	// RETURN shortestPath((a)-[*]-(b)) — wrapper in expression context rejected.
	rng := &ast.RangeQuantifier{Pos: pos(1, 21)}
	pp := &ast.PathPattern{
		Head: &ast.PathElement{
			Node: &ast.NodePattern{Variable: ptr("a"), Pos: pos(1, 10)},
			Next: &ast.PathElement{
				Relationship: &ast.RelationshipPattern{Direction: ast.RelDirectionNone, Range: rng, Pos: pos(1, 20)},
				Node:         &ast.NodePattern{Variable: ptr("b"), Pos: pos(1, 30)},
			},
		},
		Shortest: ast.ShortestSingle,
		Pos:      pos(1, 7),
	}
	q := singleNode(
		[]ast.ReadingClause{matchNode("a"), matchNode("b")},
		nil, nil,
		&ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{{Expr: pp, Alias: ptr("sp")}}}},
	)
	errs := sema.Analyse(q)
	found := false
	for _, e := range errs {
		if e.Kind == sema.KindInvalidShortestPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a KindInvalidShortestPath error, got: %v", errs)
	}
}

// TestShortestPath_BoltMapping confirms the violation maps to a SyntaxError.
func TestShortestPath_BoltMapping(t *testing.T) {
	rng := &ast.RangeQuantifier{Min: int64ptr(2), Max: int64ptr(5), Pos: pos(1, 21)}
	q := shortestQuery(shortestMatch(ast.ShortestSingle, "p", "", rng, false))
	errs := sema.Analyse(q)
	se := sema.MapToBolt(errs)
	if se == nil {
		t.Fatal("expected a SemanticError")
	}
	if se.Category != sema.CategorySyntaxError || se.SubType != sema.SubTypeInvalidShortestPath {
		t.Errorf("mapping = %s.%s, want %s.%s", se.Category, se.SubType,
			sema.CategorySyntaxError, sema.SubTypeInvalidShortestPath)
	}
}
