package funcs_test

// list_funcs_test.go — tests for extended list built-ins (task-266).
//
// Covers: sort, extract stub, filter stub.
// NULL propagation and arity errors are verified.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ─────────────────────────────────────────────────────────────────────────────
// sort()
// ─────────────────────────────────────────────────────────────────────────────

func TestList_Sort_Integers(t *testing.T) {
	input := expr.ListValue{expr.IntegerValue(3), expr.IntegerValue(1), expr.IntegerValue(2)}
	v := mustCall(t, "sort", input)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("sort() got %v (%T), want list of 3", v, v)
	}
	for i, want := range []int64{1, 2, 3} {
		if lv[i] != expr.IntegerValue(want) {
			t.Errorf("[%d] got %v, want %d", i, lv[i], want)
		}
	}
}

func TestList_Sort_Strings(t *testing.T) {
	input := expr.ListValue{expr.StringValue("c"), expr.StringValue("a"), expr.StringValue("b")}
	v := mustCall(t, "sort", input)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("sort() got %v (%T), want list of 3", v, v)
	}
	for i, want := range []string{"a", "b", "c"} {
		if lv[i] != expr.StringValue(want) {
			t.Errorf("[%d] got %v, want %s", i, lv[i], want)
		}
	}
}

func TestList_Sort_Empty(t *testing.T) {
	v := mustCall(t, "sort", expr.ListValue{})
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 0 {
		t.Errorf("sort([]) = %v, want []", v)
	}
}

func TestList_Sort_Null(t *testing.T) {
	v := mustCall(t, "sort", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("sort(null) = %v, want null", v)
	}
}

func TestList_Sort_TypeError(t *testing.T) {
	_, err := call(t, "sort", expr.StringValue("oops"))
	if err == nil {
		t.Error("sort(string) should return TypeError")
	}
}

func TestList_Sort_ArityError(t *testing.T) {
	_, err := call(t, "sort")
	if err == nil {
		t.Error("sort() with no args should return ArityError")
	}
}

func TestList_Sort_DoesNotMutateOriginal(t *testing.T) {
	original := expr.ListValue{expr.IntegerValue(3), expr.IntegerValue(1), expr.IntegerValue(2)}
	_ = mustCall(t, "sort", original)
	// original must be unchanged
	if original[0] != expr.IntegerValue(3) {
		t.Error("sort() must not mutate the original list")
	}
}

func TestList_Sort_NullElements_SortLast(t *testing.T) {
	// NULLs should sort last per openCypher ordering.
	input := expr.ListValue{expr.IntegerValue(2), expr.Null, expr.IntegerValue(1)}
	v := mustCall(t, "sort", input)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 3 {
		t.Fatalf("sort() got %v (%T), want list of 3", v, v)
	}
	if lv[2] != expr.Null {
		t.Errorf("[2] got %v, want null (nulls last)", lv[2])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// extract() stub — receives pre-evaluated ListValue, passes through
// ─────────────────────────────────────────────────────────────────────────────

func TestList_Extract_Passthrough(t *testing.T) {
	input := expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}
	v := mustCall(t, "extract", input)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 2 {
		t.Errorf("extract() passthrough failed: %v", v)
	}
}

func TestList_Extract_Null(t *testing.T) {
	v := mustCall(t, "extract", expr.Null)
	if !expr.IsNull(v) {
		t.Errorf("extract(null) = %v, want null", v)
	}
}

func TestList_Extract_ArityError(t *testing.T) {
	_, err := call(t, "extract")
	if err == nil {
		t.Error("extract() with no args should return ArityError")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// filter() stub — receives pre-evaluated ListValue, passes through
// ─────────────────────────────────────────────────────────────────────────────

func TestList_Filter_Passthrough(t *testing.T) {
	input := expr.ListValue{expr.IntegerValue(4), expr.IntegerValue(5)}
	v := mustCall(t, "filter", input)
	lv, ok := v.(expr.ListValue)
	if !ok || len(lv) != 2 {
		t.Errorf("filter() passthrough failed: %v", v)
	}
}

func TestList_Filter_ArityError(t *testing.T) {
	_, err := call(t, "filter")
	if err == nil {
		t.Error("filter() with no args should return ArityError")
	}
}
