package cypher_test

// reduce_test.go — end-to-end tests for the reduce() function.
//
// reduce(acc = init, x IN list | expr) is wired through the parser as a
// dedicated *ast.ReduceExpr node and evaluated by evalReduceExpr in
// cypher/expr/eval.go. These tests verify the full parse → sema → eval path.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runReduceQuery runs q against an empty graph and returns the single
// scalar value in the first column of the first row of the result.
// Fails the test on any error.
func runReduceQuery(tb testing.TB, q string) expr.Value {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngineWithRegistry(g, funcs.DefaultRegistry)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		tb.Fatalf("Run(%q): %v", q, err)
	}
	defer res.Close()
	if !res.Next() {
		tb.Fatalf("Run(%q): no rows returned", q)
	}
	cols := res.Columns()
	if len(cols) == 0 {
		tb.Fatalf("Run(%q): no columns", q)
	}
	rec := res.Record()
	v, ok := rec[cols[0]]
	if !ok {
		tb.Fatalf("Run(%q): column %q missing from record %v", q, cols[0], rec)
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("Run(%q): result error: %v", q, err)
	}
	val, _ := v.(expr.Value)
	return val
}

// TestReduce_SumIntegers verifies the canonical reduce sum example.
func TestReduce_SumIntegers(t *testing.T) {
	t.Parallel()
	got := runReduceQuery(t, "RETURN reduce(acc = 0, x IN [1, 2, 3] | acc + x)")
	if got != expr.IntegerValue(6) {
		t.Errorf("got %v want 6", got)
	}
}

// TestReduce_EmptyList verifies that reduce on an empty list returns the
// initial value unchanged.
func TestReduce_EmptyList(t *testing.T) {
	t.Parallel()
	got := runReduceQuery(t, "RETURN reduce(acc = 42, x IN [] | acc + x)")
	if got != expr.IntegerValue(42) {
		t.Errorf("got %v want 42", got)
	}
}

// TestReduce_NullList verifies that reduce on a null list returns null per
// the openCypher specification.
func TestReduce_NullList(t *testing.T) {
	t.Parallel()
	got := runReduceQuery(t, "RETURN reduce(acc = 0, x IN null | acc + x)")
	if !expr.IsNull(got) {
		t.Errorf("got %v want null", got)
	}
}

// TestReduce_StringConcat verifies accumulation over a list of strings.
func TestReduce_StringConcat(t *testing.T) {
	t.Parallel()
	got := runReduceQuery(t, `RETURN reduce(s = '', x IN ['a', 'b', 'c'] | s + x)`)
	if got != expr.StringValue("abc") {
		t.Errorf("got %v want 'abc'", got)
	}
}

// TestReduce_UppercaseKeyword verifies that REDUCE (uppercase) is accepted.
func TestReduce_UppercaseKeyword(t *testing.T) {
	t.Parallel()
	got := runReduceQuery(t, "RETURN REDUCE(acc = 0, x IN [1, 2, 3] | acc + x)")
	if got != expr.IntegerValue(6) {
		t.Errorf("got %v want 6", got)
	}
}

// TestReduce_FoldOrder verifies correct left-to-right folding order using a
// non-commutative operation: 10 - 1 - 2 - 3 = 4.
func TestReduce_FoldOrder(t *testing.T) {
	t.Parallel()
	got := runReduceQuery(t, "RETURN reduce(acc = 10, x IN [1, 2, 3] | acc - x)")
	if got != expr.IntegerValue(4) {
		t.Errorf("got %v want 4", got)
	}
}
