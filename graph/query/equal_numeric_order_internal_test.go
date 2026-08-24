package query

// equal_numeric_order_internal_test.go — white-box regression cover for the
// unified numeric EQUALITY and for the re-decided index arms that serve it
// (task #2601).
//
// Four things are proven here that cannot be observed from outside the package:
//
//  1. equalValue implements ONE numeric relation across INTEGER and FLOAT, and
//     implements it EXACTLY — the pair the openCypher TCK uses to forbid a
//     float64 promotion stays distinct.
//  2. equalValue is the EQUALITY relation, not equivalence: NaN is not equal to
//     itself. That is the one place the three candidate relations disagree, and
//     it is asserted rather than left to a comment.
//  3. equalValue and valueInRange(v, e, e) agree on every ORDERABLE kind, which
//     is the asymmetry #2601 closed, and DISAGREE on the equatable-but-not-
//     orderable kinds, which is correct and must stay.
//  4. WHICH arm serves what. A string or bool equality is hash-served and
//     EXACT; a numeric equality is btree-served and RESIDUAL-FILTERED; a
//     numeric hash index is not consulted at all. Each is proven with a spy
//     index that deliberately claims an id the predicate excludes: the exact
//     arms must KEEP it (the scan never re-checked them), the residual arm must
//     DROP it, and the removed arms must never be read.

import (
	"math"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ----- the relation itself --------------------------------------------------

// TestEqualValue_UnifiedNumericEqualityIsExact drives equalValue directly over
// the cross-kind and large-integer pairs. The large-integer premise is asserted
// by TestCompareValues_LargeIntegerPremiseHolds in the companion file, which is
// what makes the exactness rows below discriminate a promoting implementation.
func TestEqualValue_UnifiedNumericEqualityIsExact(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b lpg.PropertyValue
		want bool
	}{
		// One numeric kind, both directions.
		{"int 5 == float 5", lpg.Int64Value(5), lpg.Float64Value(5), true},
		{"float 5 == int 5", lpg.Float64Value(5), lpg.Int64Value(5), true},
		{"int 5 != float 5.5", lpg.Int64Value(5), lpg.Float64Value(5.5), false},
		{"float 5.5 != int 5", lpg.Float64Value(5.5), lpg.Int64Value(5), false},
		{"int 5 == int 5", lpg.Int64Value(5), lpg.Int64Value(5), true},
		{"float 5.5 == float 5.5", lpg.Float64Value(5.5), lpg.Float64Value(5.5), true},

		// The TCK's own forbidden promotion: all three share one float64.
		{"2^62+1 != 2^62-4", lpg.Int64Value(tckBigIntA), lpg.Int64Value(tckBigIntB), false},
		{"2^62+1 != float 2^62", lpg.Int64Value(tckBigIntA), lpg.Float64Value(float64(twoTo62)), false},
		{"2^62-4 != float 2^62", lpg.Int64Value(tckBigIntB), lpg.Float64Value(float64(twoTo62)), false},
		{"2^62 == float 2^62", lpg.Int64Value(twoTo62), lpg.Float64Value(float64(twoTo62)), true},

		// The int64 extremes, where float64(i) lands on the wrong side of i.
		{"MaxInt64 != float 2^63", lpg.Int64Value(math.MaxInt64),
			lpg.Float64Value(float64TwoTo63), false},
		{"MinInt64 == float -2^63", lpg.Int64Value(math.MinInt64),
			lpg.Float64Value(-float64TwoTo63), true},

		// Infinities are ordered but equal to no integer.
		{"int 0 != +Inf", lpg.Int64Value(0), lpg.Float64Value(math.Inf(1)), false},
		{"int 0 != -Inf", lpg.Int64Value(0), lpg.Float64Value(math.Inf(-1)), false},
		{"+Inf == +Inf", lpg.Float64Value(math.Inf(1)), lpg.Float64Value(math.Inf(1)), true},

		// Signed zero: -0.0 == +0.0 == 0 under IEEE-754.
		{"int 0 == float -0.0", lpg.Int64Value(0), lpg.Float64Value(math.Copysign(0, -1)), true},
		{"float -0.0 == float 0.0", lpg.Float64Value(math.Copysign(0, -1)), lpg.Float64Value(0), true},

		// INTEGER x FLOAT is the ONLY off-diagonal pair openCypher unifies.
		{"int 5 != string \"5\"", lpg.Int64Value(5), lpg.StringValue("5"), false},
		{"string \"5\" != float 5", lpg.StringValue("5"), lpg.Float64Value(5), false},
		{"int 1 != bool true", lpg.Int64Value(1), lpg.BoolValue(true), false},
		{"bool true != int 1", lpg.BoolValue(true), lpg.Int64Value(1), false},

		// Within-kind equality for the kinds #2601 did not touch.
		{"string equal", lpg.StringValue("a"), lpg.StringValue("a"), true},
		{"string unequal", lpg.StringValue("a"), lpg.StringValue("b"), false},
		{"bool equal", lpg.BoolValue(true), lpg.BoolValue(true), true},
		{"bool unequal", lpg.BoolValue(true), lpg.BoolValue(false), false},
		{"bytes equal", lpg.BytesValue([]byte{1, 2}), lpg.BytesValue([]byte{1, 2}), true},
		{"bytes unequal length", lpg.BytesValue([]byte{1, 2}), lpg.BytesValue([]byte{1}), false},
		{"bytes unequal content", lpg.BytesValue([]byte{1, 2}), lpg.BytesValue([]byte{1, 3}), false},

		// A kind equalValue has never handled stays unhandled.
		{"list is not equatable here", lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1)}),
			lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1)}), false},
		{"zero-kind values", lpg.PropertyValue{}, lpg.PropertyValue{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := equalValue(tc.a, tc.b); got != tc.want {
				t.Fatalf("equalValue(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// Equality is SYMMETRIC. Asserting it here is what keeps the
			// cross-kind numeric arm from being written in one direction only.
			if got := equalValue(tc.b, tc.a); got != tc.want {
				t.Fatalf("equalValue is not symmetric: (%v, %v) = %v but the reverse = %v",
					tc.a, tc.b, tc.want, got)
			}
		})
	}
}

// TestEqualValue_NaNIsNeverEqual pins the NaN decision explicitly, because it is
// the single point at which the three relations that all unify INTEGER and
// FLOAT disagree:
//
//	equality       NaN = NaN is FALSE   <- what equalValue implements
//	comparability  NaN is unordered     <- valueInRange, same observable outcome
//	equivalence    NaN = NaN is TRUE    <- a value set's canonical key
//	                                       (cypher/exec/constraints.go
//	                                       floatCanonicalKey), NOT reused here
//
// Getting this wrong by reusing the canonical-key path would be invisible to
// every other test in this package, so it is asserted on its own.
func TestEqualValue_NaNIsNeverEqual(t *testing.T) {
	t.Parallel()

	nan := lpg.Float64Value(math.NaN())
	others := []lpg.PropertyValue{
		nan,
		lpg.Float64Value(0),
		lpg.Float64Value(math.Inf(1)),
		lpg.Float64Value(math.Inf(-1)),
		lpg.Int64Value(0),
		lpg.Int64Value(math.MaxInt64),
		lpg.Int64Value(math.MinInt64),
	}
	for _, o := range others {
		if equalValue(nan, o) {
			t.Errorf("equalValue(NaN, %v) reported EQUAL; under openCypher equality every "+
				"comparison against NaN is false", o)
		}
		if equalValue(o, nan) {
			t.Errorf("equalValue(%v, NaN) reported EQUAL", o)
		}
	}
	// The one that matters most, stated separately so a failure names it.
	if equalValue(nan, nan) {
		t.Fatal("equalValue(NaN, NaN) reported EQUAL: that is the EQUIVALENCE relation " +
			"(a value set folds NaN onto itself), not the EQUALITY relation this predicate " +
			"implements")
	}
}

// TestEqualValue_AgreesWithDegenerateRangeOnOrderableKinds is the definitional
// form of the asymmetry #2601 closed: for an ORDERABLE value, equalValue and
// valueInRange(v, e, e) are the same predicate.
//
// The second half is as load-bearing as the first. openCypher's equatability is
// WIDER than its comparability, so for BOOLEAN, BYTES and TIME the two must
// DISAGREE — equalValue matches, valueInRange cannot, because compareValues
// reports every pair over those kinds as unordered. Pinning the disagreement is
// what stops a later reader from "finishing" #2601 by widening compareValues,
// which would make WithRange match over unordered kinds.
func TestEqualValue_AgreesWithDegenerateRangeOnOrderableKinds(t *testing.T) {
	t.Parallel()

	orderable := []lpg.PropertyValue{
		lpg.Int64Value(0), lpg.Int64Value(5), lpg.Int64Value(-7),
		lpg.Int64Value(math.MaxInt64), lpg.Int64Value(math.MinInt64),
		lpg.Int64Value(twoTo62), lpg.Int64Value(tckBigIntA), lpg.Int64Value(tckBigIntB),
		lpg.Float64Value(0), lpg.Float64Value(5), lpg.Float64Value(5.5),
		lpg.Float64Value(math.Copysign(0, -1)),
		lpg.Float64Value(math.Inf(1)), lpg.Float64Value(math.Inf(-1)),
		lpg.Float64Value(math.NaN()),
		lpg.StringValue(""), lpg.StringValue("a"),
	}
	agreedTrue := 0
	for _, v := range orderable {
		for _, e := range orderable {
			eq := equalValue(v, e)
			rg := valueInRange(v, e, e)
			if eq != rg {
				t.Fatalf("equalValue(%v, %v) = %v but valueInRange(%v, %v, %v) = %v: an equality "+
					"and the degenerate range over the same ORDERABLE value must agree (#2601)",
					v, e, eq, v, e, e, rg)
			}
			if eq {
				agreedTrue++
			}
		}
	}
	if agreedTrue == 0 {
		t.Fatal("every pair agreed on FALSE; two predicates that both reject everything agree " +
			"vacuously, so the identity would prove nothing")
	}

	// The complementary half: equatable but NOT orderable, so the two relations
	// must part company.
	notOrderable := []lpg.PropertyValue{
		lpg.BoolValue(true),
		lpg.BoolValue(false),
		lpg.BytesValue([]byte{1, 2, 3}),
		lpg.TimeValue(time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)),
	}
	for _, v := range notOrderable {
		if !equalValue(v, v) {
			t.Errorf("equalValue(%v, %v) = false: openCypher makes this kind EQUATABLE", v, v)
		}
		if valueInRange(v, v, v) {
			t.Errorf("valueInRange(%v, %v, %v) = true: openCypher does not ORDER this kind, so "+
				"both bound tests are unordered and no range over it can hold", v, v, v)
		}
	}
}

// ----- spy plumbing ---------------------------------------------------------

// attachSpy registers sub as the ONLY index on g, on a fresh manager. Being the
// only one is what makes every "was it consulted" assertion below unambiguous.
func attachSpy(tb testing.TB, g *lpg.Graph[string, int64], name string, sub index.Subscriber) {
	tb.Helper()
	mgr := index.NewManager()
	if err := mgr.CreateIndex(name, sub); err != nil {
		tb.Fatalf("CreateIndex %s: %v", name, err)
	}
	g.SetIndexManager(mgr)
}

// ----- spy hash indexes for the arms #2601 re-decided ----------------------

// spyHashTyped is a hashLookuper[V] stand-in that records every typed read and
// returns the SAME posting list for every value, however wrong. Recording the
// reads is the point: for the arms #2601 removed the assertion is that the
// counters stay at ZERO, which no answer comparison alone could establish (a
// removed arm and a consulted-but-harmless arm produce the same answer on a
// well-behaved index).
type spyHashTyped[V comparable] struct {
	label, property string
	ids             []uint64
	reads           int
}

func (s *spyHashTyped[V]) Apply(index.Change) {}
func (s *spyHashTyped[V]) Kind() string       { return "hash" }
func (s *spyHashTyped[V]) BoundNode() (label, property string, ok bool) {
	return s.label, s.property, true
}

func (s *spyHashTyped[V]) Cardinality(V) uint64 {
	s.reads++
	return uint64(len(s.ids))
}

func (s *spyHashTyped[V]) LookupAppend(_ V, dst []uint64) []uint64 {
	s.reads++
	return append(dst, s.ids...)
}

func (s *spyHashTyped[V]) Lookup(V) *roaring64.Bitmap {
	s.reads++
	bm := roaring64.New()
	bm.AddMany(s.ids)
	return bm
}

// TestSeek_NumericHashIsNotConsulted pins the removal of the
// hashLookuper[int64] and hashLookuper[float64] arms (#2601).
//
// A single-kind hash index cannot hold both numeric kinds, so under a unified
// equality it is a SUBSET of the answer and a subset cannot be repaired by a
// residual filter. The spy claims BOTH ids — including the one whose property is
// not equal — so if the arm still existed and were trusted as exact, the surplus
// id would survive. The read counter additionally proves the index was never
// touched, which is the difference between "removed" and "consulted and lucky".
func TestSeek_NumericHashIsNotConsulted(t *testing.T) {
	t.Parallel()

	t.Run("int64-keyed", func(t *testing.T) {
		t.Parallel()
		g, c, idIn, idOut := buildSpyRangeGraph(t, lpg.Int64Value(5), lpg.Int64Value(9))
		spy := &spyHashTyped[int64]{label: "N", property: "v", ids: []uint64{idIn, idOut}}
		attachSpy(t, g, "n_v_hash_int", spy)

		got := New(g, c).Match().Vertex(
			WithLabel[string, int64]("N"),
			WithProperty[string, int64]("v", lpg.Int64Value(5)),
		).Collect()

		if spy.reads != 0 {
			t.Fatalf("the int64-keyed hash index was read %d time(s); the arm was removed in #2601 "+
				"because it cannot hold the float-valued nodes a unified equality must also match",
				spy.reads)
		}
		if len(got) != 1 || got[0] != "in" {
			t.Fatalf("got %v, want [in]: with no usable index the predicate must scan and be exact", got)
		}
	})

	t.Run("float64-keyed", func(t *testing.T) {
		t.Parallel()
		g, c, idIn, idOut := buildSpyRangeGraph(t, lpg.Float64Value(5), lpg.Float64Value(9))
		spy := &spyHashTyped[float64]{label: "N", property: "v", ids: []uint64{idIn, idOut}}
		attachSpy(t, g, "n_v_hash_f64", spy)

		got := New(g, c).Match().Vertex(
			WithLabel[string, int64]("N"),
			WithProperty[string, int64]("v", lpg.Float64Value(5)),
		).Collect()

		if spy.reads != 0 {
			t.Fatalf("the float64-keyed hash index was read %d time(s); the arm was removed in #2601 "+
				"because the typed read interface cannot tell a float-only projection (a subset) "+
				"from a unified numeric one (which over-returns above 2^53)", spy.reads)
		}
		if len(got) != 1 || got[0] != "in" {
			t.Fatalf("got %v, want [in]", got)
		}
	})
}

// TestSeek_StringAndBoolHashEqualityAreAuthoritative pins the arms that SURVIVED
// as exact, which is the other half of the same decision.
//
// The proof is the mirror image of the numeric one: the spy claims an id whose
// property is not equal and that id must SURVIVE, which can only happen if the
// scan never re-checked the index's answer. If a future change made every
// equality residual-filtered, the per-query property read would come back for
// string and bool equalities and this test would go red, making that a
// deliberate decision instead of a silent regression.
func TestSeek_StringAndBoolHashEqualityAreAuthoritative(t *testing.T) {
	t.Parallel()

	t.Run("string-keyed", func(t *testing.T) {
		t.Parallel()
		g, c, idIn, idOut := buildSpyRangeGraph(t, lpg.StringValue("b"), lpg.StringValue("z"))
		spy := &spyHashTyped[string]{label: "N", property: "v", ids: []uint64{idIn, idOut}}
		attachSpy(t, g, "n_v_hash_str", spy)

		got := New(g, c).Match().Vertex(
			WithLabel[string, int64]("N"),
			WithProperty[string, int64]("v", lpg.StringValue("b")),
		).Collect()

		if spy.reads == 0 {
			t.Fatal("the string hash index was never read, so this test says nothing about the arm")
		}
		if len(got) != 2 {
			t.Fatalf("got %v, want both ids: a string equality is EXACT, so the predicate is "+
				"discharged and the scan must not re-check the index's answer", got)
		}
	})

	t.Run("bool-keyed", func(t *testing.T) {
		t.Parallel()
		g, c, idIn, idOut := buildSpyRangeGraph(t, lpg.BoolValue(true), lpg.BoolValue(false))
		spy := &spyHashTyped[bool]{label: "N", property: "v", ids: []uint64{idIn, idOut}}
		attachSpy(t, g, "n_v_hash_bool", spy)

		got := New(g, c).Match().Vertex(
			WithLabel[string, int64]("N"),
			WithProperty[string, int64]("v", lpg.BoolValue(true)),
		).Collect()

		if spy.reads == 0 {
			t.Fatal("the bool hash index was never read, so this test says nothing about the arm")
		}
		if len(got) != 2 {
			t.Fatalf("got %v, want both ids: a bool equality is EXACT for the same reason a string "+
				"one is — equality is not unified across those kinds", got)
		}
	})
}

// TestSeek_NumericEqualityIsResidualFiltered proves the arm a numeric equality
// was MOVED to: the float64-keyed btree companion, seeked as the degenerate
// range [v, v] and NOT discharged.
//
// The spy claims an id whose property is not equal, and the per-node comparison
// must remove it. The seeked interval is pinned too, so a change to
// numericSeekBounds shows up here rather than only as a silent answer change.
func TestSeek_NumericEqualityIsResidualFiltered(t *testing.T) {
	t.Parallel()

	g, c, idIn, idOut := buildSpyRangeGraph(t, lpg.Int64Value(5), lpg.Int64Value(9))
	spy := &spyBTreeNumeric{label: "N", property: "v", ids: []uint64{idIn, idOut}}
	attachSpy(t, g, "n_v_btree_num", spy)

	got := New(g, c).Match().Vertex(
		WithLabel[string, int64]("N"),
		WithProperty[string, int64]("v", lpg.Float64Value(5)),
	).Collect()

	if spy.rangeCalls == 0 {
		t.Fatal("the numeric companion was never consulted, so a numeric equality is not being " +
			"index-served at all and this test says nothing about the seek path")
	}
	if len(got) != 1 || got[0] != "in" {
		t.Fatalf("got %v, want [in]: the index claimed both ids and the residual filter had to drop "+
			"the unequal one; getting both means the predicate was marked served", got)
	}
	if spy.lastLo != 5 || spy.lastHi != 5 {
		t.Fatalf("the equality seek was issued as [%v, %v], want the degenerate range [5, 5]",
			spy.lastLo, spy.lastHi)
	}
}

// TestSeek_NumericEqualityResidualIsExactAtTheBoundary is the same proof at the
// value trio the TCK uses to forbid a float64 promotion. All three ids share one
// float64 key, so the companion index CANNOT separate them however narrow the
// seek is — only an exact residual comparison can. This is the case that makes
// "superset plus residual" strictly stronger than any hash arm could be.
func TestSeek_NumericEqualityResidualIsExactAtTheBoundary(t *testing.T) {
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
	attachSpy(t, g, "n_v_btree_num", spy)

	eng := New(g, csr.BuildFromAdjList(g.AdjList()))
	for _, tc := range []struct {
		name     string
		expected lpg.PropertyValue
		want     string
	}{
		{"float at 2^62 matches only the exact integer",
			lpg.Float64Value(float64(twoTo62)), "exact"},
		{"int at 2^62+1", lpg.Int64Value(tckBigIntA), "above"},
		{"int at 2^62-4", lpg.Int64Value(tckBigIntB), "below"},
	} {
		got := eng.Match().Vertex(
			WithLabel[string, int64]("N"),
			WithProperty[string, int64]("v", tc.expected),
		).Collect()
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%s: got %v, want [%s]: %d, %d and %d are DISTINCT integers that share one "+
				"float64 key, so a promoting residual — or an index arm trusted as exact — returns "+
				"all three", tc.name, got, tc.want, tckBigIntA, tckBigIntB, twoTo62)
		}
	}
	if spy.rangeCalls == 0 {
		t.Fatal("the numeric companion was never consulted")
	}
}

// TestSeek_NaNEqualityNarrowsToEmpty pins the NaN branch of seekNumericEqInto.
// A NaN expected value is equal to nothing, so the seek reports itself as having
// served the predicate with an EMPTY working set rather than consulting the
// index — and, because it is still inexact, the residual then runs over nothing.
//
// The spy claims both ids, so an implementation that fell through to the scan
// with bm untouched would also return the empty set and this test could not tell
// the two apart. The rangeCalls counter is what separates them.
func TestSeek_NaNEqualityNarrowsToEmpty(t *testing.T) {
	t.Parallel()

	g, c, idIn, idOut := buildSpyRangeGraph(t, lpg.Float64Value(math.NaN()), lpg.Int64Value(9))
	spy := &spyBTreeNumeric{label: "N", property: "v", ids: []uint64{idIn, idOut}}
	attachSpy(t, g, "n_v_btree_num", spy)

	got := New(g, c).Match().Vertex(
		WithLabel[string, int64]("N"),
		WithProperty[string, int64]("v", lpg.Float64Value(math.NaN())),
	).Collect()

	if len(got) != 0 {
		t.Fatalf("got %v, want none: nothing is equal to NaN, the NaN-valued node included", got)
	}
	if spy.rangeCalls != 0 {
		t.Fatalf("the index was ranged %d time(s) for a NaN expected value; there is no interval to "+
			"seek, so the arm must clear the working set instead of asking the index", spy.rangeCalls)
	}
}

// TestSeekNumericEqInto_DeclinesWhatItCannotServe pins the two declines that
// keep the arm from being reached by a predicate it must not serve: a
// non-numeric expected value, and a btree whose key type is not float64. Both
// return false so the caller falls through to the exact scan.
func TestSeekNumericEqInto_DeclinesWhatItCannotServe(t *testing.T) {
	t.Parallel()

	// An unset PropertyValue carries kind 0, which isNumericKind rejects like any
	// other non-numeric kind. Naming it keeps the composite literal below from
	// needing an elided type that gofmt -s would rewrite to a bare {}.
	var zeroKind lpg.PropertyValue

	numeric := &spyBTreeNumeric{label: "N", property: "v", ids: []uint64{1, 2}}
	for _, v := range []lpg.PropertyValue{
		lpg.StringValue("5"), lpg.BoolValue(true), lpg.BytesValue([]byte{1}), zeroKind,
	} {
		bm := roaring64.New()
		bm.AddMany([]uint64{1, 2, 3})
		if seekNumericEqInto(bm, numeric, v) {
			t.Errorf("a %v expected value was served by the numeric arm", v.Kind())
		}
		if bm.GetCardinality() != 3 {
			t.Errorf("a declined seek modified the working set (cardinality %d, want 3)",
				bm.GetCardinality())
		}
	}
	if numeric.rangeCalls != 0 {
		t.Errorf("a declined seek still ranged the index %d time(s)", numeric.rangeCalls)
	}

	// An int64-keyed btree is a SUBSET of a unified numeric equality, exactly as
	// it is of a unified numeric range (#2600), so the assertion must not match
	// it and the index must never be read.
	intKeyed := &spyBTreeInt64{label: "N", property: "v", ids: []uint64{1, 2}}
	bm := roaring64.New()
	bm.AddMany([]uint64{1, 2, 3})
	if seekNumericEqInto(bm, intKeyed, lpg.Int64Value(5)) {
		t.Error("an int64-keyed btree served a numeric equality; it cannot hold the float-valued " +
			"nodes the equality must also match")
	}
	if intKeyed.rangeCalls != 0 {
		t.Errorf("the int64-keyed btree was ranged %d time(s)", intKeyed.rangeCalls)
	}
	if bm.GetCardinality() != 3 {
		t.Errorf("a declined seek modified the working set (cardinality %d, want 3)",
			bm.GetCardinality())
	}
}
