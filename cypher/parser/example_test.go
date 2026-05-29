package parser_test

import (
	"errors"
	"fmt"

	"gograph/cypher/ast"
	"gograph/cypher/parser"
)

// ExampleParse demonstrates basic Cypher parsing. The returned AST can be
// inspected or pretty-printed by downstream tooling.
func ExampleParse() {
	q, err := parser.Parse("MATCH (n:Person) RETURN n.name")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("ok, type:", fmt.Sprintf("%T", q))
	// Output:
	// ok, type: *ast.SingleQuery
}

// ExampleParseStrict demonstrates multi-error collection for tooling use cases
// such as editors and linters. ParseStrict reports all syntax errors (up to the
// internal cap) rather than stopping at the first.
func ExampleParseStrict() {
	// Two independent syntax errors separated by a semicolon.
	_, errs := parser.ParseStrict("RETURN , ; RETURN ,")
	if len(errs) == 0 {
		fmt.Println("no errors")
		return
	}

	for _, e := range errs {
		var pe *parser.ParseError
		if errors.As(e, &pe) {
			fmt.Printf("syntax error at %d:%d\n", pe.Line, pe.Column)
		}
	}
	// Output:
	// syntax error at 1:7
	// syntax error at 1:11
}

// ExampleParse_inspectAST shows how to inspect the AST root that Parse returns.
// A single-part query parses to *ast.SingleQuery, whose reading clauses and
// RETURN can then be walked by downstream stages.
func ExampleParse_inspectAST() {
	q, err := parser.Parse("MATCH (n:Person) RETURN n.name")
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		fmt.Printf("unexpected root: %T\n", q)
		return
	}

	fmt.Println("reading clauses:", len(sq.ReadingClauses))
	fmt.Println("has RETURN:", sq.Return != nil)
	if _, isMatch := sq.ReadingClauses[0].(*ast.Match); isMatch {
		fmt.Println("first clause: MATCH")
	}
	// Output:
	// reading clauses: 1
	// has RETURN: true
	// first clause: MATCH
}
