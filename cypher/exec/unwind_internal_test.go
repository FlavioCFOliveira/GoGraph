package exec

// unwind_internal_test.go — whitebox tests that verify private invariants of
// the Unwind operator. Kept in the production package so the test can inspect
// fields that are not part of the exported API.

import (
	"context"
	"testing"

	"gograph/cypher/expr"
)

// initResetChild is a minimal Operator stub for verifying Unwind.Init's reset
// contract. It emits a fixed sequence of rows; Close is a no-op.
type initResetChild struct {
	rows []Row
	idx  int
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

func (*initResetChild) Close() error { return nil }

// TestUnwind_InitResetsCurRow verifies that Init clears every per-iteration
// field, mirroring Close. Without this guarantee, a re-Init pattern could
// leak the previous run's curRow into the new cycle.
func TestUnwind_InitResetsCurRow(t *testing.T) {
	child := &initResetChild{rows: []Row{{expr.StringValue("first")}}}
	op := NewUnwind(child, func(_ Row) (expr.ListValue, error) {
		return expr.ListValue{expr.IntegerValue(1)}, nil
	})

	// First cycle: consume one row to populate curRow.
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row Row
	if ok, err := op.Next(&row); !ok || err != nil {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	if op.curRow == nil {
		t.Fatal("precondition: curRow should be populated after first Next")
	}

	// Init must zero every field — direct field inspection is the point of
	// this whitebox test.
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("re-Init: %v", err)
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

	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
