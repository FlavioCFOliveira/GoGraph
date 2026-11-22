package ast_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
)

func ExampleWalk() {
	q, err := parser.Parse("MATCH (a) WHERE EXISTS { CALL db.labels() YIELD label } RETURN a")
	if err != nil {
		panic(err)
	}
	for n := range ast.Walk(q) {
		if call, ok := n.(*ast.Call); ok {
			fmt.Println(call.Procedure)
			break
		}
	}
	// Output: labels
}

func TestWalkNestedPatterns(t *testing.T) {
	q, err := parser.Parse(`
		MATCH (a) WHERE EXISTS {
			MATCH (a)-[:R*1..3]->(b) RETURN b
			UNION MATCH (a)-[:S]->(b) RETURN b
		}
		RETURN COUNT { MATCH (a)-[:T]->(c) RETURN c },
		       [(a)-[:U]->(d) | d]
	`)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for n := range ast.Walk(q) {
		switch v := n.(type) {
		case *ast.ExistsSubquery:
			kinds = append(kinds, "EXISTS")
		case *ast.MultiQuery:
			kinds = append(kinds, "UNION")
		case *ast.CountSubquery:
			kinds = append(kinds, "COUNT")
		case *ast.PatternComprehension:
			kinds = append(kinds, "comprehension")
		case *ast.PathPattern:
			kinds = append(kinds, "path")
		case *ast.RelationshipPattern:
			kinds = append(kinds, v.Types...)
		}
	}
	want := []string{"path", "EXISTS", "UNION", "path", "R", "path", "S", "COUNT", "path", "T", "comprehension", "path", "U"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("walk = %v, want %v", kinds, want)
	}
}

func TestWalkPreorderAndStop(t *testing.T) {
	lhs, rhs := &ast.Variable{Name: "a"}, &ast.Variable{Name: "b"}
	sum := &ast.BinaryOp{Operator: "+", Left: lhs, Right: rhs}
	root := &ast.Return{Projection: &ast.Projection{Items: []*ast.ProjectionItem{{Expr: sum}}, Limit: &ast.IntLiteral{Value: 2}}}
	want := []ast.Node{root, sum, lhs, rhs, root.Projection.Limit}
	seq := ast.Walk(root)
	for stop := 1; stop <= len(want); stop++ {
		var got []ast.Node
		for n := range seq {
			got = append(got, n)
			if len(got) == stop {
				break
			}
		}
		if !slices.Equal(got, want[:stop]) {
			t.Fatalf("stop %d: got %v, want %v", stop, got, want[:stop])
		}
	}
	var missing *ast.Return
	for _, root := range []ast.Node{nil, missing} {
		for range ast.Walk(root) {
			t.Fatal("visited a nil node")
		}
	}
}

func TestWalkWriteAndProjectionExpressions(t *testing.T) {
	q, err := parser.Parse(`
		MERGE (a {v: prop()})
		ON CREATE SET a.v = created()
		ON MATCH SET a.v = matched()
		RETURN CASE selected() WHEN cond() THEN result() ELSE fallback() END
		ORDER BY sorted() SKIP skipped() LIMIT limited()
	`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for n := range ast.Walk(q) {
		if f, ok := n.(*ast.FunctionInvocation); ok {
			got = append(got, f.Name)
		}
	}
	want := []string{"prop", "created", "matched", "selected", "cond", "result", "fallback", "sorted", "skipped", "limited"}
	if !slices.Equal(got, want) {
		t.Fatalf("functions = %v, want %v", got, want)
	}
}
