package funcs

// list_funcs.go — extended list built-ins for the Cypher function registry (task-266).
//
// Adds: sort (list), extract (deprecated alias for list comprehension),
//       filter (deprecated alias for list comprehension with predicate).
//
// NOTE: all(x IN list WHERE pred), any(...), none(...), single(...), and
// reduce(acc=init, x IN list | expr) are handled specially by the evaluator
// (evalQuantifier / evalReduce in cypher/expr/eval.go) because they receive
// a ListComprehension AST node as their argument. They are NOT registered here.

import (
	"sort"

	"gograph/cypher/expr"
)

// registerListFuncs registers extended list built-ins into r.
func registerListFuncs(r *Registry) {
	r.Register("sort", fnSort)
	// extract(x IN list | expr) and filter(x IN list WHERE pred) are legacy
	// Neo4j functions. They are now represented at the AST level as
	// ListComprehensions and dispatched by the evaluator. Register no-op stubs
	// so that a direct fn-call path (from hand-crafted FunctionInvocations) does
	// not produce "unknown function" errors at runtime; the stubs receive the
	// already-evaluated result of the ListComprehension arg and pass it through.
	r.Register("extract", fnExtractStub)
	r.Register("filter", fnFilterStub)
}

// fnSort returns a new sorted copy of a list of comparable values.
// Sorting follows the openCypher 9 total ordering (via expr.Compare).
// NULL → NULL; non-list → TypeError.
func fnSort(args []expr.Value) (expr.Value, error) {
	if err := requireArity("sort", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	lv, ok := args[0].(expr.ListValue)
	if !ok {
		return nil, &TypeError{Function: "sort", ArgIndex: 0, Got: args[0].Kind(), Want: "List"}
	}
	result := make(expr.ListValue, len(lv))
	copy(result, lv)
	sort.SliceStable(result, func(i, j int) bool {
		return expr.Compare(result[i], result[j]) < 0
	})
	return result, nil
}

// fnExtractStub passes through a pre-evaluated ListValue argument.
// The real extract() logic runs in the evaluator (list comprehension).
func fnExtractStub(args []expr.Value) (expr.Value, error) {
	if err := requireArity("extract", args, 1); err != nil {
		return nil, err
	}
	return args[0], nil
}

// fnFilterStub passes through a pre-evaluated ListValue argument.
// The real filter() logic runs in the evaluator (list comprehension).
func fnFilterStub(args []expr.Value) (expr.Value, error) {
	if err := requireArity("filter", args, 1); err != nil {
		return nil, err
	}
	return args[0], nil
}
