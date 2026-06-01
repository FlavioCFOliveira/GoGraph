package funcs_test

// range_bound_test.go — security regression tests for the range() element-count
// bound (rmp #1235, security finding H6).
//
// Before the fix, range() pre-computed an int capacity as
// (end-start)/step + 1, which could overflow or be astronomically large. A
// query such as range(1, 9223372036854775807) panicked with
// "makeslice: cap out of range"; a merely huge bound such as
// range(1, 5000000000) allocated tens of gigabytes and was OOM-killed. The
// fix computes the count overflow-safely in uint64 and rejects any range that
// would materialise more than the documented cap with a typed error.

import (
	"errors"
	"math"
	"testing"

	"gograph/cypher/expr"
)

// callRange is a convenience wrapper that never fatals on error, so the
// recover-free panic-detection tests can inspect the (value, error) pair.
func callRange(t *testing.T, args ...expr.Value) (expr.Value, error) {
	t.Helper()
	return call(t, "range", args...)
}

// TestFn_Range_MaxInt64_NoPanic asserts that the historical PoC trigger
// returns a typed error instead of panicking with makeslice or OOM-killing.
func TestFn_Range_MaxInt64_NoPanic(t *testing.T) {
	v, err := callRange(t, expr.IntegerValue(1), expr.IntegerValue(math.MaxInt64))
	if err == nil {
		t.Fatalf("range(1, MaxInt64) returned no error (value=%v); expected a typed out-of-range error", v)
	}
	var ee *expr.EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("range(1, MaxInt64) error type = %T (%v); want *expr.EvalError", err, err)
	}
}

// TestFn_Range_FullInt64Span exercises the int64-overflow corner: a span that
// does not fit in int64 (start=MinInt64, end=MaxInt64). The naive signed
// (end-start) wraps; the uint64 span computation must still report an
// over-cap count and return a typed error rather than panicking.
func TestFn_Range_FullInt64Span(t *testing.T) {
	v, err := callRange(t, expr.IntegerValue(math.MinInt64), expr.IntegerValue(math.MaxInt64))
	if err == nil {
		t.Fatalf("range(MinInt64, MaxInt64) returned no error (value=%v); expected a typed out-of-range error", v)
	}
	var ee *expr.EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("range(MinInt64, MaxInt64) error type = %T (%v); want *expr.EvalError", err, err)
	}
}

// TestFn_Range_LargeNegativeStep covers the negative-step over-cap path with a
// magnitude that does not trigger int64 loop wraparound: range(0, -200000000,
// -1) would produce 200_000_001 elements (> cap) and must be rejected with a
// typed error rather than allocating ~3 GB.
func TestFn_Range_LargeNegativeStep(t *testing.T) {
	v, err := callRange(t, expr.IntegerValue(0), expr.IntegerValue(-200_000_000), expr.IntegerValue(-1))
	if err == nil {
		t.Fatalf("range(0, -200000000, -1) returned no error (len=%d); expected a typed out-of-range error", listLen(v))
	}
	var ee *expr.EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("range(0, -200000000, -1) error type = %T (%v); want *expr.EvalError", err, err)
	}
}

// TestFn_Range_JustOverCap asserts that a range one element over the cap is
// rejected with a typed error.
func TestFn_Range_JustOverCap(t *testing.T) {
	// maxRangeElements is 100_000_000 (unexported); a count of cap+1 is
	// produced by range(0, cap) since the count is span/step + 1 = cap + 1.
	const cap = 100_000_000
	v, err := callRange(t, expr.IntegerValue(0), expr.IntegerValue(cap))
	if err == nil {
		t.Fatalf("range(0, %d) returned no error (len=%d); expected a typed out-of-range error", cap, listLen(v))
	}
	var ee *expr.EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("range(0, %d) error type = %T (%v); want *expr.EvalError", cap, err, err)
	}
}

// TestFn_Range_LargeUnderCap asserts that a large but under-cap range is
// produced correctly and is not rejected. One million elements is well below
// the 1e8 cap yet large enough to exercise the materialisation path; it stays
// within a few tens of MB so it is safe under -race and within the package
// time budget. Materialising the full cap (1e8) would allocate ~1.6 GB and is
// intentionally not asserted here.
func TestFn_Range_LargeUnderCap(t *testing.T) {
	const n = 1_000_000
	v := mustCall(t, "range", expr.IntegerValue(1), expr.IntegerValue(n))
	lv, ok := v.(expr.ListValue)
	if !ok {
		t.Fatalf("range(1, %d) = %T, want ListValue", n, v)
	}
	if len(lv) != n {
		t.Fatalf("range(1, %d) length = %d, want %d", n, len(lv), n)
	}
	if lv[0] != expr.IntegerValue(1) {
		t.Fatalf("range(1, %d)[0] = %v, want 1", n, lv[0])
	}
	if lv[n-1] != expr.IntegerValue(n) {
		t.Fatalf("range(1, %d)[last] = %v, want %d", n, lv[n-1], n)
	}
}

// TestFn_Range_SmallUnchanged guards against a regression in the common,
// small-bound case used by every TCK scenario.
func TestFn_Range_SmallUnchanged(t *testing.T) {
	tests := []struct {
		name string
		args []expr.Value
		want []int64
	}{
		{"ascending", []expr.Value{expr.IntegerValue(1), expr.IntegerValue(5)}, []int64{1, 2, 3, 4, 5}},
		{"step", []expr.Value{expr.IntegerValue(0), expr.IntegerValue(10), expr.IntegerValue(2)}, []int64{0, 2, 4, 6, 8, 10}},
		{"descending", []expr.Value{expr.IntegerValue(5), expr.IntegerValue(1), expr.IntegerValue(-1)}, []int64{5, 4, 3, 2, 1}},
		{"single", []expr.Value{expr.IntegerValue(3), expr.IntegerValue(3)}, []int64{3}},
		{"empty_inconsistent_dir", []expr.Value{expr.IntegerValue(5), expr.IntegerValue(1)}, []int64{}},
		{"negative_bounds", []expr.Value{expr.IntegerValue(-3), expr.IntegerValue(2)}, []int64{-3, -2, -1, 0, 1, 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := mustCall(t, "range", tc.args...)
			lv, ok := v.(expr.ListValue)
			if !ok {
				t.Fatalf("range(%v) = %T, want ListValue", tc.args, v)
			}
			if len(lv) != len(tc.want) {
				t.Fatalf("range(%v) length = %d, want %d", tc.args, len(lv), len(tc.want))
			}
			for i, w := range tc.want {
				if lv[i] != expr.IntegerValue(w) {
					t.Fatalf("range(%v)[%d] = %v, want %d", tc.args, i, lv[i], w)
				}
			}
		})
	}
}

// listLen returns the length of v if it is a ListValue, or -1 otherwise. Used
// only in error-path diagnostics.
func listLen(v expr.Value) int {
	if lv, ok := v.(expr.ListValue); ok {
		return len(lv)
	}
	return -1
}
