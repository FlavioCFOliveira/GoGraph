package exec_test

// rollup_apply_budget_test.go — per-list element budget and in-drain
// cancellation for the RollUpApply operator (pattern-comprehension execution)
// (#1298).
//
// These mirror the sibling pipeline-breaker guards (eager_budget_test.go for
// Eager; the collect() budget in eager_aggregation_budget_test.go): a pattern
// comprehension over a supernode anchor drains the anchor's whole degree into a
// single list, so the collected list must be bounded by the same budget
// collect() uses, surfacing funcs.ErrCollectItemsExceeded when exceeded, and the
// drain must honour context cancellation so a huge enumeration is abortable
// mid-build.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// rollupCountingOp emits up to total rows (each a single IntegerValue column)
// and records how many times Next was invoked, so a cancellation test can prove
// the drain aborted before consuming the inner plan.
type rollupCountingOp struct {
	total    int
	produced int
	nextCnt  int
}

func (c *rollupCountingOp) Init(_ context.Context) error {
	c.produced = 0
	c.nextCnt = 0
	return nil
}
func (c *rollupCountingOp) Close() error { return nil }
func (c *rollupCountingOp) Next(out *exec.Row) (bool, error) {
	c.nextCnt++
	if c.produced >= c.total {
		return false, nil
	}
	*out = exec.Row{expr.IntegerValue(int64(c.produced))}
	c.produced++
	return true, nil
}

// oneRowOp emits exactly one (empty) outer row, the driver for a single
// pattern-comprehension evaluation.
type oneRowOp struct{ done bool }

func (o *oneRowOp) Init(_ context.Context) error { o.done = false; return nil }
func (o *oneRowOp) Close() error                 { return nil }
func (o *oneRowOp) Next(out *exec.Row) (bool, error) {
	if o.done {
		return false, nil
	}
	o.done = true
	*out = exec.Row{}
	return true, nil
}

// TestRollUpApply_ItemBudgetEnforced drives an inner plan that emits more rows
// than a test-lowered per-list budget and asserts the drain aborts with
// funcs.ErrCollectItemsExceeded.
//
// Pre-fix: RollUpApply had no per-list budget and drainInner appended every
// inner row, so Drain returned nil and a full list. Post-fix the bounded drain
// returns the shared typed cap error.
func TestRollUpApply_ItemBudgetEnforced(t *testing.T) {
	t.Parallel()
	outer := &oneRowOp{}
	inner := &rollupCountingOp{total: 64}
	ru := exec.NewRollUpApplyN(outer, inner, exec.NewArgument(), nil, 8) // budget below inner size

	_, err := exec.Drain(context.Background(), ru)
	if !errors.Is(err, funcs.ErrCollectItemsExceeded) {
		t.Fatalf("Drain: err = %v, want ErrCollectItemsExceeded", err)
	}
}

// TestRollUpApply_DefaultBudgetViaConstructor confirms the plain NewRollUpApply
// constructor installs the finite default budget (funcs.DefaultMaxCollectItems)
// rather than an unbounded list: a small inner well under the default drains
// fully with no error and the full list is collected.
func TestRollUpApply_DefaultBudgetViaConstructor(t *testing.T) {
	t.Parallel()
	const n = 1000
	outer := &oneRowOp{}
	inner := &rollupCountingOp{total: n}
	ru := exec.NewRollUpApply(outer, inner, exec.NewArgument(), nil) // default budget

	rows, err := exec.Drain(context.Background(), ru)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	list, ok := rows[0][len(rows[0])-1].(expr.ListValue)
	if !ok {
		t.Fatalf("collected column is %T, want expr.ListValue", rows[0][len(rows[0])-1])
	}
	if len(list) != n {
		t.Fatalf("collected list has %d elements, want %d", len(list), n)
	}
}

// TestRollUpApply_UnlimitedBudgetOptOut proves the explicit opt-out: a negative
// budget disables the cap, so an inner larger than a small budget collects every
// element with no error.
func TestRollUpApply_UnlimitedBudgetOptOut(t *testing.T) {
	t.Parallel()
	const n = 5000
	outer := &oneRowOp{}
	inner := &rollupCountingOp{total: n}
	ru := exec.NewRollUpApplyN(outer, inner, exec.NewArgument(), nil, -1) // opt-out

	rows, err := exec.Drain(context.Background(), ru)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	list, ok := rows[0][len(rows[0])-1].(expr.ListValue)
	if !ok {
		t.Fatalf("collected column is %T, want expr.ListValue", rows[0][len(rows[0])-1])
	}
	if len(list) != n {
		t.Fatalf("opt-out: collected list has %d elements, want %d", len(list), n)
	}
}

// TestRollUpApply_DrainAbortsOnCancelledContext drives a cancellable context and
// a large inner, cancels before draining, and asserts the drain returns the
// context error promptly — without consuming the inner plan. This guards the
// in-drain ctx.Err() check so a comprehension over a supernode anchor is
// abortable mid-build rather than only at the per-outer-row boundary. The
// unlimited budget ensures the cap does not mask the cancellation.
func TestRollUpApply_DrainAbortsOnCancelledContext(t *testing.T) {
	t.Parallel()
	outer := &oneRowOp{}
	inner := &rollupCountingOp{total: 1_000_000} // would dominate runtime if drained
	ru := exec.NewRollUpApplyN(outer, inner, exec.NewArgument(), nil, -1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before draining

	_, err := exec.Drain(ctx, ru)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain: err = %v, want context.Canceled", err)
	}
	if inner.nextCnt != 0 {
		t.Fatalf("inner.Next called %d times; want 0 (drain must abort before consuming)", inner.nextCnt)
	}
}
