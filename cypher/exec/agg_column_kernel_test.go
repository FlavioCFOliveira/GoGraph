package exec_test

// agg_column_kernel_test.go — differential coverage for columnar (chunk-input)
// EagerAggregation (rmp #2104).
//
// Every test drives the SAME logical input through EagerAggregation twice — once
// column-major (WithChunkInput, the vectorized SoA kernels) and once row-at-a-time
// (the boxed funcs.Aggregator path) — and asserts the two outputs are BYTE-IDENTICAL
// (same concrete expr.Value type and value, floats compared by bit pattern). This is
// the reversibility contract (design §6): the columnar fast path must be equivalent
// by construction to the row path it replaces. The full openCypher TCK provides the
// semantic gate; these tests pin the ON-vs-OFF equivalence at the operator boundary,
// including the cases most at risk: exact large-integer SUM, mixed int/float columns,
// NULL-containing columns, cross-type grouping equivalence, and the buffering-
// aggregator delegation.

import (
	"context"
	"errors"
	"math"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// chunkRowSource emits a fixed list of boxed rows either row-at-a-time (Next) or
// column-major (FillChunk). The column-major path builds a dynamic chunk and appends
// each cell via PutValue, so a column's backing storage is decided exactly as the
// engine's pre-projection decides it: an int-only column commits to int64, a
// float-only column to float64, a mixed int/float column promotes to a boxed backing,
// and a NULL records validity — the four storage shapes the kernels must all handle.
type chunkRowSource struct {
	rows  [][]expr.Value
	ncols int
	idx   int
}

func (s *chunkRowSource) Init(_ context.Context) error { s.idx = 0; return nil }
func (s *chunkRowSource) Close() error                 { return nil }

func (s *chunkRowSource) Next(out *exec.Row) (bool, error) {
	if s.idx >= len(s.rows) {
		return false, nil
	}
	r := make(exec.Row, s.ncols)
	copy(r, s.rows[s.idx])
	s.idx++
	*out = r
	return true, nil
}

func (s *chunkRowSource) NewOutputChunk(capacity int) *exec.Chunk {
	return exec.NewDynamicChunk(capacity, s.ncols)
}

func (s *chunkRowSource) FillChunk(dst *exec.Chunk, maxRows int) (int, error) {
	n := 0
	for n < maxRows && s.idx < len(s.rows) {
		for c := 0; c < s.ncols; c++ {
			dst.PutValue(c, s.rows[s.idx][c])
		}
		s.idx++
		n++
	}
	return n, nil
}

// runAgg builds and drains an EagerAggregation over rows, column-major when useChunk
// is set (WithChunkInput) and row-at-a-time otherwise.
func runAgg(t *testing.T, rows [][]expr.Value, ncols int, keyCols []int, factories []funcs.AggregatorFactory, useChunk bool) ([]exec.Row, error) {
	t.Helper()
	src := &chunkRowSource{rows: rows, ncols: ncols}
	op, err := exec.NewEagerAggregation(src, keyCols, factories, 0)
	if err != nil {
		t.Fatalf("NewEagerAggregation: %v", err)
	}
	if useChunk {
		if werr := op.WithChunkInput(); werr != nil {
			t.Fatalf("WithChunkInput: %v", werr)
		}
	}
	return drainAgg(t, op)
}

func drainAgg(t *testing.T, op exec.Operator) ([]exec.Row, error) {
	t.Helper()
	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() {
		if err := op.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()
	var out []exec.Row
	for {
		var r exec.Row
		ok, err := op.Next(&r)
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, r)
	}
}

// valBytesEqual reports whether two values are byte-identical: same concrete
// expr.Value kind and value, with floats compared by their IEEE-754 bit pattern (so
// NaN and -0.0 are distinguished) and lists compared element-wise the same way.
func valBytesEqual(a, b expr.Value) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Kind() != b.Kind() {
		return false
	}
	switch av := a.(type) {
	case expr.IntegerValue:
		bv, ok := b.(expr.IntegerValue)
		return ok && av == bv
	case expr.FloatValue:
		bv, ok := b.(expr.FloatValue)
		return ok && math.Float64bits(float64(av)) == math.Float64bits(float64(bv))
	case expr.StringValue:
		bv, ok := b.(expr.StringValue)
		return ok && av == bv
	case expr.BoolValue:
		bv, ok := b.(expr.BoolValue)
		return ok && av == bv
	case expr.ListValue:
		bv, ok := b.(expr.ListValue)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !valBytesEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		// Null and any other kind: fall back to equivalence (Null ≡ Null etc).
		return expr.Equivalent(a, b)
	}
}

// rowKey builds a stable canonical string for a row so two result multisets can be
// sorted and compared position-independently.
func rowKey(r exec.Row) string {
	var b []byte
	for _, v := range r {
		if v == nil {
			b = append(b, "<nil>|"...)
			continue
		}
		b = append(b, v.Kind().String()...)
		b = append(b, ':')
		switch cv := v.(type) {
		case expr.FloatValue:
			// Bit pattern so -0.0/NaN/exact-mantissa differences are visible.
			var tmp [8]byte
			bits := math.Float64bits(float64(cv))
			for i := 0; i < 8; i++ {
				tmp[i] = byte(bits >> (8 * i))
			}
			b = append(b, tmp[:]...)
		default:
			b = append(b, v.String()...)
		}
		b = append(b, '|')
	}
	return string(b)
}

// assertSameMultiset fails the test unless want and got are the same multiset of
// byte-identical rows.
func assertSameMultiset(t *testing.T, name string, want, got []exec.Row) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: row count row-mode=%d columnar=%d", name, len(want), len(got))
	}
	sort.Slice(want, func(i, j int) bool { return rowKey(want[i]) < rowKey(want[j]) })
	sort.Slice(got, func(i, j int) bool { return rowKey(got[i]) < rowKey(got[j]) })
	for i := range want {
		if len(want[i]) != len(got[i]) {
			t.Fatalf("%s: row %d width row-mode=%d columnar=%d", name, i, len(want[i]), len(got[i]))
		}
		for c := range want[i] {
			if !valBytesEqual(want[i][c], got[i][c]) {
				t.Fatalf("%s: row %d col %d NOT byte-identical: row-mode=%#v columnar=%#v",
					name, i, c, want[i][c], got[i][c])
			}
		}
	}
}

// assertColumnarEqualsRow drives rows through both paths and asserts byte-identity.
func assertColumnarEqualsRow(t *testing.T, name string, rows [][]expr.Value, ncols int, keyCols []int, factories func() []funcs.AggregatorFactory) {
	t.Helper()
	rowOut, rowErr := runAgg(t, rows, ncols, keyCols, factories(), false)
	if rowErr != nil {
		t.Fatalf("%s: row-mode drain error: %v", name, rowErr)
	}
	colOut, colErr := runAgg(t, rows, ncols, keyCols, factories(), true)
	if colErr != nil {
		t.Fatalf("%s: columnar drain error: %v", name, colErr)
	}
	assertSameMultiset(t, name, rowOut, colOut)
}

// iv/fv/sv/nv are boxing shorthands for building test rows.
func iv(i int64) expr.Value   { return expr.IntegerValue(i) }
func fv(f float64) expr.Value { return expr.FloatValue(f) }
func sv(s string) expr.Value  { return expr.StringValue(s) }
func nv() expr.Value          { return expr.Null }

// TestColumnarAgg_Differential is the core ON-vs-OFF byte-identity matrix over
// COUNT(*), COUNT(expr), SUM, AVG, MIN, MAX with a group key, across int, float,
// mixed int/float, and NULL-containing argument columns.
func TestColumnarAgg_Differential(t *testing.T) {
	t.Parallel()

	// key (col 0) ∈ {"a","b"}; argument (col 1) varies by case.
	intRows := [][]expr.Value{
		{sv("a"), iv(1)}, {sv("b"), iv(10)}, {sv("a"), iv(2)},
		{sv("b"), iv(20)}, {sv("a"), iv(3)}, {sv("b"), iv(30)},
	}
	floatRows := [][]expr.Value{
		{sv("a"), fv(1.5)}, {sv("b"), fv(10.25)}, {sv("a"), fv(2.5)},
		{sv("b"), fv(20.5)}, {sv("a"), fv(0.125)},
	}
	mixedRows := [][]expr.Value{
		{sv("a"), iv(1)}, {sv("a"), fv(2.5)}, {sv("b"), iv(10)},
		{sv("b"), fv(0.5)}, {sv("a"), iv(3)}, {sv("b"), fv(1.25)},
	}
	nullRows := [][]expr.Value{
		{sv("a"), iv(1)}, {sv("a"), nv()}, {sv("b"), nv()},
		{sv("b"), iv(20)}, {sv("a"), iv(3)}, {sv("b"), nv()},
	}
	// A group ("c") whose every argument is NULL — SUM must be 0, AVG/MIN/MAX NULL,
	// COUNT(expr) 0, COUNT(*) the row count.
	allNullGroup := [][]expr.Value{
		{sv("c"), nv()}, {sv("c"), nv()}, {sv("a"), iv(5)},
	}

	datasets := []struct {
		name string
		rows [][]expr.Value
	}{
		{"int", intRows},
		{"float", floatRows},
		{"mixed", mixedRows},
		{"null", nullRows},
		{"allNullGroup", allNullGroup},
	}
	aggs := []struct {
		name    string
		factory func() []funcs.AggregatorFactory
	}{
		{"count_star", func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewCountStarAgg()} }},
		{"count_expr", func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewCountAgg()} }},
		{"sum", func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewSumAgg()} }},
		{"avg", func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewAvgAgg()} }},
		{"min", func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewMinAgg()} }},
		{"max", func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewMaxAgg()} }},
		{"all", func() []funcs.AggregatorFactory {
			return []funcs.AggregatorFactory{
				funcs.NewCountStarAgg(), funcs.NewCountAgg(), funcs.NewSumAgg(),
				funcs.NewAvgAgg(), funcs.NewMinAgg(), funcs.NewMaxAgg(),
			}
		}},
	}

	for _, ds := range datasets {
		for _, ag := range aggs {
			name := ds.name + "/" + ag.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				assertColumnarEqualsRow(t, name, ds.rows, 2, []int{0}, ag.factory)
			})
		}
	}
}

// TestColumnarAgg_LargeIntSumExact pins that an integer SUM whose exact value exceeds
// float64's 53-bit mantissa is computed exactly on the columnar path — never routed
// through a float64 accumulator — and is byte-identical to the row path.
func TestColumnarAgg_LargeIntSumExact(t *testing.T) {
	t.Parallel()

	// 2^53 + 1 + 1 + 1 = 9007199254740995. As float64 the running sum would round
	// (2^53 has no representable neighbour at distance 1), giving a wrong result.
	rows := [][]expr.Value{
		{sv("g"), iv(1 << 53)}, {sv("g"), iv(1)}, {sv("g"), iv(1)}, {sv("g"), iv(1)},
	}
	rowOut, err := runAgg(t, rows, 2, []int{0}, []funcs.AggregatorFactory{funcs.NewSumAgg()}, false)
	if err != nil {
		t.Fatalf("row-mode: %v", err)
	}
	colOut, err := runAgg(t, rows, 2, []int{0}, []funcs.AggregatorFactory{funcs.NewSumAgg()}, true)
	if err != nil {
		t.Fatalf("columnar: %v", err)
	}
	assertSameMultiset(t, "largeIntSum", rowOut, colOut)

	want := expr.IntegerValue((1 << 53) + 3)
	got := colOut[0][1]
	if iv, ok := got.(expr.IntegerValue); !ok || iv != want {
		t.Fatalf("large-int SUM: got %#v, want IntegerValue(%d) (exact, not float-rounded)", got, int64(want))
	}
}

// TestColumnarAgg_SumOverflowParity pins that an integer SUM overflow surfaces the
// SAME typed *expr.EvalError on the columnar path as on the row path.
func TestColumnarAgg_SumOverflowParity(t *testing.T) {
	t.Parallel()

	rows := [][]expr.Value{
		{sv("g"), iv(math.MaxInt64)}, {sv("g"), iv(1)},
	}
	_, rowErr := runAgg(t, rows, 2, []int{0}, []funcs.AggregatorFactory{funcs.NewSumAgg()}, false)
	_, colErr := runAgg(t, rows, 2, []int{0}, []funcs.AggregatorFactory{funcs.NewSumAgg()}, true)
	if rowErr == nil || colErr == nil {
		t.Fatalf("expected overflow errors: row=%v col=%v", rowErr, colErr)
	}
	if rowErr.Error() != colErr.Error() {
		t.Fatalf("overflow message mismatch:\n row=%q\n col=%q", rowErr.Error(), colErr.Error())
	}
	var ee *expr.EvalError
	if !errors.As(colErr, &ee) {
		t.Fatalf("columnar overflow error is %T, want *expr.EvalError", colErr)
	}
}

// TestColumnarAgg_IntFloatSeparateGroups pins the #2050 exact grouping equivalence on
// the columnar key path: int 2^53+1 and float 2^53.0 are DISTINCT group keys (the
// integer is not float64-representable), while int 2^53 and float 2^53.0 ARE the same
// group. The columnar and row group counts must agree for every ordering.
func TestColumnarAgg_IntFloatSeparateGroups(t *testing.T) {
	t.Parallel()

	const p53 = int64(1) << 53

	// {int 2^53+1, float 2^53.0}: 2 distinct groups.
	sep := [][]expr.Value{
		{iv(p53 + 1), iv(1)}, {fv(float64(p53)), iv(1)}, {iv(p53 + 1), iv(1)},
	}
	colOut, err := runAgg(t, sep, 2, []int{0}, []funcs.AggregatorFactory{funcs.NewCountStarAgg()}, true)
	if err != nil {
		t.Fatalf("columnar: %v", err)
	}
	if len(colOut) != 2 {
		t.Fatalf("int(2^53+1) vs float(2^53): got %d groups, want 2 (distinct)", len(colOut))
	}
	assertColumnarEqualsRow(t, "sepGroups", sep, 2, []int{0},
		func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewCountStarAgg()} })

	// {int 2^53, float 2^53.0}: 1 group (representable, equivalent).
	same := [][]expr.Value{
		{iv(p53), iv(1)}, {fv(float64(p53)), iv(1)},
	}
	sameOut, err := runAgg(t, same, 2, []int{0}, []funcs.AggregatorFactory{funcs.NewCountStarAgg()}, true)
	if err != nil {
		t.Fatalf("columnar: %v", err)
	}
	if len(sameOut) != 1 {
		t.Fatalf("int(2^53) vs float(2^53): got %d groups, want 1 (equivalent)", len(sameOut))
	}
}

// TestColumnarAgg_MinMaxStringAndMixed exercises the min/max boxed-fallback path: a
// string argument column (non-numeric → boxed comparison) and a heterogeneous
// int/string column (numeric best promoted to a boxed cross-kind best) must both be
// byte-identical to the row path.
func TestColumnarAgg_MinMaxStringAndMixed(t *testing.T) {
	t.Parallel()

	stringRows := [][]expr.Value{
		{sv("a"), sv("delta")}, {sv("a"), sv("alpha")}, {sv("b"), sv("zeta")},
		{sv("a"), sv("charlie")}, {sv("b"), sv("beta")},
	}
	// int + string in one column → dynamic column promotes to boxed; min/max compare
	// across kinds by kindOrder (numbers before strings).
	crossKindRows := [][]expr.Value{
		{sv("a"), iv(5)}, {sv("a"), sv("x")}, {sv("a"), iv(2)},
		{sv("b"), sv("y")}, {sv("b"), iv(100)},
	}
	for _, tc := range []struct {
		name string
		rows [][]expr.Value
	}{
		{"string", stringRows},
		{"crossKind", crossKindRows},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertColumnarEqualsRow(t, "min_"+tc.name, tc.rows, 2, []int{0},
				func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewMinAgg()} })
			assertColumnarEqualsRow(t, "max_"+tc.name, tc.rows, 2, []int{0},
				func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewMaxAgg()} })
		})
	}
}

// TestColumnarAgg_MinMaxFloatNaN pins NaN handling: openCypher sorts NaN last, so
// min ignores NaN unless every value is NaN, and max prefers NaN. The columnar
// numeric-best path must match the row path's expr.Compare exactly.
func TestColumnarAgg_MinMaxFloatNaN(t *testing.T) {
	t.Parallel()

	nan := math.NaN()
	rows := [][]expr.Value{
		{sv("a"), fv(nan)}, {sv("a"), fv(2.0)}, {sv("a"), fv(1.0)},
		{sv("b"), fv(nan)}, {sv("b"), fv(nan)}, // all-NaN group
	}
	assertColumnarEqualsRow(t, "min_nan", rows, 2, []int{0},
		func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewMinAgg()} })
	assertColumnarEqualsRow(t, "max_nan", rows, 2, []int{0},
		func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewMaxAgg()} })
}

// TestColumnarAgg_CrossBatchTypePromotion forces a SUM group to see integers in the
// first chunk and a float in the next (the run spans > DefaultChunkCapacity rows), so
// the per-group accumulator must promote from exact-int to float ACROSS batches
// exactly as funcs.SumAgg does — bit-identical to the row path.
func TestColumnarAgg_CrossBatchTypePromotion(t *testing.T) {
	t.Parallel()

	const n = exec.DefaultChunkCapacity + 100
	rows := make([][]expr.Value, 0, n)
	// Batch 1 is entirely integers for group "g"; the float lands in batch 2.
	for i := 0; i < n; i++ {
		if i == exec.DefaultChunkCapacity+10 {
			rows = append(rows, []expr.Value{sv("g"), fv(0.5)})
			continue
		}
		rows = append(rows, []expr.Value{sv("g"), iv(1)})
	}
	assertColumnarEqualsRow(t, "crossBatchSum", rows, 2, []int{0},
		func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewSumAgg()} })
	assertColumnarEqualsRow(t, "crossBatchAvg", rows, 2, []int{0},
		func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewAvgAgg()} })
}

// TestColumnarAgg_BufferingDelegates pins that the buffering aggregators (collect,
// percentileCont) have no vectorized form and delegate to the boxed funcs.Aggregator
// on the columnar path, producing byte-identical results to the row path.
func TestColumnarAgg_BufferingDelegates(t *testing.T) {
	t.Parallel()

	rows := [][]expr.Value{
		{sv("a"), iv(3)}, {sv("b"), iv(10)}, {sv("a"), iv(1)},
		{sv("b"), iv(20)}, {sv("a"), iv(2)}, {sv("a"), nv()},
	}
	assertColumnarEqualsRow(t, "collect", rows, 2, []int{0},
		func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewCollectAgg()} })
	assertColumnarEqualsRow(t, "percentileCont", rows, 2, []int{0},
		func() []funcs.AggregatorFactory { return []funcs.AggregatorFactory{funcs.NewPercentileContAgg(0.5)} })
	// A collect mixed with a vectorized SUM in one aggregation: the two kernels must
	// coexist (one boxed, one SoA) and both stay byte-identical.
	assertColumnarEqualsRow(t, "collect+sum", rows, 2, []int{0},
		func() []funcs.AggregatorFactory {
			return []funcs.AggregatorFactory{funcs.NewCollectAgg(), funcs.NewSumAgg()}
		})
}

// customAgg is a funcs.Aggregator unknown to buildAggKernels' type switch, used to
// pin that an unrecognised aggregator routes to the boxed fallback rather than being
// silently dropped.
type customAgg struct{ n int64 }

func (a *customAgg) Init() { a.n = 0 }
func (a *customAgg) Step(v expr.Value) error {
	if !expr.IsNull(v) {
		a.n++
	}
	return nil
}
func (a *customAgg) Result() expr.Value { return expr.IntegerValue(a.n * 100) }

// TestColumnarAgg_UnknownAggregatorDelegates pins that a funcs.Aggregator not in the
// kernel type switch falls back to the boxed path and stays byte-identical.
func TestColumnarAgg_UnknownAggregatorDelegates(t *testing.T) {
	t.Parallel()

	rows := [][]expr.Value{
		{sv("a"), iv(1)}, {sv("a"), nv()}, {sv("b"), iv(2)}, {sv("a"), iv(3)},
	}
	factory := func() []funcs.AggregatorFactory {
		return []funcs.AggregatorFactory{func() funcs.Aggregator { a := &customAgg{}; a.Init(); return a }}
	}
	assertColumnarEqualsRow(t, "customAgg", rows, 2, []int{0}, factory)
}
