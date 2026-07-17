package exec

import (
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// mustPanic asserts that fn panics; it fails the test otherwise.
func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%s: expected panic, got none", name)
		}
	}()
	fn()
}

// sameFloatBits compares two float64 by their IEEE-754 bit pattern, so NaN and
// signed zero are distinguished (unlike ==).
func sameFloatBits(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}

// ─────────────────────────────────────────────────────────────────────────────
// Construction and introspection
// ─────────────────────────────────────────────────────────────────────────────

func TestNewChunkDefaultsAndIntrospection(t *testing.T) {
	c := NewChunk(0, expr.KindInteger, expr.KindFloat, expr.KindString, expr.KindBool, expr.KindList)
	if got := c.Cap(); got != DefaultChunkCapacity {
		t.Errorf("Cap()=%d, want default %d", got, DefaultChunkCapacity)
	}
	if got := c.NumCols(); got != 5 {
		t.Errorf("NumCols()=%d, want 5", got)
	}
	if got := c.Len(); got != 0 {
		t.Errorf("empty chunk Len()=%d, want 0", got)
	}
	wantKinds := []expr.Kind{expr.KindInteger, expr.KindFloat, expr.KindString, expr.KindBool, expr.KindList}
	for j, want := range wantKinds {
		if got := c.ColKind(j); got != want {
			t.Errorf("ColKind(%d)=%s, want %s", j, got, want)
		}
	}

	// A scalar kind selects a typed backing; every other kind is boxed.
	wantStore := []storageTag{stI64, stF64, stStr, stBool, stBoxed}
	for j, want := range wantStore {
		if got := c.cols[j].store; got != want {
			t.Errorf("column %d store=%d, want %d", j, got, want)
		}
	}

	// An empty chunk of zero columns has length 0.
	empty := NewChunk(8)
	if empty.NumCols() != 0 || empty.Len() != 0 {
		t.Errorf("zero-column chunk: NumCols=%d Len=%d, want 0,0", empty.NumCols(), empty.Len())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Scalar round-trip and box-at-sink equivalence
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkInt64RoundTripAndBoxing(t *testing.T) {
	// Includes int64 boundaries and negative values.
	vals := []int64{0, 1, -1, 42, math.MinInt64, math.MaxInt64}
	c := NewChunk(len(vals), expr.KindInteger)
	for _, v := range vals {
		c.AppendInt64(0, v)
	}
	if c.Len() != len(vals) {
		t.Fatalf("Len()=%d, want %d", c.Len(), len(vals))
	}

	for row, want := range vals {
		// Cell accessor.
		got, valid := c.Int64(0, row)
		if !valid {
			t.Errorf("row %d: valid=false, want true", row)
		}
		if got != want {
			t.Errorf("row %d: Int64=%d, want %d", row, got, want)
		}
		// Box-at-sink must equal direct boxing (what lpgPropToExpr produces for a
		// PropInt64: expr.IntegerValue(i)).
		boxed := c.BoxCell(0, row)
		wantBoxed := expr.IntegerValue(want)
		iv, ok := boxed.(expr.IntegerValue)
		if !ok {
			t.Errorf("row %d: BoxCell kind=%s, want Integer", row, boxed.Kind())
		} else if iv != wantBoxed {
			t.Errorf("row %d: BoxCell=%d, want %d", row, int64(iv), int64(wantBoxed))
		}
	}

	// Vectorized accessor: contiguous, unboxed, allValid.
	data, valid, allValid := c.Int64Column(0)
	if !allValid {
		t.Errorf("Int64Column allValid=false, want true (no nulls appended)")
	}
	if valid != nil {
		t.Errorf("Int64Column valid=%v, want nil under allValid fast path", valid)
	}
	if len(data) != len(vals) {
		t.Fatalf("Int64Column len=%d, want %d", len(data), len(vals))
	}
	for i, want := range vals {
		if data[i] != want {
			t.Errorf("Int64Column[%d]=%d, want %d", i, data[i], want)
		}
	}
}

func TestChunkFloat64BitPatternRoundTrip(t *testing.T) {
	// NaN, +Inf, -Inf, -0.0, +0.0 must round-trip by bit pattern; a NaN cell is a
	// PRESENT float (valid), distinct from NULL.
	vals := []float64{
		0.0,
		math.Copysign(0, -1), // -0.0
		1.5,
		-2.25,
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		math.MaxFloat64,
		math.SmallestNonzeroFloat64,
	}
	c := NewChunk(len(vals), expr.KindFloat)
	for _, v := range vals {
		c.AppendFloat64(0, v)
	}

	for row, want := range vals {
		got, valid := c.Float64(0, row)
		if !valid {
			t.Errorf("row %d: valid=false, want true (present float, incl NaN)", row)
		}
		if !sameFloatBits(got, want) {
			t.Errorf("row %d: Float64 bits=%x, want %x", row, math.Float64bits(got), math.Float64bits(want))
		}
		boxed := c.BoxCell(0, row)
		fv, ok := boxed.(expr.FloatValue)
		if !ok {
			t.Errorf("row %d: BoxCell kind=%s, want Float", row, boxed.Kind())
			continue
		}
		if !sameFloatBits(float64(fv), want) {
			t.Errorf("row %d: BoxCell bits=%x, want %x", row, math.Float64bits(float64(fv)), math.Float64bits(want))
		}
	}
}

func TestChunkStringRoundTrip(t *testing.T) {
	// Empty and unicode strings; empty string is a PRESENT value, not NULL.
	vals := []string{"", "hello", "héllo wörld", "日本語", "\x00embedded-nul\x00"}
	c := NewChunk(len(vals), expr.KindString)
	for _, v := range vals {
		c.AppendString(0, v)
	}

	for row, want := range vals {
		got, valid := c.String(0, row)
		if !valid {
			t.Errorf("row %d: valid=false, want true", row)
		}
		if got != want {
			t.Errorf("row %d: String=%q, want %q", row, got, want)
		}
		boxed := c.BoxCell(0, row)
		sv, ok := boxed.(expr.StringValue)
		if !ok {
			t.Errorf("row %d: BoxCell kind=%s, want String", row, boxed.Kind())
		} else if string(sv) != want {
			t.Errorf("row %d: BoxCell=%q, want %q", row, string(sv), want)
		}
	}
}

func TestChunkBoolRoundTrip(t *testing.T) {
	vals := []bool{true, false, true, false}
	c := NewChunk(len(vals), expr.KindBool)
	for _, v := range vals {
		c.AppendBool(0, v)
	}
	for row, want := range vals {
		got, valid := c.Bool(0, row)
		if !valid {
			t.Errorf("row %d: valid=false, want true", row)
		}
		if got != want {
			t.Errorf("row %d: Bool=%v, want %v", row, got, want)
		}
		boxed := c.BoxCell(0, row)
		bv, ok := boxed.(expr.BoolValue)
		if !ok {
			t.Errorf("row %d: BoxCell kind=%s, want Bool", row, boxed.Kind())
		} else if bool(bv) != want {
			t.Errorf("row %d: BoxCell=%v, want %v", row, bool(bv), want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NULL handling — the validity bitmap is authoritative
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkNullVsPresentZeroInt(t *testing.T) {
	c := NewChunk(2, expr.KindInteger)
	c.AppendInt64(0, 0) // present zero
	c.AppendNull(0)     // null (placeholder also 0)

	// Present zero.
	if v, valid := c.Int64(0, 0); !valid || v != 0 {
		t.Errorf("row 0: got (%d,%v), want (0,true)", v, valid)
	}
	if b := c.BoxCell(0, 0); !boxedIs(b, expr.IntegerValue(0)) {
		t.Errorf("row 0: BoxCell=%v, want IntegerValue(0)", b)
	}
	// Null — distinct from present zero despite the same backing value.
	if !c.IsNull(0, 1) {
		t.Errorf("row 1: IsNull=false, want true")
	}
	if _, valid := c.Int64(0, 1); valid {
		t.Errorf("row 1: valid=true, want false")
	}
	if b := c.BoxCell(0, 1); !expr.IsNull(b) {
		t.Errorf("row 1: BoxCell=%v, want Null", b)
	}
}

func TestChunkNullVsEmptyString(t *testing.T) {
	c := NewChunk(2, expr.KindString)
	c.AppendString(0, "") // present empty
	c.AppendNull(0)       // null

	if v, valid := c.String(0, 0); !valid || v != "" {
		t.Errorf("row 0: got (%q,%v), want (\"\",true)", v, valid)
	}
	if b := c.BoxCell(0, 0); expr.IsNull(b) {
		t.Errorf("row 0: BoxCell is Null, want present empty StringValue")
	}
	if b := c.BoxCell(0, 1); !expr.IsNull(b) {
		t.Errorf("row 1: BoxCell=%v, want Null", b)
	}
}

func TestChunkNullVsPresentFalseBool(t *testing.T) {
	c := NewChunk(2, expr.KindBool)
	c.AppendBool(0, false) // present false
	c.AppendNull(0)        // null

	if v, valid := c.Bool(0, 0); !valid || v != false {
		t.Errorf("row 0: got (%v,%v), want (false,true)", v, valid)
	}
	if b := c.BoxCell(0, 0); expr.IsNull(b) {
		t.Errorf("row 0: BoxCell is Null, want present BoolValue(false)")
	}
	if b := c.BoxCell(0, 1); !expr.IsNull(b) {
		t.Errorf("row 1: BoxCell=%v, want Null", b)
	}
}

func TestChunkNaNPresentVsNull(t *testing.T) {
	c := NewChunk(2, expr.KindFloat)
	c.AppendFloat64(0, math.NaN()) // present NaN
	c.AppendNull(0)                // null

	if v, valid := c.Float64(0, 0); !valid || !math.IsNaN(v) {
		t.Errorf("row 0: got (%v,%v), want (NaN,true)", v, valid)
	}
	if b := c.BoxCell(0, 0); expr.IsNull(b) {
		t.Errorf("row 0: BoxCell is Null, want present FloatValue(NaN)")
	}
	if b := c.BoxCell(0, 1); !expr.IsNull(b) {
		t.Errorf("row 1: BoxCell=%v, want Null", b)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// allValid fast path and lazy bitmap materialization
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkAllValidFastPathAndMaterialization(t *testing.T) {
	c := NewChunk(8, expr.KindInteger)
	// Three valid appends keep the fast path: no bitmap allocated.
	c.AppendInt64(0, 10)
	c.AppendInt64(0, 20)
	c.AppendInt64(0, 30)
	col := &c.cols[0]
	if !col.allValid {
		t.Fatalf("after all-valid appends: allValid=false, want true")
	}
	if col.valid != nil {
		t.Fatalf("after all-valid appends: valid bitmap allocated (%v), want nil", col.valid)
	}

	// First null materializes the bitmap; the three prior rows must stay valid.
	c.AppendNull(0)
	if col.allValid {
		t.Fatalf("after a null: allValid=true, want false")
	}
	if col.valid == nil {
		t.Fatalf("after a null: valid bitmap nil, want materialized")
	}
	for row := 0; row < 3; row++ {
		if !c.IsValid(0, row) {
			t.Errorf("row %d: IsValid=false after materialization, want true", row)
		}
	}
	if c.IsValid(0, 3) {
		t.Errorf("row 3: IsValid=true, want false (the null)")
	}

	// A subsequent valid append after the null is recorded explicitly.
	c.AppendInt64(0, 50)
	if !c.IsValid(0, 4) {
		t.Errorf("row 4: IsValid=false, want true")
	}
	if v, valid := c.Int64(0, 4); !valid || v != 50 {
		t.Errorf("row 4: got (%d,%v), want (50,true)", v, valid)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Vectorized access with nulls, and BitSet
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkVectorizedWithNulls(t *testing.T) {
	// Span two bitmap words (>64 rows) to exercise word boundaries.
	const n = 130
	c := NewChunk(n, expr.KindInteger)
	isNull := func(i int) bool { return i%7 == 0 }
	for i := 0; i < n; i++ {
		if isNull(i) {
			c.AppendNull(0)
		} else {
			c.AppendInt64(0, int64(i*3))
		}
	}

	data, valid, allValid := c.Int64Column(0)
	if allValid {
		t.Fatalf("allValid=true, want false (nulls present)")
	}
	if len(data) != n {
		t.Fatalf("Int64Column len=%d, want %d", len(data), n)
	}
	for i := 0; i < n; i++ {
		gotValid := BitSet(valid, i)
		if gotValid == isNull(i) {
			t.Errorf("row %d: BitSet=%v, want %v", i, gotValid, !isNull(i))
		}
		if !isNull(i) && data[i] != int64(i*3) {
			t.Errorf("row %d: data=%d, want %d", i, data[i], i*3)
		}
	}

	// BitSet is out-of-range safe.
	if BitSet(valid, -1) || BitSet(valid, 1<<20) {
		t.Errorf("BitSet out-of-range should be false")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Capacity growth
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkCapacityGrowth(t *testing.T) {
	// Capacity is a hint; appending past it must still work (values and validity).
	c := NewChunk(2, expr.KindInteger)
	const n = 100
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			c.AppendInt64(0, int64(i))
		} else {
			c.AppendNull(0)
		}
	}
	if c.Len() != n {
		t.Fatalf("Len()=%d, want %d after growth", c.Len(), n)
	}
	for i := 0; i < n; i++ {
		v, valid := c.Int64(0, i)
		if i%2 == 0 {
			if !valid || v != int64(i) {
				t.Errorf("row %d: got (%d,%v), want (%d,true)", i, v, valid, i)
			}
		} else if valid {
			t.Errorf("row %d: valid=true, want false", i)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Positional set (overwrite)
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkPositionalSet(t *testing.T) {
	c := NewChunk(4, expr.KindInteger, expr.KindString)
	for i := 0; i < 4; i++ {
		c.AppendInt64(0, int64(i))
		c.AppendString(1, "orig")
	}

	// Overwrite a value.
	c.SetInt64(0, 2, 999)
	if v, valid := c.Int64(0, 2); !valid || v != 999 {
		t.Errorf("row 2 col 0: got (%d,%v), want (999,true)", v, valid)
	}

	// Overwrite with NULL, then back to a value — validity must track both ways.
	c.SetNull(0, 1)
	if !c.IsNull(0, 1) {
		t.Errorf("row 1 col 0: IsNull=false after SetNull, want true")
	}
	c.SetInt64(0, 1, 7)
	if v, valid := c.Int64(0, 1); !valid || v != 7 {
		t.Errorf("row 1 col 0: got (%d,%v), want (7,true) after re-set", v, valid)
	}

	// SetNull on a string releases the slot and boxes to Null.
	c.SetNull(1, 0)
	if b := c.BoxCell(1, 0); !expr.IsNull(b) {
		t.Errorf("row 0 col 1: BoxCell=%v, want Null", b)
	}
	if s := c.cols[1].str[0]; s != "" {
		t.Errorf("row 0 col 1: backing slot=%q, want cleared \"\"", s)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AppendValue / boxed columns
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkAppendValueTypedRouting(t *testing.T) {
	c := NewChunk(4, expr.KindInteger, expr.KindFloat, expr.KindString, expr.KindBool)
	c.AppendValue(0, expr.IntegerValue(11))
	c.AppendValue(1, expr.FloatValue(2.5))
	c.AppendValue(2, expr.StringValue("s"))
	c.AppendValue(3, expr.BoolValue(true))

	if v, _ := c.Int64(0, 0); v != 11 {
		t.Errorf("col 0: got %d, want 11", v)
	}
	if v, _ := c.Float64(1, 0); v != 2.5 {
		t.Errorf("col 1: got %v, want 2.5", v)
	}
	if v, _ := c.String(2, 0); v != "s" {
		t.Errorf("col 2: got %q, want \"s\"", v)
	}
	if v, _ := c.Bool(3, 0); v != true {
		t.Errorf("col 3: got %v, want true", v)
	}

	// A nil interface and expr.Null both append NULL.
	c.AppendValue(0, nil)
	c.AppendValue(0, expr.Null)
	if !c.IsNull(0, 1) || !c.IsNull(0, 2) {
		t.Errorf("AppendValue(nil)/AppendValue(Null) should record NULL")
	}
}

func TestChunkBoxedColumnRoundTrip(t *testing.T) {
	// A non-scalar kind is stored boxed and round-trips the exact Value; a boxed
	// column also honours the validity bitmap for NULL.
	c := NewChunk(3, expr.KindList)
	list := expr.ListValue{expr.IntegerValue(1), expr.StringValue("x")}
	c.AppendValue(0, list)
	c.AppendNull(0)
	c.AppendValue(0, expr.Null) // also NULL

	// Row 0: the stored list comes back by identity.
	got := c.BoxCell(0, 0)
	gl, ok := got.(expr.ListValue)
	if !ok || len(gl) != 2 {
		t.Fatalf("row 0: BoxCell=%v, want ListValue of len 2", got)
	}
	if iv, ok := gl[0].(expr.IntegerValue); !ok || iv != 1 {
		t.Errorf("row 0: list[0]=%v, want IntegerValue(1)", gl[0])
	}

	// Rows 1 and 2: NULL.
	for _, row := range []int{1, 2} {
		if !c.IsNull(0, row) {
			t.Errorf("row %d: IsNull=false, want true", row)
		}
		if b := c.BoxCell(0, row); !expr.IsNull(b) {
			t.Errorf("row %d: BoxCell=%v, want Null", row, b)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BoxRow — full-row materialization
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkBoxRow(t *testing.T) {
	c := NewChunk(2, expr.KindInteger, expr.KindString, expr.KindFloat)
	// Row 0: all present.
	c.AppendInt64(0, 5)
	c.AppendString(1, "a")
	c.AppendFloat64(2, 1.25)
	// Row 1: mixed nulls.
	c.AppendNull(0)
	c.AppendString(1, "b")
	c.AppendNull(2)

	var dst Row
	dst = c.BoxRow(0, dst)
	if len(dst) != 3 {
		t.Fatalf("BoxRow len=%d, want 3", len(dst))
	}
	if !boxedIs(dst[0], expr.IntegerValue(5)) {
		t.Errorf("row 0 col 0=%v, want IntegerValue(5)", dst[0])
	}
	if !boxedIs(dst[1], expr.StringValue("a")) {
		t.Errorf("row 0 col 1=%v, want StringValue(a)", dst[1])
	}
	if fv, ok := dst[2].(expr.FloatValue); !ok || float64(fv) != 1.25 {
		t.Errorf("row 0 col 2=%v, want FloatValue(1.25)", dst[2])
	}

	// Reuse dst for row 1; the returned slice should reuse the backing.
	before := &dst[0]
	dst = c.BoxRow(1, dst)
	if &dst[0] != before {
		t.Errorf("BoxRow did not reuse the dst backing")
	}
	if !expr.IsNull(dst[0]) {
		t.Errorf("row 1 col 0=%v, want Null", dst[0])
	}
	if !boxedIs(dst[1], expr.StringValue("b")) {
		t.Errorf("row 1 col 1=%v, want StringValue(b)", dst[1])
	}
	if !expr.IsNull(dst[2]) {
		t.Errorf("row 1 col 2=%v, want Null", dst[2])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Reset and pooling
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkResetClearsState(t *testing.T) {
	c := NewChunk(4, expr.KindInteger, expr.KindString, expr.KindList)
	c.AppendInt64(0, 1)
	c.AppendString(1, "keep-me")
	c.AppendValue(2, expr.ListValue{expr.IntegerValue(9)})
	c.AppendNull(0) // materialize a bitmap on col 0
	c.AppendString(1, "and-me")
	c.AppendValue(2, expr.Null)

	c.Reset()

	if c.Len() != 0 {
		t.Fatalf("after Reset: Len()=%d, want 0", c.Len())
	}
	// Fast path restored on every column; string/boxed slots released.
	for j := range c.cols {
		col := &c.cols[j]
		if !col.allValid {
			t.Errorf("col %d: allValid=false after Reset, want true", j)
		}
		if col.n != 0 {
			t.Errorf("col %d: n=%d after Reset, want 0", j, col.n)
		}
	}
	// The string backing must not retain header references past the used length.
	strCol := &c.cols[1]
	full := strCol.str[:cap(strCol.str)]
	for i := 0; i < 2; i++ {
		if full[i] != "" {
			t.Errorf("string backing[%d]=%q after Reset, want cleared", i, full[i])
		}
	}
	boxCol := &c.cols[2]
	fullBox := boxCol.boxed[:cap(boxCol.boxed)]
	for i := 0; i < 2; i++ {
		if fullBox[i] != nil {
			t.Errorf("boxed backing[%d]=%v after Reset, want nil", i, fullBox[i])
		}
	}

	// The chunk is fully reusable and correct after Reset.
	c.AppendInt64(0, 100)
	c.AppendString(1, "new")
	c.AppendValue(2, expr.IntegerValue(7))
	if v, valid := c.Int64(0, 0); !valid || v != 100 {
		t.Errorf("post-Reset reuse col 0: got (%d,%v), want (100,true)", v, valid)
	}
	// col 0 bitmap backing was retained but zeroed; the fresh valid append must
	// read as valid via the fast path.
	if !c.IsValid(0, 0) {
		t.Errorf("post-Reset reuse col 0: IsValid=false, want true")
	}
}

func TestChunkPoolRoundTrip(t *testing.T) {
	pool := NewChunkPool(4, expr.KindInteger, expr.KindString)

	c := pool.Get()
	if c.NumCols() != 2 || c.Cap() != 4 {
		t.Fatalf("pooled chunk: NumCols=%d Cap=%d, want 2,4", c.NumCols(), c.Cap())
	}
	c.AppendInt64(0, 1)
	c.AppendNull(0)
	c.AppendString(1, "x")
	c.AppendString(1, "y")
	pool.Put(c) // resets

	c2 := pool.Get()
	if c2.Len() != 0 {
		t.Errorf("chunk from pool has Len()=%d, want 0 (reset)", c2.Len())
	}
	if !c2.cols[0].allValid {
		t.Errorf("chunk from pool: col 0 allValid=false, want true (reset)")
	}
	// Reusable and correct.
	c2.AppendInt64(0, 42)
	if v, valid := c2.Int64(0, 0); !valid || v != 42 {
		t.Errorf("reused pool chunk: got (%d,%v), want (42,true)", v, valid)
	}
}

// The kinds slice passed to NewChunkPool must be copied, not aliased.
func TestChunkPoolCopiesKinds(t *testing.T) {
	kinds := []expr.Kind{expr.KindInteger, expr.KindString}
	pool := NewChunkPool(4, kinds...)
	kinds[0] = expr.KindFloat // mutate caller's slice after construction
	c := pool.Get()
	if c.ColKind(0) != expr.KindInteger {
		t.Errorf("pool aliased caller kinds: ColKind(0)=%s, want Integer", c.ColKind(0))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Programmer-error panics (misuse is a bug, surfaced immediately)
// ─────────────────────────────────────────────────────────────────────────────

func TestChunkKindMismatchPanics(t *testing.T) {
	c := NewChunk(4, expr.KindInteger)
	mustPanic(t, "AppendFloat64 on int column", func() { c.AppendFloat64(0, 1.0) })
	mustPanic(t, "AppendString on int column", func() { c.AppendString(0, "x") })
	mustPanic(t, "AppendBool on int column", func() { c.AppendBool(0, true) })
	mustPanic(t, "Float64 read on int column", func() { c.Float64(0, 0) })
	mustPanic(t, "Float64Column on int column", func() { c.Float64Column(0) })
	mustPanic(t, "AppendValue Float on int column", func() { c.AppendValue(0, expr.FloatValue(1)) })
}

func TestChunkOutOfRangeRowPanics(t *testing.T) {
	c := NewChunk(4, expr.KindInteger)
	c.AppendInt64(0, 1)
	mustPanic(t, "Int64 row beyond length", func() { c.Int64(0, 1) })
	mustPanic(t, "Int64 negative row", func() { c.Int64(0, -1) })
	mustPanic(t, "SetInt64 beyond length", func() { c.SetInt64(0, 5, 1) })
	mustPanic(t, "IsValid beyond length", func() { c.IsValid(0, 9) })
	mustPanic(t, "BoxCell beyond length", func() { c.BoxCell(0, 9) })
}

func TestChunkBoxRowRaggedPanics(t *testing.T) {
	c := NewChunk(4, expr.KindInteger, expr.KindString)
	c.AppendInt64(0, 1)
	c.AppendInt64(0, 2)
	c.AppendString(1, "only-one") // col 1 shorter than col 0 → ragged
	var dst Row
	mustPanic(t, "BoxRow on ragged chunk", func() { c.BoxRow(0, dst) })
}

// ─────────────────────────────────────────────────────────────────────────────
// Bitmap helpers (word-boundary correctness)
// ─────────────────────────────────────────────────────────────────────────────

func TestSetValidUpTo(t *testing.T) {
	cases := []int{0, 1, 63, 64, 65, 127, 128, 200}
	for _, count := range cases {
		words := (count + 63) >> 6
		bm := make([]uint64, words)
		setValidUpTo(bm, count)
		for i := 0; i < count; i++ {
			if !BitSet(bm, i) {
				t.Errorf("count=%d: bit %d not set, want set", count, i)
			}
		}
		// The bit at `count` (if addressable) must remain unset.
		if count < words*64 && BitSet(bm, count) {
			t.Errorf("count=%d: bit %d set, want unset", count, count)
		}
	}
}

// boxedIs reports whether got is a non-null value equal to want under Cypher
// equality. Used to assert box-at-sink produced the expected concrete value.
func boxedIs(got, want expr.Value) bool {
	if expr.IsNull(got) {
		return false
	}
	return expr.IsTruthy(got.Equal(want))
}

// TestAppendRowFrom copies rows (including NULLs, across all scalar kinds and a
// boxed column) from a source chunk into a same-schema destination and asserts the
// destination cells box back byte-identically — the compaction primitive the
// columnar filter relies on (#1704 P3).
func TestAppendRowFrom(t *testing.T) {
	kinds := []expr.Kind{expr.KindInteger, expr.KindFloat, expr.KindString, expr.KindBool, expr.KindList}
	src := NewChunk(8, kinds...)
	// Row 0: all present.
	src.AppendInt64(0, 256)
	src.AppendFloat64(1, 1.5)
	src.AppendString(2, "hi")
	src.AppendBool(3, true)
	src.AppendValue(4, expr.ListValue{expr.IntegerValue(1)})
	// Row 1: all NULL.
	for j := range kinds {
		src.AppendNull(j)
	}
	// Row 2: mixed present/null.
	src.AppendInt64(0, -7)
	src.AppendNull(1)
	src.AppendString(2, "")
	src.AppendNull(3)
	src.AppendValue(4, expr.ListValue{})

	dst := NewChunk(8, kinds...)
	for row := 0; row < 3; row++ {
		dst.AppendRowFrom(src, row)
	}
	if dst.Len() != 3 {
		t.Fatalf("dst.Len()=%d, want 3", dst.Len())
	}
	for row := 0; row < 3; row++ {
		for j := range kinds {
			got, want := dst.BoxCell(j, row), src.BoxCell(j, row)
			gN, wN := expr.IsNull(got), expr.IsNull(want)
			if gN != wN {
				t.Fatalf("cell (%d,%d): null mismatch got=%v want=%v", j, row, gN, wN)
			}
			if !wN && !expr.IsTruthy(got.Equal(want)) {
				t.Fatalf("cell (%d,%d): got=%#v want=%#v", j, row, got, want)
			}
		}
	}
}

// TestAppendRowFromColumnCountMismatch asserts AppendRowFrom rejects a schema
// mismatch loudly (programmer error), never producing a ragged chunk.
func TestAppendRowFromColumnCountMismatch(t *testing.T) {
	src := NewChunk(4, expr.KindInteger, expr.KindInteger)
	src.AppendInt64(0, 1)
	src.AppendInt64(1, 2)
	dst := NewChunk(4, expr.KindInteger)
	mustPanic(t, "AppendRowFrom column mismatch", func() { dst.AppendRowFrom(src, 0) })
}

// TestIsInt64Column reports the storage of static and dynamic columns so the
// columnar projection's chunk-input fast path can confirm a raw NodeID column.
func TestIsInt64Column(t *testing.T) {
	c := NewChunk(4, expr.KindInteger, expr.KindFloat)
	if !c.IsInt64Column(0) {
		t.Errorf("static integer column: IsInt64Column=false, want true")
	}
	if c.IsInt64Column(1) {
		t.Errorf("float column: IsInt64Column=true, want false")
	}
	d := NewDynamicChunk(4, 1)
	if d.IsInt64Column(0) {
		t.Errorf("uncommitted dynamic column: IsInt64Column=true, want false")
	}
	d.PutInt64(0, 5)
	if !d.IsInt64Column(0) {
		t.Errorf("dynamic column committed to int64: IsInt64Column=false, want true")
	}
}
