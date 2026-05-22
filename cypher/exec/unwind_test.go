package exec_test

// unwind_test.go — tests for the Unwind operator (task-457).
//
// The Unwind operator implements the openCypher UNWIND clause: for each
// input row it evaluates a list expression and emits one output row per
// list element, appending the element value as a new column. NULL and
// empty lists emit no rows. These tests cover the four scenarios named in
// the task spec (literal list, function result, property collection,
// empty list) plus the error and cancellation paths.

import (
	"context"
	"errors"
	"testing"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// staticChildOp — a deterministic Operator stub.
//
// Returns a fixed sequence of rows from Next. Optional errors short-circuit
// Init / Next / Close so each branch in the Unwind operator can be exercised
// in isolation. Tracks whether Close was called so the test can assert
// resource hygiene.
// ─────────────────────────────────────────────────────────────────────────────

type staticChildOp struct {
	rows     []exec.Row
	idx      int
	initErr  error
	nextErr  error
	closeErr error
	closed   bool
	ctx      context.Context //nolint:containedctx // stored for per-Next ctx check, mirrors sliceOperator
}

// Init stores ctx for the per-Next cancellation check, resets per-cycle
// counters (idx, closed) so the stub can be safely reused across multiple
// Init→Close cycles, and returns initErr. Pattern follows sliceOperator.Init
// (exec_test.go:35-39).
func (c *staticChildOp) Init(ctx context.Context) error {
	c.ctx = ctx
	c.idx = 0
	c.closed = false
	return c.initErr
}

// Next honours the Operator contract: it checks ctx.Done() at the top of every
// call before any other work, mirroring sliceOperator.Next (exec_test.go:42).
func (c *staticChildOp) Next(out *exec.Row) (bool, error) {
	if err := c.ctx.Err(); err != nil {
		return false, err
	}
	if c.nextErr != nil {
		return false, c.nextErr
	}
	if c.idx >= len(c.rows) {
		return false, nil
	}
	*out = c.rows[c.idx]
	c.idx++
	return true, nil
}

func (c *staticChildOp) Close() error {
	c.closed = true
	return c.closeErr
}

// helper to build a literal list value.
func litList(vs ...expr.Value) expr.ListValue { return expr.ListValue(vs) }

// ─────────────────────────────────────────────────────────────────────────────
// 1. Table-driven happy-path tests
//
// Covers the four scenarios listed in the technical requirements:
//   - literal list                  → fixed list returned for every input row
//   - function result               → list computed from the input row
//   - property collection           → list extracted from a row column
//   - empty list                    → no rows emitted for that input row
// ─────────────────────────────────────────────────────────────────────────────

func TestUnwind_TableDriven(t *testing.T) {
	tests := []struct {
		name     string
		child    *staticChildOp
		listFn   exec.UnwindListFn
		wantRows [][]expr.Value
	}{
		{
			name: "literal list against single input row",
			// UNWIND [1, 2, 3] AS x
			child: &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}},
			listFn: func(_ exec.Row) (expr.ListValue, error) {
				return litList(expr.IntegerValue(1), expr.IntegerValue(2), expr.IntegerValue(3)), nil
			},
			wantRows: [][]expr.Value{
				{expr.StringValue("ctx"), expr.IntegerValue(1)},
				{expr.StringValue("ctx"), expr.IntegerValue(2)},
				{expr.StringValue("ctx"), expr.IntegerValue(3)},
			},
		},
		{
			name: "function result computed from input row (range-like)",
			// UNWIND range(1, row.n) AS x — emulated: list size derived from row[0].
			child: &staticChildOp{rows: []exec.Row{{expr.IntegerValue(4)}}},
			listFn: func(row exec.Row) (expr.ListValue, error) {
				n := int(row[0].(expr.IntegerValue))
				out := make(expr.ListValue, 0, n)
				for i := 1; i <= n; i++ {
					out = append(out, expr.IntegerValue(int64(i)))
				}
				return out, nil
			},
			wantRows: [][]expr.Value{
				{expr.IntegerValue(4), expr.IntegerValue(1)},
				{expr.IntegerValue(4), expr.IntegerValue(2)},
				{expr.IntegerValue(4), expr.IntegerValue(3)},
				{expr.IntegerValue(4), expr.IntegerValue(4)},
			},
		},
		{
			name: "property collection extracted from row column",
			// UNWIND n.tags AS t — emulated: column 1 already holds a ListValue.
			child: &staticChildOp{rows: []exec.Row{
				{expr.StringValue("alice"), litList(expr.StringValue("admin"), expr.StringValue("editor"))},
			}},
			listFn: func(row exec.Row) (expr.ListValue, error) {
				return row[1].(expr.ListValue), nil
			},
			wantRows: [][]expr.Value{
				{expr.StringValue("alice"), litList(expr.StringValue("admin"), expr.StringValue("editor")), expr.StringValue("admin")},
				{expr.StringValue("alice"), litList(expr.StringValue("admin"), expr.StringValue("editor")), expr.StringValue("editor")},
			},
		},
		{
			name: "empty list — no rows emitted for the input row",
			// UNWIND [] AS x
			child: &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}},
			listFn: func(_ exec.Row) (expr.ListValue, error) {
				return expr.ListValue{}, nil
			},
			wantRows: nil,
		},
		{
			name:  "nil list — treated the same as empty (openCypher NULL semantics)",
			child: &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}},
			listFn: func(_ exec.Row) (expr.ListValue, error) {
				return nil, nil
			},
			wantRows: nil,
		},
		{
			name:     "empty child stream — Unwind emits nothing",
			child:    &staticChildOp{rows: nil},
			listFn:   func(_ exec.Row) (expr.ListValue, error) { return litList(expr.IntegerValue(1)), nil },
			wantRows: nil,
		},
		{
			name: "multiple input rows, multi-element lists — full cartesian product",
			child: &staticChildOp{rows: []exec.Row{
				{expr.StringValue("a")},
				{expr.StringValue("b")},
			}},
			listFn: func(_ exec.Row) (expr.ListValue, error) {
				return litList(expr.IntegerValue(10), expr.IntegerValue(20)), nil
			},
			wantRows: [][]expr.Value{
				{expr.StringValue("a"), expr.IntegerValue(10)},
				{expr.StringValue("a"), expr.IntegerValue(20)},
				{expr.StringValue("b"), expr.IntegerValue(10)},
				{expr.StringValue("b"), expr.IntegerValue(20)},
			},
		},
		{
			name: "interleaved empty and non-empty lists across input rows",
			child: &staticChildOp{rows: []exec.Row{
				{expr.StringValue("keep1")},
				{expr.StringValue("skip")},
				{expr.StringValue("keep2")},
			}},
			listFn: func(row exec.Row) (expr.ListValue, error) {
				if row[0].(expr.StringValue) == "skip" {
					return expr.ListValue{}, nil
				}
				return litList(expr.IntegerValue(1)), nil
			},
			wantRows: [][]expr.Value{
				{expr.StringValue("keep1"), expr.IntegerValue(1)},
				{expr.StringValue("keep2"), expr.IntegerValue(1)},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op, err := exec.NewUnwind(tc.child, tc.listFn)
			if err != nil {
				t.Fatalf("NewUnwind: %v", err)
			}
			rows, err := exec.Drain(context.Background(), op)
			if err != nil {
				t.Fatalf("Drain: %v", err)
			}
			if !tc.child.closed {
				t.Error("child.Close was not called by the pipeline driver")
			}
			if len(rows) != len(tc.wantRows) {
				t.Fatalf("got %d rows, want %d (rows=%v)", len(rows), len(tc.wantRows), rows)
			}
			for i, got := range rows {
				want := tc.wantRows[i]
				if len(got) != len(want) {
					t.Fatalf("row %d: got width %d, want width %d", i, len(got), len(want))
				}
				for j := range want {
					if !valuesEqual(got[j], want[j]) {
						t.Errorf("row %d col %d: got %v, want %v", i, j, got[j], want[j])
					}
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Error paths
// ─────────────────────────────────────────────────────────────────────────────

func TestUnwind_ListFnError(t *testing.T) {
	wantErr := errors.New("synthetic listFn failure")
	child := &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}}
	op, err := exec.NewUnwind(child, func(_ exec.Row) (expr.ListValue, error) {
		return nil, wantErr
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}

	_, err = exec.Drain(context.Background(), op)
	if err == nil {
		t.Fatal("expected error from Drain, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain does not contain synthetic error: %v", err)
	}
	if !child.closed {
		t.Error("child.Close not called after Next error")
	}
}

func TestUnwind_ChildNextError(t *testing.T) {
	wantErr := errors.New("synthetic child Next failure")
	child := &staticChildOp{nextErr: wantErr}
	op, err := exec.NewUnwind(child, func(_ exec.Row) (expr.ListValue, error) {
		return litList(expr.IntegerValue(1)), nil
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}

	_, err = exec.Drain(context.Background(), op)
	if err == nil {
		t.Fatal("expected error from Drain, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain does not contain synthetic error: %v", err)
	}
	if !child.closed {
		t.Error("child.Close not called after Next error")
	}
}

func TestUnwind_ChildInitError(t *testing.T) {
	wantErr := errors.New("synthetic child Init failure")
	child := &staticChildOp{initErr: wantErr}
	op, err := exec.NewUnwind(child, func(_ exec.Row) (expr.ListValue, error) {
		return litList(expr.IntegerValue(1)), nil
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}

	if err := op.Init(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Init: got %v, want chain containing %v", err, wantErr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Cancellation — context check at the top of Next
// ─────────────────────────────────────────────────────────────────────────────

func TestUnwind_ContextCancellation(t *testing.T) {
	// Drain itself checks ctx.Err() before calling Next, so to exercise the
	// guard at the top of Unwind.Next we call Next directly. The flow:
	//   1. Init with a cancellable context.
	//   2. Cancel the context.
	//   3. Call Next — the internal guard must return (false, context.Canceled).
	long := make(expr.ListValue, 0, 1024)
	for i := range 1024 {
		long = append(long, expr.IntegerValue(int64(i)))
	}
	child := &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}}
	op, err := exec.NewUnwind(child, func(_ exec.Row) (expr.ListValue, error) {
		return long, nil
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cancel()

	var row exec.Row
	ok, nextErr := op.Next(&row)
	if ok {
		t.Error("expected Next to return ok=false after cancellation")
	}
	if !errors.Is(nextErr, context.Canceled) {
		t.Errorf("Next error = %v, want context.Canceled", nextErr)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Drain also surfaces cancellation — assert the documented contract.
	child2 := &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}}
	op2, err := exec.NewUnwind(child2, func(_ exec.Row) (expr.ListValue, error) {
		return long, nil
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := exec.Drain(ctx2, op2); !errors.Is(err, context.Canceled) {
		t.Errorf("Drain error = %v, want chain containing context.Canceled", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Close hygiene — Close must reach the child even when Next was never
// called and must be idempotent within a single pipeline lifecycle.
// ─────────────────────────────────────────────────────────────────────────────

func TestUnwind_CloseClosesChildEvenWithoutNext(t *testing.T) {
	child := &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}}
	op, err := exec.NewUnwind(child, func(_ exec.Row) (expr.ListValue, error) {
		return litList(expr.IntegerValue(1)), nil
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}

	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !child.closed {
		t.Error("child.Close was not called by Unwind.Close")
	}
}

// TestStaticChildOp_InitResetsState verifies the stub supports Init→…→Close
// reuse cycles by resetting idx and closed on every Init. Without this the
// second cycle would emit zero rows and falsely report Close had run.
func TestStaticChildOp_InitResetsState(t *testing.T) {
	child := &staticChildOp{rows: []exec.Row{{expr.IntegerValue(1)}, {expr.IntegerValue(2)}}}

	// Cycle 1: drive to completion via direct Operator API.
	if err := child.Init(context.Background()); err != nil {
		t.Fatalf("cycle1 Init: %v", err)
	}
	var r1, r2, rEnd exec.Row
	if ok, err := child.Next(&r1); !ok || err != nil {
		t.Fatalf("cycle1 Next1: ok=%v err=%v", ok, err)
	}
	if ok, err := child.Next(&r2); !ok || err != nil {
		t.Fatalf("cycle1 Next2: ok=%v err=%v", ok, err)
	}
	if ok, err := child.Next(&rEnd); ok || err != nil {
		t.Fatalf("cycle1 Next-end: ok=%v err=%v", ok, err)
	}
	if err := child.Close(); err != nil {
		t.Fatalf("cycle1 Close: %v", err)
	}
	if !child.closed {
		t.Fatal("cycle1: closed not set after Close")
	}

	// Cycle 2: re-Init the SAME stub and verify it serves the rows again.
	if err := child.Init(context.Background()); err != nil {
		t.Fatalf("cycle2 Init: %v", err)
	}
	if child.closed {
		t.Error("cycle2: Init did not reset closed to false")
	}
	if child.idx != 0 {
		t.Errorf("cycle2: Init did not reset idx, got %d want 0", child.idx)
	}
	var r3 exec.Row
	if ok, err := child.Next(&r3); !ok || err != nil {
		t.Fatalf("cycle2 Next1 (should re-emit first row): ok=%v err=%v", ok, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// valuesEqual compares two expr.Value instances. expr.Value supports an Equal
// method that returns a Cypher-trivalent BoolValue/NULL; for tests we expect
// non-NULL equality so this helper unwraps that.
func valuesEqual(a, b expr.Value) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	res := a.Equal(b)
	bv, ok := res.(expr.BoolValue)
	if !ok {
		return false
	}
	return bool(bv)
}
