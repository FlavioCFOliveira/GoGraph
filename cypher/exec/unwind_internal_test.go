package exec

// unwind_internal_test.go — whitebox tests that verify private invariants of
// the Unwind operator. Kept in the production package so the test can inspect
// fields that are not part of the exported API.

import (
	"context"
	"errors"
	"testing"

	"gograph/cypher/expr"
)

// initResetChild is a minimal Operator stub for verifying Unwind.Init's reset
// contract and Close's idempotency. It tracks the number of Close calls in
// closeCount so the test can assert exact invocation counts.
type initResetChild struct {
	rows       []Row
	idx        int
	closeCount int
}

func (c *initResetChild) Init(_ context.Context) error { return nil }

func (c *initResetChild) Next(out *Row) (bool, error) {
	if c.idx >= len(c.rows) {
		return false, nil
	}
	*out = c.rows[c.idx]
	c.idx++
	return true, nil
}

func (c *initResetChild) Close() error { c.closeCount++; return nil }

// TestUnwind_InitResetsCurRow verifies that Init clears every per-iteration
// field, mirroring Close. Without this guarantee, a re-Init pattern could
// leak the previous run's curRow into the new cycle.
func TestUnwind_InitResetsCurRow(t *testing.T) {
	child := &initResetChild{rows: []Row{{expr.StringValue("first")}}}
	op, err := NewUnwind(child, func(_ Row) (expr.ListValue, error) {
		return expr.ListValue{expr.IntegerValue(1)}, nil
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}

	// First cycle: consume one row to populate curRow.
	if initErr := op.Init(context.Background()); initErr != nil {
		t.Fatalf("Init: %v", initErr)
	}
	var row Row
	if ok, nextErr := op.Next(&row); !ok || nextErr != nil {
		t.Fatalf("Next: ok=%v err=%v", ok, nextErr)
	}
	if op.curRow == nil {
		t.Fatal("precondition: curRow should be populated after first Next")
	}

	// Init must zero every field — direct field inspection is the point of
	// this whitebox test.
	if reinitErr := op.Init(context.Background()); reinitErr != nil {
		t.Fatalf("re-Init: %v", reinitErr)
	}
	if op.curRow != nil {
		t.Errorf("Init did not reset curRow: got %v, want nil", op.curRow)
	}
	if op.curList != nil {
		t.Errorf("Init did not reset curList: got %v, want nil", op.curList)
	}
	if op.listIdx != 0 {
		t.Errorf("Init did not reset listIdx: got %d, want 0", op.listIdx)
	}

	if closeErr := op.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
}

// TestNewUnwind_NilChild verifies that NewUnwind rejects a nil child Operator
// with the typed sentinel ErrUnwindNilChild, instead of returning a zero-valued
// struct that would panic at Init/Close time.
func TestNewUnwind_NilChild(t *testing.T) {
	op, err := NewUnwind(nil, func(_ Row) (expr.ListValue, error) {
		return expr.ListValue{expr.IntegerValue(1)}, nil
	})
	if op != nil {
		t.Errorf("got non-nil op for nil child: %v", op)
	}
	if !errors.Is(err, ErrUnwindNilChild) {
		t.Errorf("got err = %v, want errors.Is(err, ErrUnwindNilChild)", err)
	}
}

// TestNewUnwind_NilListFn verifies that NewUnwind rejects a nil listFn with
// the typed sentinel ErrUnwindNilListFn.
func TestNewUnwind_NilListFn(t *testing.T) {
	child := &initResetChild{rows: nil}
	op, err := NewUnwind(child, nil)
	if op != nil {
		t.Errorf("got non-nil op for nil listFn: %v", op)
	}
	if !errors.Is(err, ErrUnwindNilListFn) {
		t.Errorf("got err = %v, want errors.Is(err, ErrUnwindNilListFn)", err)
	}
}

// TestUnwind_CloseIsIdempotent verifies the idempotency contract: calling
// Close more than once between two Init invocations returns nil from the
// later calls and does NOT propagate to child.Close again. Init resets the
// guard, so a fresh Init→Close cycle re-arms child.Close.
func TestUnwind_CloseIsIdempotent(t *testing.T) {
	child := &initResetChild{rows: []Row{{expr.StringValue("ctx")}}}
	op, err := NewUnwind(child, func(_ Row) (expr.ListValue, error) {
		return expr.ListValue{expr.IntegerValue(1)}, nil
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}

	if initErr := op.Init(context.Background()); initErr != nil {
		t.Fatalf("Init: %v", initErr)
	}

	// First Close — propagates to child.
	if closeErr := op.Close(); closeErr != nil {
		t.Errorf("first Close: %v", closeErr)
	}
	if child.closeCount != 1 {
		t.Errorf("after first Close: closeCount = %d, want 1", child.closeCount)
	}

	// Second & third Close — must be no-ops; closeCount stays at 1.
	if closeErr := op.Close(); closeErr != nil {
		t.Errorf("second Close: %v", closeErr)
	}
	if closeErr := op.Close(); closeErr != nil {
		t.Errorf("third Close: %v", closeErr)
	}
	if child.closeCount != 1 {
		t.Errorf("after duplicate Close calls: closeCount = %d, want 1", child.closeCount)
	}

	// Re-Init re-arms the idempotency guard; the next Close closes the child
	// again exactly once.
	if initErr := op.Init(context.Background()); initErr != nil {
		t.Fatalf("re-Init: %v", initErr)
	}
	if closeErr := op.Close(); closeErr != nil {
		t.Errorf("Close after re-Init: %v", closeErr)
	}
	if child.closeCount != 2 {
		t.Errorf("after re-Init+Close: closeCount = %d, want 2", child.closeCount)
	}
}
