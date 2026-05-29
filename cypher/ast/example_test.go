package ast_test

// example_test.go — runnable godoc examples for the AST package (#1108).
// They show how to inspect a parsed query by type-switching its clauses and
// how to render an AST back to canonical Cypher text.

import (
	"fmt"

	"gograph/cypher/ast"
	"gograph/cypher/parser"
)

// ExamplePrint renders a parsed AST back to canonical Cypher source text.
// Print is the inverse-direction helper used by tooling and debug output.
func ExamplePrint() {
	q, err := parser.Parse("MATCH (n:Person) RETURN n.name")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(ast.Print(q))
	// Output:
	// MATCH (n:Person) RETURN n.name
}

// ExampleSingleQuery shows how to inspect the clause structure of a parsed
// query by type-switching the typed AST nodes.
func ExampleSingleQuery() {
	q, err := parser.Parse("MATCH (n:Person) WHERE n.age > 18 RETURN n.name")
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		fmt.Printf("unexpected root: %T\n", q)
		return
	}

	for _, clause := range sq.ReadingClauses {
		switch c := clause.(type) {
		case *ast.Match:
			fmt.Println("MATCH with WHERE:", c.Where != nil)
		default:
			fmt.Printf("other reading clause: %T\n", c)
		}
	}
	fmt.Println("RETURN present:", sq.Return != nil)
	// Output:
	// MATCH with WHERE: true
	// RETURN present: true
}
