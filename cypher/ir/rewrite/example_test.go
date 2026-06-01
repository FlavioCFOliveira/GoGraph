package rewrite_test

// example_test.go — runnable godoc examples for the IR rewrite framework
// (#1110). They show how an optimisation rule reshapes a logical plan, both by
// applying a single rule directly and by running a Registry of rules through a
// Driver.

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir/rewrite"
)

// selectOverProjection builds the plan
//
//	Selection(n.age > 18) → Projection(n) → AllNodesScan(n)
//
// which is the canonical input for the predicate-pushdown rule.
func selectOverProjection() ir.LogicalPlan {
	return ir.NewSelection("n.age > 18",
		ir.NewProjection(
			[]ir.ProjectionItem{{Name: "n", Expression: "n"}},
			ir.NewAllNodesScan("n"),
		),
	)
}

// ExamplePredicatePushdown applies a single rewrite rule and shows how the
// Selection is pushed below the Projection so the filter runs earlier.
func ExamplePredicatePushdown() {
	plan := selectOverProjection()
	fmt.Print("before:\n", ir.Explain(plan))

	optimised, changed := rewrite.PredicatePushdown{}.Apply(plan)
	fmt.Println("changed:", changed)
	fmt.Print("after:\n", ir.Explain(optimised))
	// Output:
	// before:
	// Selection [n.age > 18]
	// └─ Projection [n]
	//    └─ AllNodesScan [n]
	// changed: true
	// after:
	// Projection [n]
	// └─ Selection [n.age > 18]
	//    └─ AllNodesScan [n]
}

// ExampleDriver runs a Registry of rules to a fixed point, returning the
// optimised plan and the number of rewrites applied.
func ExampleDriver() {
	reg := &rewrite.Registry{}
	reg.Register(rewrite.PredicatePushdown{})

	driver := rewrite.NewDriver(reg)
	optimised, count := driver.Run(context.Background(), selectOverProjection())

	fmt.Println("rewrites applied:", count)
	fmt.Print(ir.Explain(optimised))
	// Output:
	// rewrites applied: 1
	// Projection [n]
	// └─ Selection [n.age > 18]
	//    └─ AllNodesScan [n]
}
