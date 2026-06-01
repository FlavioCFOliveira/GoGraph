package ir_test

// example_test.go — runnable godoc examples for the logical-plan IR (#1109).
// They show how to translate a parsed query into a logical plan and inspect
// the resulting operator tree.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
)

// ExampleFromAST translates a parsed query into a logical plan and prints the
// canonical name of the root operator.
func ExampleFromAST() {
	q, err := parser.Parse("MATCH (n:Person) RETURN n")
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	plan, err := ir.FromAST(q)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Println("root operator:", ir.OperatorName(plan))
	// Output:
	// root operator: ProduceResults
}

// ExampleExplain renders a logical plan as a human-readable operator tree.
func ExampleExplain() {
	q, err := parser.Parse("MATCH (n:Person) RETURN n")
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	plan, err := ir.FromAST(q)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Print(ir.Explain(plan))
	// Output:
	// ProduceResults [n]
	// └─ Projection [n]
	//    └─ NodeByLabelScan [n:Person]
}
