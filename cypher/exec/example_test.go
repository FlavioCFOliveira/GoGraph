package exec_test

// example_test.go — runnable godoc examples for the Volcano-style executor
// (#1114). They show how a caller assembles a small operator pipeline and
// drains it to a result set, without touching the graph store.

import (
	"context"
	"fmt"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
)

// ExampleDrain assembles the equivalent of `UNWIND [10, 20, 30] AS x RETURN x
// LIMIT 2` as a hand-built operator tree and drains it. SingleRow seeds one
// empty row, Unwind expands the literal list, and Limit caps the output.
func ExampleDrain() {
	// SingleRow emits exactly one empty row to drive the pipeline.
	src := exec.NewSingleRowOperator()

	// Unwind expands a fixed list; listFn ignores the (empty) input row.
	unwind, err := exec.NewUnwind(src, func(exec.Row) (expr.ListValue, error) {
		return expr.ListValue{
			expr.IntegerValue(10),
			expr.IntegerValue(20),
			expr.IntegerValue(30),
		}, nil
	})
	if err != nil {
		fmt.Println("NewUnwind:", err)
		return
	}

	// Limit passes at most two rows downstream.
	limit, err := exec.NewLimit(unwind, 2)
	if err != nil {
		fmt.Println("NewLimit:", err)
		return
	}

	// Drain runs the pipeline and always closes it before returning.
	rows, err := exec.Drain(context.Background(), limit)
	if err != nil {
		fmt.Println("Drain:", err)
		return
	}

	fmt.Println("rows:", len(rows))
	for _, r := range rows {
		fmt.Println("x =", r[0])
	}
	// Output:
	// rows: 2
	// x = 10
	// x = 20
}
