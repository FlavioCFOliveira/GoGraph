package exec_test

// eager_budget_test.go — memory cap and in-drain cancellation for the Eager
// pipeline-breaker (task #1295).
//
// These tests mirror the sibling pipeline-breaker guards (TestSort_MemoryCapEnforced,
// TestSort context-cancel-during-collect) for the Eager barrier:
//
//   - the Init drain is bounded by maxRows and surfaces ErrEagerMemoryExceeded;
//   - the Init drain honours context cancellation via an in-loop ctx.Err()
//     check, so a cancelled context aborts buffering promptly instead of
//     consuming the whole child first;
//   - a child under the cap with a live context drains fully without error.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// countingOp emits up to total rows and records how many times Next was
// actually invoked. It is used to prove that a cancelled-before-Init drain
// returns without consuming the child (promptness).
type countingOp struct {
	total    int
	produced int
	nextCnt  int
}

func (c *countingOp) Init(_ context.Context) error {
	c.produced = 0
	c.nextCnt = 0
	return nil
}
func (c *countingOp) Close() error { return nil }
func (c *countingOp) Next(out *exec.Row) (bool, error) {
	c.nextCnt++
	if c.produced >= c.total {
		return false, nil
	}
	*out = exec.Row{expr.IntegerValue(int64(c.produced))}
	c.produced++
	return true, nil
}

// TestEager_MemoryCapEnforced wraps a child emitting more rows than a
// test-lowered cap and asserts the Init drain aborts with
// ErrEagerMemoryExceeded.
//
// Pre-fix: Eager had no maxRows field and drained unbounded, so Init returned
// nil. Post-fix: the bounded drain returns the typed error.
func TestEager_MemoryCapEnforced(t *testing.T) {
	t.Parallel()
	child := &countingOp{total: 5}
	eager := exec.NewEager(child, 3) // cap below child size

	err := eager.Init(context.Background())
	if !errors.Is(err, exec.ErrEagerMemoryExceeded) {
		t.Fatalf("Init: err = %v, want ErrEagerMemoryExceeded", err)
	}
	_ = eager.Close()
}

// TestEager_MemoryCapEnforcedViaDrain asserts the cap error survives the Drain
// helper (and therefore would surface via Result.Err() at the engine level),
// since Drain wraps Init errors with %w.
func TestEager_MemoryCapEnforcedViaDrain(t *testing.T) {
	t.Parallel()
	child := &countingOp{total: 100}
	eager := exec.NewEager(child, 10)

	_, err := exec.Drain(context.Background(), eager)
	if !errors.Is(err, exec.ErrEagerMemoryExceeded) {
		t.Fatalf("Drain: err = %v, want ErrEagerMemoryExceeded", err)
	}
}

// TestEager_InitAbortsOnCancelledContext drives a cancellable context and a
// large child, cancels the context before Init, and asserts Init returns the
// context error promptly — without consuming the child.
//
// Pre-fix: the Init drain had no in-loop ctx check (the only ctx.Err() lived in
// Next, which runs after the full drain), so Init drained the entire child and
// returned nil. Post-fix: the iter%4096==0 check fires at iteration 0 and
// aborts before pulling any row.
func TestEager_InitAbortsOnCancelledContext(t *testing.T) {
	t.Parallel()
	child := &countingOp{total: 1_000_000} // large: would dominate runtime if drained
	eager := exec.NewEager(child, 0)       // default cap; cap must not mask the ctx abort

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Init

	err := eager.Init(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Init: err = %v, want context.Canceled", err)
	}
	if child.nextCnt != 0 {
		t.Fatalf("child.Next called %d times; want 0 (drain must abort before consuming)", child.nextCnt)
	}
	_ = eager.Close()
}

// TestEager_SmallChildUnderDefaultDrainsFully is the control: a child well
// under the default cap with a live context drains fully and emits every row
// with no error.
func TestEager_SmallChildUnderDefaultDrainsFully(t *testing.T) {
	t.Parallel()
	const n = 1000
	child := &countingOp{total: n}
	eager := exec.NewEager(child, 0) // default cap (10M)

	rows, err := exec.Drain(context.Background(), eager)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != n {
		t.Fatalf("got %d rows, want %d", len(rows), n)
	}
	for i, r := range rows {
		got, ok := r[0].(expr.IntegerValue)
		if !ok || int64(got) != int64(i) {
			t.Fatalf("row[%d] = %v, want IntegerValue(%d)", i, r[0], i)
		}
	}
}
