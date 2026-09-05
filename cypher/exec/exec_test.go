package exec_test

// exec_test.go — tests for the Operator interface and Drain (tasks-234, 235).
//
// Coverage targets:
//   - Drain: end-of-stream, error propagation, context cancellation ≤ 100ms, Close always called.
//   - Pipeline chaining: FilterOperator above SliceOperator.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers — minimal Operator implementations for testing
// ─────────────────────────────────────────────────────────────────────────────

// sliceOperator emits a pre-defined slice of rows.
type sliceOperator struct {
	rows   []exec.Row
	idx    int
	ctx    context.Context //nolint:containedctx // test helper stores ctx intentionally
	closed bool
}

func newSliceOperator(rows ...exec.Row) *sliceOperator { return &sliceOperator{rows: rows} }

func (s *sliceOperator) Init(ctx context.Context) error {
	s.ctx = ctx
	s.idx = 0
	return nil
}

func (s *sliceOperator) Next(out *exec.Row) (bool, error) {
	if err := s.ctx.Err(); err != nil {
		return false, err
	}
	if s.idx >= len(s.rows) {
		return false, nil
	}
	*out = s.rows[s.idx]
	s.idx++
	return true, nil
}

func (s *sliceOperator) Close() error {
	s.closed = true
	return nil
}

// errorOperator returns an error after n rows.
type errorOperator struct {
	inner     exec.Operator
	failAfter int
	count     int
	closed    bool
}

func (e *errorOperator) Init(ctx context.Context) error { return e.inner.Init(ctx) }
func (e *errorOperator) Next(out *exec.Row) (bool, error) {
	if e.count >= e.failAfter {
		return false, errors.New("errorOperator: forced error")
	}
	ok, err := e.inner.Next(out)
	if ok {
		e.count++
	}
	return ok, err
}
func (e *errorOperator) Close() error {
	e.closed = true
	return e.inner.Close()
}

// infiniteOperator emits rows forever until context is cancelled.
type infiniteOperator struct {
	ctx    context.Context //nolint:containedctx // test helper stores ctx intentionally
	count  int
	closed bool
}

func (op *infiniteOperator) Init(ctx context.Context) error { op.ctx = ctx; return nil }
func (op *infiniteOperator) Next(out *exec.Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	// Check context every 4096 iterations as per operator contract.
	if op.count%4096 == 0 {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}
	}
	*out = exec.Row{expr.IntegerValue(int64(op.count))}
	op.count++
	return true, nil
}
func (op *infiniteOperator) Close() error { op.closed = true; return nil }

// ─────────────────────────────────────────────────────────────────────────────
// 6. Drain — end-of-stream, rows collected
// ─────────────────────────────────────────────────────────────────────────────

func TestDrain_EndOfStream(t *testing.T) {
	op := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
		exec.Row{expr.IntegerValue(3)},
	)
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain unexpected error: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("Drain returned %d rows, want 3", len(rows))
	}
	if !op.closed {
		t.Error("Close was not called after successful Drain")
	}
	for i, row := range rows {
		want := expr.IntegerValue(int64(i + 1))
		if row[0] != want {
			t.Errorf("rows[%d][0] = %v, want %v", i, row[0], want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Drain — error propagation, Close still called
// ─────────────────────────────────────────────────────────────────────────────

func TestDrain_ErrorPropagation(t *testing.T) {
	inner := newSliceOperator(
		exec.Row{expr.IntegerValue(1)},
		exec.Row{expr.IntegerValue(2)},
		exec.Row{expr.IntegerValue(3)},
	)
	op := &errorOperator{inner: inner, failAfter: 2}

	rows, err := exec.Drain(context.Background(), op)
	if err == nil {
		t.Fatal("expected error from Drain, got nil")
	}
	if !op.closed {
		t.Error("Close was not called after error")
	}
	// We got 2 rows before the error.
	if len(rows) != 2 {
		t.Errorf("got %d rows before error, want 2", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. Drain — context cancellation honoured within 100ms
// ─────────────────────────────────────────────────────────────────────────────

func TestDrain_CancellationWithin100ms(t *testing.T) {
	op := &infiniteOperator{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := exec.Drain(ctx, op)
		done <- err
	}()

	// Cancel after a brief moment to let the goroutine start.
	time.Sleep(5 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Drain should have returned an error after cancellation")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Drain did not return within 100ms after context cancellation")
	}

	if !op.closed {
		t.Error("Close was not called after context cancellation")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 9. Drain — empty operator
// ─────────────────────────────────────────────────────────────────────────────

func TestDrain_Empty(t *testing.T) {
	op := newSliceOperator()
	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
	if !op.closed {
		t.Error("Close not called for empty operator")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 10. Benchmarks
// ─────────────────────────────────────────────────────────────────────────────

func BenchmarkDrain_Throughput(b *testing.B) {
	const nRows = 1000
	rows := make([]exec.Row, nRows)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(int64(i))}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		op := newSliceOperator(rows...)
		_, err := exec.Drain(context.Background(), op)
		if err != nil {
			b.Fatal(err)
		}
	}
}
