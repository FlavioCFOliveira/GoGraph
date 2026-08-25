package query

// range_numeric_order_internal_test.go — white-box regression cover for the
// unified numeric range comparison and the residual filter that makes the seek
// and scan arms agree (task #2600).
//
// The three things proven here cannot be observed from outside the package:
//
//  1. compareValues implements ONE numeric order across INTEGER and FLOAT, and
//     implements it EXACTLY — the pair the openCypher TCK uses to forbid a
//     float64 promotion is kept distinct.
//  2. numericSeekBound establishes the lof <= lo <= hi <= hif invariant that
//     numericSeekBounds' superset argument rests on, including at the int64
//     extremes where the conversion to float64 lands on the wrong side.
//  3. A NUMERIC range seek is residual-filtered while a STRING range seek is
//     authoritative. Both are proven with a spy index that deliberately claims
//     an id whose property is OUT of range: the numeric arm must drop it, the
//     string arm must keep it.

import (
	"math"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// The two int64 values the openCypher TCK uses to pin that a large-integer
// comparison must NOT be performed in float64: both round to 2^62, so any
// implementation that widens the integer reports them equal.
const (
	tckBigIntA int64 = 4611686018427387905 // 2^62 + 1
	tckBigIntB int64 = 4611686018427387900 // 2^62 - 4
	twoTo62    int64 = 4611686018427387904 // 2^62, exactly representable
)

// TestCompareValues_LargeIntegerPremiseHolds is the precondition for every
// exactness claim below: the two TCK values really do collapse onto the same
// float64, so a promoting comparison really would conflate them. Without this,
// the exactness assertions would pass for a promoting implementation too.
func TestCompareValues_LargeIntegerPremiseHolds(t *testing.T) {
	t.Parallel()
	if float64(tckBigIntA) != float64(twoTo62) || float64(tckBigIntB) != float64(twoTo62) {
		t.Fatalf("premise broken: float64(%d)=%v float64(%d)=%v float64(%d)=%v — the three values no "+
			"longer share a float64 key, so the exactness tests below no longer discriminate",
			tckBigIntA, float64(tckBigIntA), tckBigIntB, float64(tckBigIntB), twoTo62, float64(twoTo62))
	}
	if tckBigIntA == twoTo62 || tckBigIntB == twoTo62 {
		t.Fatal("premise broken: the TCK values must be DISTINCT integers from 2^62")
	}
}

func TestCompareValues_UnifiedNumericOrderIsExact(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		a, b   lpg.PropertyValue
		want   int
		wantOK bool
	}{
		// One numeric order across the two kinds (CIP2016-06-14; TCK
		// Comparison1.feature "1 = 1.0", Comparison2.feature "1 < 1.0").
		{"int==float", lpg.Int64Value(1), lpg.Float64Value(1.0), 0, true},
		{"float==int", lpg.Float64Value(1.0), lpg.Int64Value(1), 0, true},
		{"int<float", lpg.Int64Value(1), lpg.Float64Value(1.5), -1, true},
		{"float<int", lpg.Float64Value(0.5), lpg.Int64Value(1), -1, true},
		{"int>float", lpg.Int64Value(2), lpg.Float64Value(1.5), 1, true},
		{"float>int", lpg.Float64Value(2.5), lpg.Int64Value(2), 1, true},
		{"int==int", lpg.Int64Value(7), lpg.Int64Value(7), 0, true},
		{"float==float", lpg.Float64Value(7.25), lpg.Float64Value(7.25), 0, true},

		// EXACT above 2^53: the two TCK values must not be folded onto 2^62.
		{"bigA>2^62", lpg.Int64Value(tckBigIntA), lpg.Float64Value(float64(twoTo62)), 1, true},
		{"bigB<2^62", lpg.Int64Value(tckBigIntB), lpg.Float64Value(float64(twoTo62)), -1, true},
		{"2^62==2^62", lpg.Int64Value(twoTo62), lpg.Float64Value(float64(twoTo62)), 0, true},
		{"2^62<bigA", lpg.Float64Value(float64(twoTo62)), lpg.Int64Value(tckBigIntA), -1, true},
		{"2^62>bigB", lpg.Float64Value(float64(twoTo62)), lpg.Int64Value(tckBigIntB), 1, true},

		// int64 extremes against the float64 range guards.
		{"maxint<2^63", lpg.Int64Value(math.MaxInt64), lpg.Float64Value(float64TwoTo63), -1, true},
		{"minint==-2^63", lpg.Int64Value(math.MinInt64), lpg.Float64Value(-float64TwoTo63), 0, true},
		{"maxint<+Inf", lpg.Int64Value(math.MaxInt64), lpg.Float64Value(math.Inf(1)), -1, true},
		{"minint>-Inf", lpg.Int64Value(math.MinInt64), lpg.Float64Value(math.Inf(-1)), 1, true},

		// Fractional parts on both sides of zero.
		{"int0<0.5", lpg.Int64Value(0), lpg.Float64Value(0.5), -1, true},
		{"int0>-0.5", lpg.Int64Value(0), lpg.Float64Value(-0.5), 1, true},
		{"int-1<-0.5", lpg.Int64Value(-1), lpg.Float64Value(-0.5), -1, true},

		// NaN: not a position in the order, so every relation over it is false.
		{"int vs NaN", lpg.Int64Value(1), lpg.Float64Value(math.NaN()), 0, false},
		{"NaN vs int", lpg.Float64Value(math.NaN()), lpg.Int64Value(1), 0, false},
		{"NaN vs float", lpg.Float64Value(math.NaN()), lpg.Float64Value(1.0), 0, false},
		{"NaN vs NaN", lpg.Float64Value(math.NaN()), lpg.Float64Value(math.NaN()), 0, false},

		// Strings order among themselves and never against a number.
		{"str<str", lpg.StringValue("a"), lpg.StringValue("b"), -1, true},
		{"str vs int", lpg.StringValue("1"), lpg.Int64Value(1), 0, false},
		{"int vs str", lpg.Int64Value(1), lpg.StringValue("1"), 0, false},

		// The kinds openCypher does not order at all.
		{"bool vs bool", lpg.BoolValue(true), lpg.BoolValue(false), 0, false},
		{"bytes vs bytes", lpg.BytesValue([]byte{1}), lpg.BytesValue([]byte{2}), 0, false},
		{"list vs list", lpg.ListValue(nil), lpg.ListValue(nil), 0, false},
		{"int vs bool", lpg.Int64Value(1), lpg.BoolValue(true), 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := compareValues(tc.a, tc.b)
			if ok != tc.wantOK {
				t.Fatalf("compareValues ok=%v, want %v (result %d)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("compareValues = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNumericSeekBound_StaysOnTheCorrectSideOfTheBound asserts the invariant
// numericSeekBounds' superset argument rests on — lof <= lo and hi <= hif,
// compared EXACTLY — for every int64 the conversion to float64 cannot represent.
//
// It is the falsification of the one-ULP step in numericSeekBound: at
// math.MaxInt64 the conversion yields 2^63, which is strictly above the bound,
// so removing the step makes the lower-bound case here fail.
func TestNumericSeekBound_StaysOnTheCorrectSideOfTheBound(t *testing.T) {
	t.Parallel()

	values := []int64{
		0, 1, -1,
		1 << 52, 1<<53 - 1, 1 << 53, 1<<53 + 1,
		tckBigIntA, tckBigIntB, twoTo62,
		math.MaxInt64, math.MaxInt64 - 1, math.MinInt64, math.MinInt64 + 1,
		-(1 << 62), -(1<<62 + 1),
	}
	inexact := 0
	for _, i := range values {
		pv := lpg.Int64Value(i)

		lof, ok := numericSeekBound(pv, true)
		if !ok {
			t.Fatalf("i=%d: numericSeekBound(lower) reported not-ok for an int64 bound", i)
		}
		if c, cok := cmpInt64Float64(i, lof); !cok || c < 0 {
			t.Fatalf("i=%d: lower seek bound %v is ABOVE the bound (cmp=%d ok=%v); the seek would "+
				"exclude a true match", i, lof, c, cok)
		}

		hif, ok := numericSeekBound(pv, false)
		if !ok {
			t.Fatalf("i=%d: numericSeekBound(upper) reported not-ok for an int64 bound", i)
		}
		if c, cok := cmpInt64Float64(i, hif); !cok || c > 0 {
			t.Fatalf("i=%d: upper seek bound %v is BELOW the bound (cmp=%d ok=%v); the seek would "+
				"exclude a true match", i, hif, c, cok)
		}

		if c, _ := cmpInt64Float64(i, float64(i)); c != 0 {
			inexact++
		}
	}
	if inexact == 0 {
		t.Fatal("no value in the table converts inexactly to float64, so the one-ULP step was never " +
			"exercised and a clean result proves nothing")
	}
	t.Logf("%d of %d bounds converted inexactly to float64", inexact, len(values))
}

// TestNumericSeekBound_RejectsNaN pins that a NaN bound is reported
// unsatisfiable rather than seeked, which is what makes seekRangeInto clear the
// working set instead of scanning the whole index range.
func TestNumericSeekBound_RejectsNaN(t *testing.T) {
	t.Parallel()
	nan := lpg.Float64Value(math.NaN())
	if _, ok := numericSeekBound(nan, true); ok {
		t.Fatal("a NaN lower bound was accepted")
	}
	if _, ok := numericSeekBound(nan, false); ok {
		t.Fatal("a NaN upper bound was accepted")
	}
	if _, _, sat := numericSeekBounds(nan, lpg.Int64Value(1)); sat {
		t.Fatal("numericSeekBounds reported a NaN lower bound satisfiable")
	}
	if _, _, sat := numericSeekBounds(lpg.Int64Value(1), nan); sat {
		t.Fatal("numericSeekBounds reported a NaN upper bound satisfiable")
	}
}

// ----- spy btree indexes ----------------------------------------------------

// spyBTreeNumeric is a float64-keyed btree stand-in that returns the SAME
// posting list for every range, however narrow. That is not an approximation of
// the real defect shape — it is a caricature of it in the only direction the
// design permits: the engine's numeric companion over-returns (its int64 keys
// round above 2^53), and the residual filter is what removes the surplus.
type spyBTreeNumeric struct {
	label, property string
	ids             []uint64
	rangeCalls      int
	lastLo, lastHi  float64
}

func (s *spyBTreeNumeric) Apply(index.Change) {}
func (s *spyBTreeNumeric) Kind() string       { return "btree" }
func (s *spyBTreeNumeric) BoundNode() (label, property string, ok bool) {
	return s.label, s.property, true
}

func (s *spyBTreeNumeric) Range(lo, hi float64) *roaring64.Bitmap {
	s.rangeCalls++
	s.lastLo, s.lastHi = lo, hi
	bm := roaring64.New()
	bm.AddMany(s.ids)
	return bm
}

// spyBTreeString is the string-keyed counterpart, used to prove the string arm
// takes the OTHER branch: it is trusted as the answer and its surplus survives.
type spyBTreeString struct {
	label, property string
	ids             []uint64
	rangeCalls      int
	lastLo, lastHi  string
}

func (s *spyBTreeString) Apply(index.Change) {}
func (s *spyBTreeString) Kind() string       { return "btree" }
func (s *spyBTreeString) BoundNode() (label, property string, ok bool) {
	return s.label, s.property, true
}

func (s *spyBTreeString) Range(lo, hi string) *roaring64.Bitmap {
	s.rangeCalls++
	s.lastLo, s.lastHi = lo, hi
	bm := roaring64.New()
	bm.AddMany(s.ids)
	return bm
}

// buildSpyRangeGraph builds a two-node :N graph whose `v` property holds inV on
// the "in" node and outV on the "out" node, registers sub as the only index, and
// returns the query engine plus the two NodeIDs.
func buildSpyRangeGraph(
	tb testing.TB, inV, outV lpg.PropertyValue,
) (*lpg.Graph[string, int64], *csr.CSR[int64], uint64, uint64) {
	tb.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	for key, v := range map[string]lpg.PropertyValue{"in": inV, "out": outV} {
		if err := g.SetNodeLabel(key, "N"); err != nil {
			tb.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "v", v); err != nil {
			tb.Fatalf("SetNodeProperty %s: %v", key, err)
		}
	}
	idIn, _ := g.AdjList().Mapper().Lookup("in")
	idOut, _ := g.AdjList().Mapper().Lookup("out")
	return g, csr.BuildFromAdjList(g.AdjList()), uint64(idIn), uint64(idOut)
}

// TestSeek_NumericRangeIsResidualFiltered proves that a numeric range seek does
// NOT discharge the predicate: the spy claims an id whose property is out of
// range, and the per-node comparison must remove it.
//
// It also pins the widened bounds the seek was issued with, so a change to
// numericSeekBounds shows up here rather than only as a silent answer change.
func TestSeek_NumericRangeIsResidualFiltered(t *testing.T) {
	t.Parallel()

	g, c, idIn, idOut := buildSpyRangeGraph(t, lpg.Int64Value(5), lpg.Int64Value(9))
	spy := &spyBTreeNumeric{label: "N", property: "v", ids: []uint64{idIn, idOut}}
	mgr := index.NewManager()
	if err := mgr.CreateIndex("n_v_btree_num", spy); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	g.SetIndexManager(mgr)

	got := New(g, c).Match().Vertex(
		WithLabel[string, int64]("N"),
		WithRange[string, int64]("v", lpg.Float64Value(5), lpg.Float64Value(5)),
	).Collect()

	if spy.rangeCalls == 0 {
		t.Fatal("the numeric index was never consulted, so this test says nothing about the seek path")
	}
	if len(got) != 1 || got[0] != "in" {
		t.Fatalf("got %v, want [in]: the index claimed both ids and the residual filter had to drop "+
			"the out-of-range one; getting both means the predicate was marked served", got)
	}
	if spy.lastLo != 5 || spy.lastHi != 5 {
		t.Fatalf("the seek was issued with [%v, %v], want [5, 5]", spy.lastLo, spy.lastHi)
	}
}

// TestSeek_NumericRangeResidualFilterIsExactAtTheBoundary is the same proof at
// the value pair the TCK uses to forbid a float64 promotion: all three ids share
// one float64 key, so the index cannot separate them and only an exact residual
// comparison can.
func TestSeek_NumericRangeResidualFilterIsExactAtTheBoundary(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	values := map[string]int64{"exact": twoTo62, "above": tckBigIntA, "below": tckBigIntB}
	ids := make([]uint64, 0, len(values))
	for key, v := range values {
		if err := g.SetNodeLabel(key, "N"); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "v", lpg.Int64Value(v)); err != nil {
			t.Fatalf("SetNodeProperty %s: %v", key, err)
		}
		id, _ := g.AdjList().Mapper().Lookup(key)
		ids = append(ids, uint64(id))
	}
	spy := &spyBTreeNumeric{label: "N", property: "v", ids: ids}
	mgr := index.NewManager()
	if err := mgr.CreateIndex("n_v_btree_num", spy); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	g.SetIndexManager(mgr)

	got := New(g, csr.BuildFromAdjList(g.AdjList())).Match().Vertex(
		WithLabel[string, int64]("N"),
		WithRange[string, int64]("v",
			lpg.Float64Value(float64(twoTo62)), lpg.Float64Value(float64(twoTo62))),
	).Collect()

	if spy.rangeCalls == 0 {
		t.Fatal("the numeric index was never consulted")
	}
	if len(got) != 1 || got[0] != "exact" {
		t.Fatalf("got %v, want [exact]: %d and %d are DISTINCT from 2^62 even though all three round "+
			"to it, so a float64 promotion in the residual comparison would return all three",
			got, tckBigIntA, tckBigIntB)
	}
}

// TestSeek_StringRangeIsAuthoritative pins the OTHER branch of the same
// decision: a string range seek is exact, so the predicate IS discharged and the
// per-node comparison is skipped for it.
//
// The proof is deliberately the mirror image of the numeric one: the spy claims
// an id whose property is out of range and that id SURVIVES, which can only
// happen if the scan never re-checked it. This pins the string fast path
// (#1651): if a future change made every range predicate residual-filtered, the
// per-query property read would come back for string ranges and this test would
// go red, making that a deliberate decision instead of a silent regression.
func TestSeek_StringRangeIsAuthoritative(t *testing.T) {
	t.Parallel()

	g, c, idIn, idOut := buildSpyRangeGraph(t, lpg.StringValue("b"), lpg.StringValue("z"))
	spy := &spyBTreeString{label: "N", property: "v", ids: []uint64{idIn, idOut}}
	mgr := index.NewManager()
	if err := mgr.CreateIndex("n_v_btree", spy); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	g.SetIndexManager(mgr)

	got := New(g, c).Match().Vertex(
		WithLabel[string, int64]("N"),
		WithRange[string, int64]("v", lpg.StringValue("a"), lpg.StringValue("c")),
	).Collect()

	if spy.rangeCalls == 0 {
		t.Fatal("the string index was never consulted, so this test says nothing about the seek path")
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want both ids: a string range seek is EXACT, so the predicate is discharged "+
			"and the scan must not re-check the index's answer", got)
	}
	if spy.lastLo != "a" || spy.lastHi != "c" {
		t.Fatalf("the seek was issued with [%q, %q], want [\"a\", \"c\"]", spy.lastLo, spy.lastHi)
	}
}

// TestSeek_Int64KeyedBTreeIsNotConsulted pins the removal of the
// btreeRanger[int64] arm. An int64-keyed index cannot hold the float-valued
// nodes a unified numeric range must also match, so it is a SUBSET of the answer
// and a subset cannot be repaired by a residual filter (#2600).
//
// The spy claims an id the range excludes; if the arm still existed the id would
// be intersected in and — being discharged as exact — would survive.
func TestSeek_Int64KeyedBTreeIsNotConsulted(t *testing.T) {
	t.Parallel()

	g, c, idIn, idOut := buildSpyRangeGraph(t, lpg.Int64Value(5), lpg.Int64Value(9))
	spy := &spyBTreeInt64{label: "N", property: "v", ids: []uint64{idIn, idOut}}
	mgr := index.NewManager()
	if err := mgr.CreateIndex("n_v_btree_int", spy); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	g.SetIndexManager(mgr)

	got := New(g, c).Match().Vertex(
		WithLabel[string, int64]("N"),
		WithRange[string, int64]("v", lpg.Int64Value(5), lpg.Int64Value(5)),
	).Collect()

	if spy.rangeCalls != 0 {
		t.Fatalf("the int64-keyed index was consulted %d time(s); the arm was removed in #2600 because "+
			"it cannot be a superset of a unified numeric range", spy.rangeCalls)
	}
	if len(got) != 1 || got[0] != "in" {
		t.Fatalf("got %v, want [in]: with no usable index the predicate must scan and still be exact", got)
	}
}

// spyBTreeInt64 exists only to prove the int64-keyed range arm is gone.
type spyBTreeInt64 struct {
	label, property string
	ids             []uint64
	rangeCalls      int
}

func (s *spyBTreeInt64) Apply(index.Change) {}
func (s *spyBTreeInt64) Kind() string       { return "btree" }
func (s *spyBTreeInt64) BoundNode() (label, property string, ok bool) {
	return s.label, s.property, true
}

func (s *spyBTreeInt64) Range(lo, hi int64) *roaring64.Bitmap {
	s.rangeCalls++
	bm := roaring64.New()
	bm.AddMany(s.ids)
	return bm
}
