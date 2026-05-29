package procs_test

// example_test.go — runnable godoc examples for the procedure registry
// (#1117). They show how a caller registers a custom procedure, lists the
// registered signatures, and looks up an entry to invoke its implementation.

import (
	"context"
	"errors"
	"fmt"

	"gograph/cypher/expr"
	"gograph/cypher/procs"
)

// ExampleRegistry shows the full register → list → lookup → invoke cycle on a
// fresh registry. List returns signatures sorted by fully-qualified name, so
// the output is stable across runs.
func ExampleRegistry() {
	reg := procs.NewRegistry()

	sig := procs.Signature{
		Namespace: []string{"demo"},
		Name:      "echo",
		Inputs:    []expr.Kind{expr.KindString},
		Outputs:   []procs.NamedType{{Name: "value", Kind: expr.KindString}},
	}
	impl := func(_ context.Context, args []expr.Value) ([][]expr.Value, error) {
		return [][]expr.Value{{args[0]}}, nil
	}
	if err := reg.Register(sig, impl); err != nil {
		fmt.Println("register error:", err)
		return
	}

	// List exposes every registered signature, sorted by "namespace.name".
	for _, s := range reg.List() {
		fmt.Printf("listed: %s.%s\n", s.Namespace[0], s.Name)
	}

	// Lookup retrieves the entry so the executor can invoke its implementation.
	entry, err := reg.Lookup([]string{"demo"}, "echo")
	if err != nil {
		fmt.Println("lookup error:", err)
		return
	}
	rows, err := entry.Impl(context.Background(), []expr.Value{expr.StringValue("hi")})
	if err != nil {
		fmt.Println("invoke error:", err)
		return
	}
	fmt.Printf("invoke result: %s\n", string(rows[0][0].(expr.StringValue)))
	// Output:
	// listed: demo.echo
	// invoke result: hi
}

// ExampleRegistry_Lookup_notFound shows the not-found path: Lookup returns a
// wrapped ErrProcNotFound for an unknown procedure.
func ExampleRegistry_Lookup_notFound() {
	reg := procs.NewRegistry()

	_, err := reg.Lookup([]string{"db"}, "missing")
	fmt.Println("is ErrProcNotFound:", errors.Is(err, procs.ErrProcNotFound))
	// Output:
	// is ErrProcNotFound: true
}
