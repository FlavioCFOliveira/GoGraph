package explain_test

// example_test.go — runnable godoc examples for plan rendering (#1113).
// They show how to render a logical plan as Neo4j-style text via TextTree.

import (
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/explain"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// ExampleTextTree renders a logical plan as a columnar text table. The tree
// follows the plan's child order, so the output is stable across runs.
func ExampleTextTree() {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewNodeByLabelScan("n", "Person"),
	)

	out := explain.TextTree(plan)

	// The rendered table is a fixed-width box; assert its structure rather
	// than embedding the exact padding so the example stays readable.
	fmt.Println("has header:", strings.Contains(out, "Operator") && strings.Contains(out, "Est.Rows"))
	fmt.Println("has root:", strings.Contains(out, "ProduceResults"))
	fmt.Println("has child:", strings.Contains(out, "NodeByLabelScan"))
	fmt.Println("child is nested:", strings.Contains(out, "└─ NodeByLabelScan"))
	// Output:
	// has header: true
	// has root: true
	// has child: true
	// child is nested: true
}
