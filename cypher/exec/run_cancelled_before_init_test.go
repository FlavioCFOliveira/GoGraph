package exec_test

// run_cancelled_before_init_test.go — rmp #2996.
//
// A statement whose context is cancelled while it is being planned must not
// pay for its operators' Init. A label scan's Init resolves the scan's MVCC
// snapshot bitmap, which costs O(uncommitted writes in the label) and does not
// consult the context; example 37's L17 measured a statement cancelled 2 ms in
// returning only after every scan's Init had run.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// initCountingOperator records how many times Init and Close ran.
type initCountingOperator struct {
	inits, closes int
}

func (o *initCountingOperator) Init(context.Context) error { o.inits++; return nil }
func (o *initCountingOperator) Next(*exec.Row) (bool, error) {
	return false, nil
}
func (o *initCountingOperator) Close() error { o.closes++; return nil }

// TestRun_CancelledContext_SkipsInit asserts that exec.Run with an already
// cancelled context does not initialise the plan, releases it exactly once, and
// reports the context error.
func TestRun_CancelledContext_SkipsInit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	op := &initCountingOperator{}
	rs := exec.Run(ctx, op, []string{"c"})
	if op.inits != 0 {
		t.Fatalf("Init ran %d time(s) on a cancelled context; want 0", op.inits)
	}
	if rs.Next() {
		t.Fatal("Next returned a row on a cancelled context")
	}
	if err := rs.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Err() = %v; want errors.Is(err, context.Canceled)", err)
	}
	if err := rs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if op.closes != 1 {
		t.Fatalf("plan closed %d time(s); want exactly 1", op.closes)
	}
}

// TestRun_LiveContext_InitsOnce asserts the live-context path is unchanged:
// the plan is initialised once and closed once.
func TestRun_LiveContext_InitsOnce(t *testing.T) {
	t.Parallel()
	op := &initCountingOperator{}
	rs := exec.Run(context.Background(), op, []string{"c"})
	if op.inits != 1 {
		t.Fatalf("Init ran %d time(s); want 1", op.inits)
	}
	for rs.Next() {
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("Err() = %v; want nil", err)
	}
	if err := rs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if op.closes != 1 {
		t.Fatalf("plan closed %d time(s); want exactly 1", op.closes)
	}
}
