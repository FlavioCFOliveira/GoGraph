package exec

// parallel_aggregate_scan_inline_internal_test.go — white-box gate for the
// budget==1 inline-serial short-circuit (#2115).
//
// This is a package-internal (white-box) test so it can read the operator's
// unexported `ranInline` diagnostic seam — the black-box determinism suite in
// parallel_aggregate_scan_test.go cannot. Its obligations are:
//
//   - the short-circuit is TAKEN when the governor budget is one worker (a single
//     morsel clamps [ParallelGovernor.Enter]'s budget to 1 regardless of
//     GOMAXPROCS);
//   - it is NOT taken when the budget exceeds one (per-node morsels with GOMAXPROCS
//     forced to 4);
//   - the inline result is BYTE-IDENTICAL to the multi-worker path — including the
//     load-bearing int/float tie representative and the group emission order — so
//     the short-circuit is a pure structural optimisation with zero behaviour
//     change;
//   - an inline error surfaces through Next like a worker error (Drain wraps it
//     "operator next:"), not through Init.

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// aggRowSlice is a minimal leaf operator streaming a fixed row slice — the
// per-morsel pre-aggregation sub-plan stand-in for these tests.
type aggRowSlice struct {
	rows []Row
	i    int
}

func (o *aggRowSlice) Init(context.Context) error { return nil }
func (o *aggRowSlice) Next(out *Row) (bool, error) {
	if o.i >= len(o.rows) {
		return false, nil
	}
	*out = o.rows[o.i]
	o.i++
	return true, nil
}
func (o *aggRowSlice) Close() error { return nil }

// inlineAggFactory returns an AggInputFactory that emits each morsel's rows in
// morsel order, keyed by NodeID.
func inlineAggFactory(rowsByID map[graph.NodeID]Row) AggInputFactory {
	return func(ids []graph.NodeID) (Operator, error) {
		rows := make([]Row, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, rowsByID[id])
		}
		return &aggRowSlice{rows: rows}, nil
	}
}

// renderAggResult renders each output value through String(), the byte-exact
// representation the determinism obligation compares.
func renderAggResult(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, v := range r {
			parts[j] = v.String()
		}
		out[i] = strings.Join(parts, "|")
	}
	return out
}

// driveInlineAgg builds a ParallelAggregateScan over ids [0,n) with the given
// morsel size under a forced GOMAXPROCS, drains it, and returns the rendered result
// together with whether the operator took the budget==1 inline path. A nil governor
// makes the budget GOMAXPROCS clamped to the morsel count, so morselSize alone
// controls which path is exercised: one morsel ⇒ budget 1 ⇒ inline.
func driveInlineAgg(t *testing.T, rowsByID map[graph.NodeID]Row, n, nKeys int, reducers []AggReducerKind, morselSize, gomax int) ([]string, bool) {
	t.Helper()
	prev := runtime.GOMAXPROCS(gomax)
	defer runtime.GOMAXPROCS(prev)
	op := NewParallelAggregateScan(idWalker{n: n}, inlineAggFactory(rowsByID), nKeys, reducers, morselSize, nil)
	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("drain (morsel=%d gomax=%d): %v", morselSize, gomax, err)
	}
	return renderAggResult(rows), op.ranInline
}

// TestParallelAggregateScan_InlineShortCircuit_GlobalMin proves the short-circuit
// is taken at budget 1 and reproduces the serial first-seen tie representative
// byte-for-byte, matching the multi-worker path. The extremum is a mixed int/float
// tie (int 2^53 vs float 2^53: Compare == 0 yet different String()), so a path that
// silently dropped the position-carrying combine would diverge the moment the two
// paths disagreed on which member to keep.
func TestParallelAggregateScan_InlineShortCircuit_GlobalMin(t *testing.T) {
	defer goleak.VerifyNone(t)

	big := int64(1) << 53
	bigF := expr.FloatValue(float64(big)) // first-seen at id 0 ⇒ the representative serial keeps
	bigI := expr.IntegerValue(big)        // ties bigF under Compare but renders "9007199254740992"
	if bigF.String() == bigI.String() {
		t.Fatalf("test not meaningful: tied members render identically (%q)", bigF.String())
	}
	vals := []expr.Value{bigF, expr.IntegerValue(big + 7), bigI, expr.FloatValue(float64(big) + 9), expr.IntegerValue(big + 3)}
	rowsByID := make(map[graph.NodeID]Row, len(vals))
	for i, v := range vals {
		rowsByID[graph.NodeID(i)] = Row{v}
	}
	reducers := []AggReducerKind{ReduceMin}

	// Single morsel ⇒ budget 1 ⇒ inline path (independent of GOMAXPROCS).
	inlineRows, inline := driveInlineAgg(t, rowsByID, len(vals), 0, reducers, 1<<20, 1)
	if !inline {
		t.Fatal("expected the budget==1 inline path with a single morsel, but ranInline was false")
	}
	// Per-node morsels with GOMAXPROCS forced to 4 ⇒ >1 worker ⇒ multi-worker path.
	mwRows, mwInline := driveInlineAgg(t, rowsByID, len(vals), 0, reducers, 1, 4)
	if mwInline {
		t.Fatal("expected the multi-worker path with per-node morsels and GOMAXPROCS=4, but ranInline was true")
	}

	assertRowsEqual(t, "inline vs multi-worker", inlineRows, mwRows)
	if len(inlineRows) != 1 || inlineRows[0] != bigF.String() {
		t.Fatalf("inline min representative = %v, want the first-seen float %q", inlineRows, bigF.String())
	}
}

// TestParallelAggregateScan_InlineShortCircuit_GroupBy proves the inline path is
// byte-identical to the multi-worker path for a GROUP BY aggregate, including the
// per-group tie representative and the ascending-first-seen emission order.
func TestParallelAggregateScan_InlineShortCircuit_GroupBy(t *testing.T) {
	defer goleak.VerifyNone(t)

	sv := func(s string) expr.Value { return expr.StringValue(s) }
	iv := func(n int64) expr.Value { return expr.IntegerValue(n) }
	fv := func(f float64) expr.Value { return expr.FloatValue(f) }
	// [key, arg]: two groups "a"/"b" interleaved (a first at id 0); each holds a
	// mixed int/float tie at its extremum so the representative is load-bearing.
	rows := []Row{
		{sv("a"), fv(1.0)}, {sv("b"), iv(20)}, {sv("a"), iv(1)},
		{sv("b"), fv(20.0)}, {sv("a"), iv(9)}, {sv("b"), iv(5)}, {sv("a"), iv(1)},
	}
	rowsByID := make(map[graph.NodeID]Row, len(rows))
	for i, r := range rows {
		rowsByID[graph.NodeID(i)] = r
	}
	reducers := []AggReducerKind{ReduceMin}

	inlineRows, inline := driveInlineAgg(t, rowsByID, len(rows), 1, reducers, 1<<20, 1)
	if !inline {
		t.Fatal("expected the budget==1 inline path with a single morsel, but ranInline was false")
	}
	mwRows, mwInline := driveInlineAgg(t, rowsByID, len(rows), 1, reducers, 1, 4)
	if mwInline {
		t.Fatal("expected the multi-worker path, but ranInline was true")
	}
	assertRowsEqual(t, "group-by inline vs multi-worker", inlineRows, mwRows)
	if len(inlineRows) != 2 {
		t.Fatalf("group-by inline emitted %d rows, want 2", len(inlineRows))
	}
}

// TestParallelAggregateScan_InlineShortCircuit_ErrorViaNext proves an inline-path
// failure surfaces through Next exactly like a worker error: Drain wraps it
// "operator next:" (not "operator init:") and the wrapped sentinel is preserved, so
// no caller can distinguish the inline path from the multi-worker path on error.
func TestParallelAggregateScan_InlineShortCircuit_ErrorViaNext(t *testing.T) {
	defer goleak.VerifyNone(t)

	// Two distinct grouping keys through one inline worker, capped at one group ⇒
	// the second key trips ErrAggMemoryExceeded inside the inline reduce.
	rowsByID := map[graph.NodeID]Row{
		0: {expr.IntegerValue(1), expr.IntegerValue(10)},
		1: {expr.IntegerValue(2), expr.IntegerValue(20)},
	}
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)
	op := NewParallelAggregateScan(idWalker{n: 2}, inlineAggFactory(rowsByID), 1,
		[]AggReducerKind{ReduceMin}, 1<<20, nil).WithGroupCap(1)

	// Init must NOT surface the error (the multi-worker path returns nil from Init).
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init returned %v; the inline error must surface through Next, not Init", err)
	}
	if !op.ranInline {
		t.Fatal("expected the inline path (single morsel), but ranInline was false")
	}
	var row Row
	_, err := op.Next(&row)
	if err == nil {
		t.Fatal("expected ErrAggMemoryExceeded from Next, got nil")
	}
	if !errors.Is(err, ErrAggMemoryExceeded) {
		t.Fatalf("Next error = %v, want it to wrap ErrAggMemoryExceeded", err)
	}
	if cerr := op.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	// End-to-end through Drain: the wrap must read "operator next:", the same as the
	// multi-worker worker-error path.
	op2 := NewParallelAggregateScan(idWalker{n: 2}, inlineAggFactory(rowsByID), 1,
		[]AggReducerKind{ReduceMin}, 1<<20, nil).WithGroupCap(1)
	_, derr := Drain(context.Background(), op2)
	if derr == nil {
		t.Fatal("Drain: expected an error, got nil")
	}
	if !errors.Is(derr, ErrAggMemoryExceeded) {
		t.Fatalf("Drain error = %v, want it to wrap ErrAggMemoryExceeded", derr)
	}
	if !strings.Contains(derr.Error(), "operator next:") {
		t.Fatalf("Drain error = %q, want it wrapped through Next (\"operator next:\")", derr.Error())
	}
}

// assertRowsEqual fails with a byte-level diff when two rendered results differ.
func assertRowsEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: row-count mismatch: got %d, want %d\n got=%v\n want=%v", what, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d BYTE-DIVERGENCE:\n got  = %q\n want = %q", what, i, got[i], want[i])
		}
	}
}
