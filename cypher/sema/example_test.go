package sema_test

// example_test.go — runnable godoc examples for the scope-analysis pass
// (#1111). They show how Analyse reports a clean query and how it classifies a
// scope violation with a typed ErrorKind.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
)

// ExampleAnalyse shows a scope-clean query: Analyse returns an empty slice.
func ExampleAnalyse() {
	q, err := parser.Parse("MATCH (n:Person) RETURN n.name")
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}

	errs := sema.Analyse(q)
	fmt.Println("scope errors:", len(errs))
	// Output:
	// scope errors: 0
}

// ExampleAnalyse_undefinedVariable shows a query that references a variable
// never introduced by any clause. Analyse reports it as an UNDEFINED_VAR
// violation.
func ExampleAnalyse_undefinedVariable() {
	q, err := parser.Parse("MATCH (n) RETURN m")
	if err != nil {
		fmt.Println("parse error:", err)
		return
	}

	errs := sema.Analyse(q)
	if len(errs) == 0 {
		fmt.Println("query is scope-clean")
		return
	}
	fmt.Println("kind:", errs[0].Kind)
	fmt.Println("message:", errs[0].Message)
	// Output:
	// kind: UNDEFINED_VAR
	// message: undefined variable "m"
}
