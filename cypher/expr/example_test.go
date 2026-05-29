package expr_test

// example_test.go — runnable godoc examples for the expression evaluator
// (#1115). They show how a caller evaluates an AST expression against a row
// and parameters, and how three-valued logic surfaces in the result.

import (
	"fmt"

	"gograph/cypher/ast"
	"gograph/cypher/expr"
)

// noRegistry is a FunctionRegistry that resolves nothing; the examples below
// evaluate operator expressions that need no function lookup.
type noRegistry struct{}

func (noRegistry) Resolve(string) (expr.BuiltinFn, bool) { return nil, false }

// ExampleEval evaluates the arithmetic expression `$base + n` where n comes
// from the row and base from the params map. The result is an IntegerValue.
func ExampleEval() {
	// AST for: n + $base
	e := &ast.BinaryOp{
		Operator: "+",
		Left:     &ast.Variable{Name: "n"},
		Right:    &ast.Parameter{Name: "base"},
	}

	row := expr.RowContext{"n": expr.IntegerValue(40)}
	params := map[string]expr.Value{"base": expr.IntegerValue(2)}

	v, err := expr.Eval(e, row, params, noRegistry{})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("kind:", v.Kind())
	fmt.Println("value:", v) // IntegerValue.String prints the decimal form.
	// Output:
	// kind: Integer
	// value: 42
}

// ExampleEval_nullPropagation shows openCypher three-valued logic: any
// comparison involving NULL evaluates to NULL, not false. IsNull reports the
// outcome.
func ExampleEval_nullPropagation() {
	// AST for: n > 1, with n bound to NULL.
	e := &ast.BinaryOp{
		Operator: ">",
		Left:     &ast.Variable{Name: "n"},
		Right:    &ast.IntLiteral{Value: 1},
	}

	row := expr.RowContext{"n": expr.Null}

	v, err := expr.Eval(e, row, nil, noRegistry{})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("is null:", expr.IsNull(v))
	// Output:
	// is null: true
}
