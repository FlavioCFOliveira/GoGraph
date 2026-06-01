package funcs_test

// example_test.go — runnable godoc examples for the built-in function
// registry (#1116). They show how a caller resolves a Cypher built-in by
// name and invokes it with runtime values.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// ExampleRegistry_Resolve resolves the built-in toUpper from DefaultRegistry
// and invokes it. Lookup is case-insensitive: names are lower-cased before
// resolution, so "toUpper" and "toupper" reach the same implementation.
func ExampleRegistry_Resolve() {
	fn, ok := funcs.DefaultRegistry.Resolve("toupper")
	if !ok {
		fmt.Println("toupper not registered")
		return
	}

	out, err := fn([]expr.Value{expr.StringValue("hello")})
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	// out is a StringValue; print its raw text rather than the quoted String().
	fmt.Println(string(out.(expr.StringValue)))
	// Output:
	// HELLO
}

// ExampleRegistry_Resolve_unknown shows the not-found path: Resolve reports
// false for a name that is not registered.
func ExampleRegistry_Resolve_unknown() {
	_, ok := funcs.DefaultRegistry.Resolve("no_such_function")
	fmt.Println("found:", ok)
	// Output:
	// found: false
}
