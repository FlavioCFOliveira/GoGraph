package exec_test

// unwind_test.go — tests for the Unwind operator (task-457).
//
// The Unwind operator implements the openCypher UNWIND clause: for each
// input row it evaluates a list expression and emits one output row per
// list element, appending the element value as a new column. NULL and
// empty lists emit no rows. These tests cover the four scenarios named in
// the task spec (literal list, function result, property collection,
// empty list) plus the error and cancellation paths.
//
// # Cypher NULL vs Go nil
//
// UnwindListFn returns expr.ListValue (a typed slice). Its zero value is a
// Go-nil slice with len()==0; it CANNOT carry the openCypher NULL singleton
// (expr.Null) directly. The mapping from Cypher NULL to nil ListValue happens
// one level higher, inside the listFn that buildUnwindOperator wires up at
// cypher/api.go:2330 ('if v == expr.Null || v == nil { return nil, nil }').
// Tests in this file therefore exercise the operator-level behaviour for a
// Go-nil slice (which is what api.go hands the operator), not the api-level
// Cypher-NULL → nil mapping. The latter belongs in tests of buildUnwindOperator
// itself.

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
	rows      []exec.Row
	idx       int
	initErr   error
	nextErr   error
	closeErr  error
	closed    bool
	ctx       context.Context //nolint:containedctx // stored for per-Next ctx check, mirrors sliceOperator
	exhausted bool            // set once Next returned (false, _); guards against contract violations
}

// Init stores ctx for the per-Next cancellation check, resets per-cycle
// counters (idx, closed, exhausted) so the stub can be safely reused across
// multiple Init→Close cycles, and returns initErr. Pattern follows
// sliceOperator.Init (exec_test.go:35-39).
func (c *staticChildOp) Init(ctx context.Context) error {
	c.ctx = ctx
	c.idx = 0
	c.closed = false
	c.exhausted = false
	return c.initErr
}

// Next honours the Operator contract: it checks ctx.Done() at the top of every
// call before any other work, mirroring sliceOperator.Next (exec_test.go:42).
//
// After Next returns (false, _) for any reason — error, end-of-stream, or
// cancellation — any subsequent call panics. The Operator contract at
// operator.go:32-33 states "After returning (false, _), Next must not be
// called again"; a strict stub surfaces violations immediately instead of
// silently re-firing errors or end-of-stream markers, which would mask bugs
// in any future operator that retries Next.
func (c *staticChildOp) Next(out *exec.Row) (bool, error) {
	if c.exhausted {
		panic("staticChildOp: Next called after (false, _) — Operator contract violation")
	}
	if err := c.ctx.Err(); err != nil {
		c.exhausted = true
		return false, err
	}
	if c.nextErr != nil {
		c.exhausted = true
		return false, c.nextErr
	}
	if c.idx >= len(c.rows) {
		c.exhausted = true
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
			// Operator-level test: listFn returns the Go-nil ListValue.
			// api.go:2330 is the site that maps Cypher NULL → nil ListValue
			// before invoking the operator; here we assert the operator
			// itself treats a Go-nil slice the same as an empty list.
			name:  "Go-nil ListValue (what api.go hands the operator for Cypher NULL)",
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
		{
			// openCypher 9 §UNWIND: UNWIND [1, null, 2] AS x emits three rows;
			// the middle row binds x to NULL. The operator must NOT skip NULL
			// elements (that would conflate empty-list with null-element
			// semantics).
			name:  "UNWIND [1, null, 2] AS x — emits three rows including a NULL element",
			child: &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}},
			listFn: func(_ exec.Row) (expr.ListValue, error) {
				return litList(expr.IntegerValue(1), expr.Null, expr.IntegerValue(2)), nil
			},
			wantRows: [][]expr.Value{
				{expr.StringValue("ctx"), expr.IntegerValue(1)},
				{expr.StringValue("ctx"), expr.Null},
				{expr.StringValue("ctx"), expr.IntegerValue(2)},
			},
		},
		{
			// openCypher 9 §UNWIND: all-null list still emits one row per
			// element, every emitted column being NULL.
			name:  "UNWIND [null, null, null] AS x — emits one NULL row per element",
			child: &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}},
			listFn: func(_ exec.Row) (expr.ListValue, error) {
				return litList(expr.Null, expr.Null, expr.Null), nil
			},
			wantRows: [][]expr.Value{
				{expr.StringValue("ctx"), expr.Null},
				{expr.StringValue("ctx"), expr.Null},
				{expr.StringValue("ctx"), expr.Null},
			},
		},
		{
			// openCypher 9 §UNWIND: list with mixed element kinds plus NULL.
			// The element column may legitimately switch between types from
			// one row to the next — this is intentional under Cypher's
			// dynamic typing.
			name:  "UNWIND [42, 'x', null, true] AS y — mixed types and NULL preserved",
			child: &staticChildOp{rows: []exec.Row{{expr.StringValue("ctx")}}},
			listFn: func(_ exec.Row) (expr.ListValue, error) {
				return litList(expr.IntegerValue(42), expr.StringValue("x"), expr.Null, expr.BoolValue(true)), nil
			},
			wantRows: [][]expr.Value{
				{expr.StringValue("ctx"), expr.IntegerValue(42)},
				{expr.StringValue("ctx"), expr.StringValue("x")},
				{expr.StringValue("ctx"), expr.Null},
				{expr.StringValue("ctx"), expr.BoolValue(true)},
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

	if initErr := op.Init(context.Background()); !errors.Is(initErr, wantErr) {
		t.Fatalf("Init: got %v, want chain containing %v", initErr, wantErr)
	}

	// Even after a failed Init, callers must invoke Close once — that is what
	// Drain does on the failure path (driver.go:21). Verify Unwind.Close is
	// safe in this state and that it still reaches the child.
	if closeErr := op.Close(); closeErr != nil {
		t.Errorf("Close after failed Init returned err = %v, want nil", closeErr)
	}
	if !child.closed {
		t.Error("child.Close was not called by Unwind.Close after failed Init")
	}
}

// TestUnwind_CloseError verifies that a Close-only error from the child
// (no Next or listFn errors) is wrapped by Drain (driver.go:61-63) into the
// "exec: operator close: %w" envelope and is recoverable via errors.Is.
func TestUnwind_CloseError(t *testing.T) {
	wantErr := errors.New("synthetic close failure")
	child := &staticChildOp{
		rows:     []exec.Row{{expr.StringValue("ctx")}},
		closeErr: wantErr,
	}
	op, err := exec.NewUnwind(child, func(_ exec.Row) (expr.ListValue, error) {
		return litList(expr.IntegerValue(1)), nil
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}

	rows, drainErr := exec.Drain(context.Background(), op)
	if drainErr == nil {
		t.Fatal("expected close error from Drain, got nil")
	}
	if !errors.Is(drainErr, wantErr) {
		t.Errorf("Drain error chain does not contain closeErr: %v", drainErr)
	}
	// Drain emits the happy-path row before failing on Close.
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1", len(rows))
	}
	if !child.closed {
		t.Error("child.Close was not called")
	}
}

// TestUnwind_NextErrorPlusCloseError verifies Drain's dual-error wrap at
// driver.go:46 ("exec: operator next: %w; close: %w") preserves BOTH
// sentinels in the error chain.
func TestUnwind_NextErrorPlusCloseError(t *testing.T) {
	nextErr := errors.New("synthetic next failure")
	closeErr := errors.New("synthetic close failure")
	child := &staticChildOp{
		nextErr:  nextErr,
		closeErr: closeErr,
	}
	op, err := exec.NewUnwind(child, func(_ exec.Row) (expr.ListValue, error) {
		return litList(expr.IntegerValue(1)), nil
	})
	if err != nil {
		t.Fatalf("NewUnwind: %v", err)
	}

	_, drainErr := exec.Drain(context.Background(), op)
	if drainErr == nil {
		t.Fatal("expected combined error from Drain, got nil")
	}
	if !errors.Is(drainErr, nextErr) {
		t.Errorf("Drain error chain missing nextErr: %v", drainErr)
	}
	if !errors.Is(drainErr, closeErr) {
		t.Errorf("Drain error chain missing closeErr: %v", drainErr)
	}
	if !child.closed {
		t.Error("child.Close was not called after Next error")
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
	if _, drainErr := exec.Drain(ctx2, op2); !errors.Is(drainErr, context.Canceled) {
		t.Errorf("Drain error = %v, want chain containing context.Canceled", drainErr)
	}
	if !child2.closed {
		t.Error("child2.Close was not called by Drain on the pre-cancelled context branch")
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

// TestStaticChildOp_PanicAfterEndOfStream verifies that the strict stub
// panics when Next is called after a previous Next returned (false, _),
// surfacing any Operator contract violation immediately rather than letting
// the caller observe silent re-emission of EOS or an error.
func TestStaticChildOp_PanicAfterEndOfStream(t *testing.T) {
	cases := []struct {
		name  string
		child *staticChildOp
	}{
		{"end-of-stream", &staticChildOp{rows: nil}},
		{"after error", &staticChildOp{nextErr: errors.New("sentinel")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.child.Init(context.Background()); err != nil {
				t.Fatalf("Init: %v", err)
			}
			var row exec.Row
			// First call returns (false, _) and marks the stub exhausted.
			if ok, _ := tc.child.Next(&row); ok {
				t.Fatalf("expected first Next to return ok=false")
			}
			// Second call must panic.
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic on Next after (false, _), got none")
				}
			}()
			_, _ = tc.child.Next(&row)
		})
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

// TestValuesEqual covers the 3VL semantics of the helper: Null vs Null is
// equal, Null vs non-Null is unequal, lists containing Null elements are
// compared structurally.
func TestValuesEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b expr.Value
		want bool
	}{
		{"both Go-nil", nil, nil, true},
		{"a Go-nil only", nil, expr.IntegerValue(1), false},
		{"b Go-nil only", expr.IntegerValue(1), nil, false},
		{"Null vs Null", expr.Null, expr.Null, true},
		{"Null vs Integer", expr.Null, expr.IntegerValue(1), false},
		{"Integer vs Null", expr.IntegerValue(1), expr.Null, false},
		{"Integer eq", expr.IntegerValue(7), expr.IntegerValue(7), true},
		{"Integer neq", expr.IntegerValue(7), expr.IntegerValue(8), false},
		{"String eq", expr.StringValue("x"), expr.StringValue("x"), true},
		{"list[1,null,2] vs same", litList(expr.IntegerValue(1), expr.Null, expr.IntegerValue(2)),
			litList(expr.IntegerValue(1), expr.Null, expr.IntegerValue(2)), true},
		{"list[1,null,2] vs [1,null,3]", litList(expr.IntegerValue(1), expr.Null, expr.IntegerValue(2)),
			litList(expr.IntegerValue(1), expr.Null, expr.IntegerValue(3)), false},
		{"list-with-null vs different length", litList(expr.IntegerValue(1), expr.Null),
			litList(expr.IntegerValue(1), expr.Null, expr.Null), false},
		{"nested list with null", litList(litList(expr.IntegerValue(1), expr.Null)),
			litList(litList(expr.IntegerValue(1), expr.Null)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := valuesEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("valuesEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// valuesEqual compares two expr.Value instances for test assertions, with
// faithful Cypher three-valued-logic (3VL) handling:
//
//   - Two Go-nil interface values compare as equal; exactly one Go-nil compares
//     as unequal. This is a pre-3VL guard against nil-receiver panics inside
//     Equal.
//   - Two expr.Null values compare as equal — even though expr.Null.Equal
//     returns expr.Null per 3VL semantics, two NULL values ARE structurally
//     identical and must be considered equal for round-trip test assertions.
//   - A NULL vs non-NULL pair compares as unequal.
//   - Otherwise, Equal is invoked. If the result is BoolValue, its bool value
//     decides; if it is expr.Null (e.g., comparing two lists that contain a
//     NULL element), both sides are recursively walked element-by-element via
//     this same helper, returning true iff every position matches under the
//     same 3VL rules. This lets the test express "two equal-by-structure
//     values" assertions even when openCypher's predicate Equal would yield
//     NULL.
func valuesEqual(a, b expr.Value) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	aNull, bNull := expr.IsNull(a), expr.IsNull(b)
	if aNull && bNull {
		return true
	}
	if aNull != bNull {
		return false
	}
	res := a.Equal(b)
	if bv, ok := res.(expr.BoolValue); ok {
		return bool(bv)
	}
	// res is expr.Null — fall back to structural equality for the kinds that
	// can produce a NULL Equal result (lists, maps; both contain elements).
	la, aok := a.(expr.ListValue)
	lb, bok := b.(expr.ListValue)
	if aok && bok {
		if len(la) != len(lb) {
			return false
		}
		for i := range la {
			if !valuesEqual(la[i], lb[i]) {
				return false
			}
		}
		return true
	}
	return false
}
