package exec

// chunk_dynamic_test.go — dynamic (Put-decided) column and promotion tests for
// the columnar late-materialisation projection (#1704 P2, #1823).

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

func TestNewDynamicChunkCommitsOnFirstPut(t *testing.T) {
	c := NewDynamicChunk(8, 4)
	if c.NumCols() != 4 {
		t.Fatalf("NumCols = %d, want 4", c.NumCols())
	}
	c.PutInt64(0, 42)
	c.PutFloat64(1, 3.5)
	c.PutString(2, "x")
	c.PutBool(3, true)
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	// Committed columns box back to the matching typed value.
	if got := c.BoxCell(0, 0); got != expr.IntegerValue(42) {
		t.Errorf("col0 = %#v, want IntegerValue(42)", got)
	}
	if got := c.BoxCell(1, 0); got != expr.FloatValue(3.5) {
		t.Errorf("col1 = %#v, want FloatValue(3.5)", got)
	}
	if got := c.BoxCell(2, 0); got != expr.StringValue("x") {
		t.Errorf("col2 = %#v, want StringValue(x)", got)
	}
	if got := c.BoxCell(3, 0); got != expr.BoolValue(true) {
		t.Errorf("col3 = %#v, want BoolValue(true)", got)
	}
	// A committed dynamic column reads back through the typed vectorized accessor.
	data, _, allValid := c.Int64Column(0)
	if len(data) != 1 || data[0] != 42 || !allValid {
		t.Errorf("Int64Column = %v allValid=%v, want [42] true", data, allValid)
	}
}

func TestDynamicChunkPromotesOnConflictingKind(t *testing.T) {
	c := NewDynamicChunk(8, 1)
	c.PutInt64(0, 5)
	c.PutInt64(0, 1000)  // stays int64, above the small-int cache
	c.PutFloat64(0, 2.5) // conflicting kind → promote whole column to boxed
	c.PutString(0, "s")  // already boxed
	if c.Len() != 4 {
		t.Fatalf("Len = %d, want 4", c.Len())
	}
	want := []expr.Value{
		expr.IntegerValue(5),
		expr.IntegerValue(1000),
		expr.FloatValue(2.5),
		expr.StringValue("s"),
	}
	for i, w := range want {
		if got := c.BoxCell(0, i); got != w {
			t.Errorf("row %d = %#v, want %#v", i, got, w)
		}
	}
}

func TestDynamicChunkPromotionPreservesNullsAndNaN(t *testing.T) {
	c := NewDynamicChunk(8, 1)
	c.PutFloat64(0, 1.5)
	c.PutNull(0)           // typed null on a committed float column
	c.PutString(0, "flip") // conflicting kind → promote to boxed (re-box incl. null)
	// Row 1 was NULL before promotion; it must box to expr.Null afterwards.
	if got := c.BoxCell(0, 0); got != expr.FloatValue(1.5) {
		t.Errorf("row0 = %#v, want FloatValue(1.5)", got)
	}
	if got := c.BoxCell(0, 1); !expr.IsNull(got) {
		t.Errorf("row1 = %#v, want Null after promotion", got)
	}
	if got := c.BoxCell(0, 2); got != expr.StringValue("flip") {
		t.Errorf("row2 = %#v, want StringValue(flip)", got)
	}
}

func TestDynamicChunkPutValueRoutesScalarsTyped(t *testing.T) {
	c := NewDynamicChunk(8, 1)
	// A boxed scalar arriving via PutValue must keep the column typed, not promote.
	c.PutInt64(0, 1)
	c.PutValue(0, expr.IntegerValue(2)) // same kind → stays typed
	if _, _, _ = c.Int64Column(0); c.cols[0].store != stI64 {
		t.Fatalf("column promoted to %d after same-kind PutValue; want stI64 (%d)", c.cols[0].store, stI64)
	}
	// A non-scalar via PutValue commits/promotes to boxed.
	c.PutValue(0, expr.ListValue{expr.IntegerValue(9)})
	if c.cols[0].store != stBoxed {
		t.Fatalf("column store = %d after list PutValue; want stBoxed (%d)", c.cols[0].store, stBoxed)
	}
	if got := c.BoxCell(0, 0); got != expr.IntegerValue(1) {
		t.Errorf("row0 = %#v, want IntegerValue(1)", got)
	}
	lv, ok := c.BoxCell(0, 2).(expr.ListValue)
	if !ok || len(lv) != 1 || lv[0] != expr.IntegerValue(9) {
		t.Errorf("row2 = %#v, want ListValue{9}", c.BoxCell(0, 2))
	}
}

func TestDynamicChunkNullFirstStaysBoxed(t *testing.T) {
	c := NewDynamicChunk(8, 1)
	c.PutNull(0) // first value NULL → commit to boxed
	c.PutInt64(0, 7)
	if c.cols[0].store != stBoxed {
		t.Fatalf("null-first column store = %d, want stBoxed (%d)", c.cols[0].store, stBoxed)
	}
	if got := c.BoxCell(0, 0); !expr.IsNull(got) {
		t.Errorf("row0 = %#v, want Null", got)
	}
	if got := c.BoxCell(0, 1); got != expr.IntegerValue(7) {
		t.Errorf("row1 = %#v, want IntegerValue(7)", got)
	}
}

func TestDynamicChunkPutValueNull(t *testing.T) {
	c := NewDynamicChunk(8, 1)
	c.PutInt64(0, 3)
	c.PutValue(0, nil)       // nil → NULL
	c.PutValue(0, expr.Null) // expr.Null → NULL
	if got := c.BoxCell(0, 1); !expr.IsNull(got) {
		t.Errorf("row1 = %#v, want Null", got)
	}
	if got := c.BoxCell(0, 2); !expr.IsNull(got) {
		t.Errorf("row2 = %#v, want Null", got)
	}
}

func TestDynamicChunkRowByteEstimate(t *testing.T) {
	c := NewDynamicChunk(8, 4)
	c.PutInt64(0, 1)                                    // fixed-width → overhead
	c.PutString(1, "abcd")                              // overhead + 4
	c.PutValue(2, expr.ListValue{expr.IntegerValue(1)}) // boxed → estimateBoxed
	c.PutNull(3)                                        // null → overhead
	const overhead = int64(16)
	estimateBoxed := func(v expr.Value) int64 {
		if _, ok := v.(expr.ListValue); ok {
			return 99
		}
		return overhead
	}
	got := c.RowByteEstimate(0, overhead, estimateBoxed)
	want := overhead + (overhead + 4) + 99 + overhead
	if got != want {
		t.Errorf("RowByteEstimate = %d, want %d", got, want)
	}
}

func TestDynamicChunkResetRestoresDynamic(t *testing.T) {
	c := NewDynamicChunk(8, 1)
	c.PutInt64(0, 1)
	c.Reset()
	if c.Len() != 0 {
		t.Fatalf("Len after Reset = %d, want 0", c.Len())
	}
	if c.cols[0].store != stDynamic {
		t.Fatalf("store after Reset = %d, want stDynamic (%d)", c.cols[0].store, stDynamic)
	}
	// Reusable after Reset: a different kind commits cleanly.
	c.PutString(0, "reused")
	if got := c.BoxCell(0, 0); got != expr.StringValue("reused") {
		t.Errorf("row0 after reuse = %#v, want StringValue(reused)", got)
	}
}
