package exec_test

// correlated_apply_test.go — tests for CorrelatedApply and OptionalApply
// (task-392).
//
// CorrelatedApply forwards each inner row verbatim per outer iteration.
// OptionalApply additionally emits one NULL-extended row per outer iteration
// that yielded zero inner rows.

import (
	"context"
	"errors"
	"testing"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// CorrelatedApply
// ─────────────────────────────────────────────────────────────────────────────

// TestCorrelatedApply_ForwardsInnerRowsVerbatim verifies that CorrelatedApply
// emits inner rows without concatenating the outer row (the inner pipeline is
// expected to have re-emitted the outer columns via its Argument leaf).
func TestCorrelatedApply_ForwardsInnerRowsVerbatim(t *testing.T) {
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(10)},
		exec.Row{expr.IntegerValue(20)},
	)
	arg := exec.NewArgument()
	// Inner is the Argument leaf itself: it re-emits the outer row.
	op := exec.NewCorrelatedApply(outer, arg, arg)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	// Each output row should be width 1 (just the outer column re-emitted by
	// Argument), not 2 — CorrelatedApply does NOT concatenate outer || inner.
	for i, row := range rows {
		if got, want := len(row), 1; got != want {
			t.Errorf("rows[%d] width = %d, want %d", i, got, want)
		}
	}
	if rows[0][0] != expr.IntegerValue(10) || rows[1][0] != expr.IntegerValue(20) {
		t.Errorf("rows = %v, want [[10], [20]]", rows)
	}
}

// TestCorrelatedApply_EmptyOuter verifies that no rows are emitted when the
// outer plan is empty.
func TestCorrelatedApply_EmptyOuter(t *testing.T) {
	outer := newSliceOperator()
	arg := exec.NewArgument()
	op := exec.NewCorrelatedApply(outer, arg, arg)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

// TestCorrelatedApply_EmptyInner verifies that an outer row producing no inner
// rows is silently dropped (CorrelatedApply is an INNER join).
func TestCorrelatedApply_EmptyInner(t *testing.T) {
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	arg := exec.NewArgument()
	// Empty inner — Argument is unused (it never gets Next-called from below).
	inner := newSliceOperator()
	op := exec.NewCorrelatedApply(outer, inner, arg)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 (inner always empty → INNER join drops)", len(rows))
	}
}

// TestCorrelatedApply_CancelledContext verifies that CorrelatedApply honours
// ctx cancellation.
func TestCorrelatedApply_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	outer := newSliceOperator(exec.Row{expr.IntegerValue(1)})
	arg := exec.NewArgument()
	op := exec.NewCorrelatedApply(outer, arg, arg)

	_, err := exec.Drain(ctx, op)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// OptionalApply
// ─────────────────────────────────────────────────────────────────────────────

// TestOptionalApply_EmitsInnerWhenMatched verifies that matching outer rows
// forward the inner rows verbatim, exactly like CorrelatedApply.
func TestOptionalApply_EmitsInnerWhenMatched(t *testing.T) {
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	arg := exec.NewArgument()
	op := exec.NewOptionalApply(outer, arg, arg, 1)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if rows[0][0] != expr.IntegerValue(1) || rows[1][0] != expr.IntegerValue(2) {
		t.Errorf("rows = %v, want [[1], [2]]", rows)
	}
}

// TestOptionalApply_EmitsNullRowWhenInnerEmpty verifies that an outer row
// producing zero inner rows still yields one row, with NULL-padded inner cols.
func TestOptionalApply_EmitsNullRowWhenInnerEmpty(t *testing.T) {
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	arg := exec.NewArgument()
	// Empty inner — Argument is unused. paddedWidth = outerWidth(1) + extra(2) = 3.
	inner := newSliceOperator()
	op := exec.NewOptionalApply(outer, inner, arg, 3)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("rows = %d, want %d (one NULL-row per outer)", got, want)
	}
	for i, row := range rows {
		if got, want := len(row), 3; got != want {
			t.Errorf("rows[%d] width = %d, want %d", i, got, want)
		}
		// outerCol matches input.
		if row[0] != expr.IntegerValue(int64(i+1)) {
			t.Errorf("rows[%d][0] = %v, want IntegerValue(%d)", i, row[0], i+1)
		}
		// padded inner cols are NULL.
		if row[1] != expr.Null || row[2] != expr.Null {
			t.Errorf("rows[%d] padded cols = %v, %v; want Null, Null", i, row[1], row[2])
		}
	}
}

// TestOptionalApply_EmptyOuter verifies that no rows are emitted when the
// outer plan is empty (OPTIONAL MATCH does not synthesise a row from nothing
// at this layer).
func TestOptionalApply_EmptyOuter(t *testing.T) {
	outer := newSliceOperator()
	arg := exec.NewArgument()
	inner := newSliceOperator()
	op := exec.NewOptionalApply(outer, inner, arg, 3)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

// TestOptionalApply_MixedMatchAndNullRow verifies that some outer rows can match
// (producing real inner rows) while others trigger the NULL row.
func TestOptionalApply_MixedMatchAndNullRow(t *testing.T) {
	// Outer row 0 matches twice; outer row 1 matches zero times.
	// We simulate this by using a programmable inner operator.
	outer := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
	)
	arg := exec.NewArgument()
	inner := &outerKeyedOperator{
		arg:     arg,
		matches: map[int64][]exec.Row{1: {{expr.IntegerValue(1), expr.IntegerValue(100)}}},
	}
	op := exec.NewOptionalApply(outer, inner, arg, 2)

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	// outer=1 → one inner row [1, 100].
	if rows[0][0] != expr.IntegerValue(1) || rows[0][1] != expr.IntegerValue(100) {
		t.Errorf("rows[0] = %v, want [1, 100]", rows[0])
	}
	// outer=2 → no inner row → NULL row [2, NULL].
	if rows[1][0] != expr.IntegerValue(2) || rows[1][1] != expr.Null {
		t.Errorf("rows[1] = %v, want [2, NULL]", rows[1])
	}
}

// outerKeyedOperator is a test-only inner operator that emits the pre-loaded
// rows for the outer row currently set on its Argument. It is initialised once
// per outer iteration via OptionalApply.
type outerKeyedOperator struct {
	arg     *exec.Argument
	matches map[int64][]exec.Row
	current []exec.Row
	idx     int
}

func (op *outerKeyedOperator) Init(_ context.Context) error {
	// The Argument's outer row is what the Apply just seeded. Inspect it via
	// the Argument's Next to discover the outer key, then load the slice of
	// matching inner rows.
	var probe exec.Row
	if err := op.arg.Init(context.Background()); err != nil {
		return err
	}
	if _, err := op.arg.Next(&probe); err != nil {
		return err
	}
	op.idx = 0
	if len(probe) == 0 {
		op.current = nil
		return nil
	}
	key := int64(probe[0].(expr.IntegerValue))
	op.current = op.matches[key]
	return nil
}

func (op *outerKeyedOperator) Next(out *exec.Row) (bool, error) {
	if op.idx >= len(op.current) {
		return false, nil
	}
	*out = op.current[op.idx]
	op.idx++
	return true, nil
}

func (op *outerKeyedOperator) Close() error { return nil }
