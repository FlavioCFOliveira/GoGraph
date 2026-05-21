package exec_test

// exec_test.go — tests for RowSlab, Operator interface, and Drain (tasks-234, 235).
//
// Coverage targets:
//   - RowSlab: zero-alloc hot path after pool warmup, ErrSlabOverflow, race-clean.
//   - Drain: end-of-stream, error propagation, context cancellation ≤ 100ms, Close always called.
//   - Pipeline chaining: FilterOperator above SliceOperator.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
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
// 1. RowSlab basic allocation
// ─────────────────────────────────────────────────────────────────────────────

func TestRowSlab_Alloc(t *testing.T) {
	s := exec.NewRowSlab(3, 8)

	for i := range 8 {
		row, err := s.Alloc()
		if err != nil {
			t.Fatalf("Alloc[%d] unexpected error: %v", i, err)
		}
		if len(row) != 3 {
			t.Fatalf("Alloc[%d] row width = %d, want 3", i, len(row))
		}
	}
	if s.Len() != 8 {
		t.Errorf("Len() = %d, want 8", s.Len())
	}

	// 9th alloc must overflow.
	_, err := s.Alloc()
	if !errors.Is(err, exec.ErrSlabOverflow) {
		t.Errorf("expected ErrSlabOverflow, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. RowSlab Reset — counter and value zeroing
// ─────────────────────────────────────────────────────────────────────────────

func TestRowSlab_Reset(t *testing.T) {
	s := exec.NewRowSlab(2, 4)
	row, _ := s.Alloc()
	row[0] = expr.IntegerValue(42)
	row[1] = expr.StringValue("x")

	s.Reset()

	if s.Len() != 0 {
		t.Errorf("after Reset: Len() = %d, want 0", s.Len())
	}

	// Reallocate: slots must be nil (zeroed).
	row2, err := s.Alloc()
	if err != nil {
		t.Fatalf("Alloc after Reset: %v", err)
	}
	for i, v := range row2 {
		if v != nil {
			t.Errorf("slot[%d] after Reset = %v, want nil", i, v)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2b. RowSlab variable-width methods (AllocRaw / SetRow / GetRow / Cap)
// ─────────────────────────────────────────────────────────────────────────────

func TestRowSlab_VarWidth(t *testing.T) {
	t.Parallel()
	s := exec.NewRowSlab(0, 4) // width=0 → variable-width slab
	if s.Cap() != 4 {
		t.Errorf("Cap() = %d, want 4", s.Cap())
	}

	idx, err := s.AllocRaw()
	if err != nil {
		t.Fatalf("AllocRaw: %v", err)
	}
	r := exec.Row{expr.StringValue("hello"), expr.IntegerValue(42)}
	s.SetRow(idx, r)

	got := s.GetRow(idx)
	if len(got) != 2 || got[0] != expr.StringValue("hello") {
		t.Errorf("GetRow = %v, want [hello 42]", got)
	}

	// AllocRaw overflow
	for range 3 {
		if _, err := s.AllocRaw(); err != nil {
			t.Fatalf("unexpected AllocRaw error: %v", err)
		}
	}
	_, err = s.AllocRaw()
	if !errors.Is(err, exec.ErrSlabOverflow) {
		t.Errorf("expected ErrSlabOverflow after cap reached, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. ErrSlabOverflow sentinel
// ─────────────────────────────────────────────────────────────────────────────

func TestRowSlab_Overflow(t *testing.T) {
	s := exec.NewRowSlab(1, 2)
	if _, err := s.Alloc(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Alloc(); err != nil {
		t.Fatal(err)
	}
	_, err := s.Alloc()
	if !errors.Is(err, exec.ErrSlabOverflow) {
		t.Errorf("want ErrSlabOverflow, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. SlabPool — Get/Put round-trip
// ─────────────────────────────────────────────────────────────────────────────

func TestSlabPool_RoundTrip(t *testing.T) {
	pool := exec.NewSlabPool(4, 16)
	s := pool.Get()
	if s == nil {
		t.Fatal("Get returned nil")
	}
	row, err := s.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	row[0] = expr.IntegerValue(1)
	pool.Put(s)

	// After Put, the slab is reset. Get it again and verify it's clean.
	s2 := pool.Get()
	if s2.Len() != 0 {
		t.Errorf("slab from pool has Len=%d, want 0", s2.Len())
	}
	pool.Put(s2)
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. RowSlab race safety (concurrent Pools)
// ─────────────────────────────────────────────────────────────────────────────

func TestSlabPool_ConcurrentAccess(_ *testing.T) {
	pool := exec.NewSlabPool(2, 64)
	var wg sync.WaitGroup
	const goroutines = 32
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			s := pool.Get()
			for range 64 {
				row, err := s.Alloc()
				if errors.Is(err, exec.ErrSlabOverflow) {
					break
				}
				if err != nil {
					return
				}
				row[0] = expr.IntegerValue(1)
				row[1] = expr.StringValue("v")
			}
			pool.Put(s)
		}()
	}
	wg.Wait()
}

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
// 10. Benchmarks — RowSlab zero-alloc after warmup
// ─────────────────────────────────────────────────────────────────────────────

func BenchmarkRowSlab_Alloc(b *testing.B) {
	pool := exec.NewSlabPool(4, exec.DefaultSlabCapacity)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s := pool.Get()
		for {
			_, err := s.Alloc()
			if err != nil {
				break
			}
		}
		pool.Put(s)
	}
}

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
