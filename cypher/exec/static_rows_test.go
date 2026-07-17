package exec_test

// static_rows_test.go — unit tests for the StaticRows leaf operator (#1922).

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestStaticRows_EmitsAllRowsInOrder verifies StaticRows emits every supplied
// row exactly once, in slice order, then reports end-of-stream.
func TestStaticRows_EmitsAllRowsInOrder(t *testing.T) {
	t.Parallel()
	rows := []exec.Row{
		{expr.StringValue("a"), expr.IntegerValue(1)},
		{expr.StringValue("b"), expr.IntegerValue(2)},
		{expr.StringValue("c"), expr.IntegerValue(3)},
	}
	op := exec.NewStaticRows(rows)
	got, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(got) != len(rows) {
		t.Fatalf("got %d rows, want %d", len(got), len(rows))
	}
	for i := range got {
		if got[i][0] != rows[i][0] || got[i][1] != rows[i][1] {
			t.Errorf("row %d = %v, want %v", i, got[i], rows[i])
		}
	}
}

// TestStaticRows_Empty verifies a nil/empty slice yields zero rows without
// error.
func TestStaticRows_Empty(t *testing.T) {
	t.Parallel()
	for _, rows := range [][]exec.Row{nil, {}} {
		op := exec.NewStaticRows(rows)
		got, err := exec.Drain(context.Background(), op)
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d rows, want 0", len(got))
		}
	}
}

// TestStaticRows_ContextCancel verifies the operator honours a cancelled
// context on Next.
func TestStaticRows_ContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op := exec.NewStaticRows([]exec.Row{{expr.StringValue("x")}})
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var out exec.Row
	ok, err := op.Next(&out)
	if ok {
		t.Error("Next returned a row on a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Next err = %v, want context.Canceled", err)
	}
	if err := op.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
