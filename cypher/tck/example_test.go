package tck_test

// example_test.go — runnable godoc examples for the TCK scenario harness
// (#1118). They show how a caller loads the embedded openCypher TCK corpus
// and drives a single scenario through the parser, mirroring what the
// parser-only runner does — without executing the full suite.

import (
	"fmt"

	"gograph/cypher/parser"
	"gograph/cypher/tck"
)

// ExampleLoadScenarios loads the embedded TCK corpus and inspects one
// scenario. The corpus content is fixed at build time, so "the corpus is
// non-empty" and "each scenario carries a query and a file path" are stable
// facts to assert; the exact scenario count is intentionally not pinned here.
func ExampleLoadScenarios() {
	scenarios, err := tck.LoadScenarios()
	if err != nil {
		fmt.Println("load error:", err)
		return
	}

	fmt.Println("corpus non-empty:", len(scenarios) > 0)

	// Every scenario exposes the query string and originating feature file.
	first := scenarios[0]
	fmt.Println("has query:", first.Query != "")
	fmt.Println("has file:", first.File != "")
	// Output:
	// corpus non-empty: true
	// has query: true
	// has file: true
}

// ExampleScenario shows how the harness consumer turns a scenario's Query into
// an AST: this is the core operation the parser-only runner performs for every
// non-skipped scenario. Here a representative query is parsed directly so the
// example stays deterministic and independent of corpus ordering.
func ExampleScenario() {
	// A Scenario carries the Cypher string lifted from its
	// "When executing query:" step; feed that to parser.Parse.
	s := &tck.Scenario{
		File:  "features/clauses/return/Return1.feature",
		Name:  "[1] Return a single value",
		Query: "RETURN 1 AS one",
	}

	_, err := parser.Parse(s.Query)
	fmt.Println("parses cleanly:", err == nil)
	// Output:
	// parses cleanly: true
}
