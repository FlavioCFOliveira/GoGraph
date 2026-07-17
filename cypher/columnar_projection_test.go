package cypher

// columnar_projection_test.go — byte-identity guard for the late-materialisation
// columnar scalar-property projection (#1704 P2, #1823).
//
// The columnar path (a terminal exec.ColumnarProject drained into a typed Chunk,
// boxed lazily at the sink) MUST produce, at the API boundary (Result.ValueAt /
// Result.Record), the byte-identical expr.Value the row-at-a-time path produces.
// These tests prove that by running the SAME data two ways over the SAME scan:
//
//   - columnar:  MATCH (n) RETURN n.val AS v   (all-property projection → columnar)
//   - row path:  MATCH (n) RETURN coalesce(n.val) AS v
//
// coalesce(x) returns x unchanged for a single argument (its value when non-null,
// NULL when null), so the two queries are value-equivalent; but the function call
// disqualifies the projection from the columnar shape, forcing the row-at-a-time
// path (evalRowPooled → lpgPropToExpr) — the exact pre-change behaviour. Any
// divergence between the two is a byte-identity defect. Both scans walk the same
// mapper order, so the results are compared element-wise.
//
// The tests also assert the columnar path was actually engaged (Result.matChunk
// non-nil) so a silent fallback cannot make the comparison vacuous.

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// valuesByteIdentical reports whether two boxed projection values are identical
// at the API boundary. NULLs (nil interface or expr.Null) compare equal to each
// other; floats compare by IEEE-754 bit pattern so NaN==NaN and ±0 are
// distinguished; every other kind (integer, string, bool, temporal, list, …)
// compares by deep structural equality — the values the two paths produce for a
// non-float are constructed by the same converter, so DeepEqual is exact.
func valuesByteIdentical(a, b expr.Value) bool {
	aNull := a == nil || expr.IsNull(a)
	bNull := b == nil || expr.IsNull(b)
	if aNull || bNull {
		return aNull && bNull
	}
	fa, aIsF := a.(expr.FloatValue)
	fb, bIsF := b.(expr.FloatValue)
	if aIsF || bIsF {
		return aIsF && bIsF && math.Float64bits(float64(fa)) == math.Float64bits(float64(fb))
	}
	return reflect.DeepEqual(a, b)
}

// drainScalarProjection runs query (which must project a single column aliased v)
// and returns the *Result plus the ordered per-row values read BOTH positionally
// (ValueAt(0)) and by name (Record()["v"]); it fails the test if the two
// accessors ever disagree for the same row.
func drainScalarProjection(t *testing.T, g *lpg.Graph[string, float64], query string) (*Result, []expr.Value) {
	t.Helper()
	eng := NewEngine(g)
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	var vals []expr.Value
	for res.Next() {
		byPos := res.ValueAt(0)
		byName, _ := res.Record()["v"].(expr.Value)
		if !valuesByteIdentical(byPos, byName) {
			t.Fatalf("ValueAt/Record disagree for %q: pos=%#v name=%#v", query, byPos, byName)
		}
		vals = append(vals, byPos)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	return res, vals
}

// singleValGraph builds a single-node graph whose one node carries property
// "val" = v, or no such property when set is false.
func singleValGraph(t *testing.T, v lpg.PropertyValue, set bool) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	if err := g.AddNode("n0"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if set {
		if err := g.SetNodeProperty("n0", "val", v); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	return g
}

func TestColumnarProjection_ByteIdentity_Scalars(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  lpg.PropertyValue
		// wantKind, when non-negative, asserts the columnar value's expr.Kind — the
		// guard that a temporal is not read back as a raw tagged string.
		wantKind expr.Kind
	}{
		{name: "int_zero", set: true, val: lpg.Int64Value(0), wantKind: expr.KindInteger},
		{name: "int_255_cache_boundary", set: true, val: lpg.Int64Value(255), wantKind: expr.KindInteger},
		{name: "int_256_above_cache", set: true, val: lpg.Int64Value(256), wantKind: expr.KindInteger},
		{name: "int_negative", set: true, val: lpg.Int64Value(-12345), wantKind: expr.KindInteger},
		{name: "int_min", set: true, val: lpg.Int64Value(math.MinInt64), wantKind: expr.KindInteger},
		{name: "int_max", set: true, val: lpg.Int64Value(math.MaxInt64), wantKind: expr.KindInteger},
		{name: "float_pi", set: true, val: lpg.Float64Value(3.14159), wantKind: expr.KindFloat},
		{name: "float_nan", set: true, val: lpg.Float64Value(math.NaN()), wantKind: expr.KindFloat},
		{name: "float_pos_inf", set: true, val: lpg.Float64Value(math.Inf(1)), wantKind: expr.KindFloat},
		{name: "float_neg_inf", set: true, val: lpg.Float64Value(math.Inf(-1)), wantKind: expr.KindFloat},
		{name: "float_neg_zero", set: true, val: lpg.Float64Value(math.Copysign(0, -1)), wantKind: expr.KindFloat},
		{name: "bool_true", set: true, val: lpg.BoolValue(true), wantKind: expr.KindBool},
		{name: "bool_false", set: true, val: lpg.BoolValue(false), wantKind: expr.KindBool},
		{name: "string_ascii", set: true, val: lpg.StringValue("hello"), wantKind: expr.KindString},
		{name: "string_empty", set: true, val: lpg.StringValue(""), wantKind: expr.KindString},
		{name: "string_unicode", set: true, val: lpg.StringValue("héllo wörld 日本語 😀"), wantKind: expr.KindString},
		// A plain string that begins with a temporal tag byte but is not a valid
		// temporal body must stay a String — the columnar classifier and
		// decodeTemporalString must agree it does not decode.
		{name: "string_tag_byte_not_temporal", set: true, val: lpg.StringValue("\x01not-a-date"), wantKind: expr.KindString},
		// Temporal: stored SOH-tagged; must decode to a Date, NOT a raw string.
		{name: "temporal_date", set: true, val: lpg.DateValue(time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)), wantKind: expr.KindDate},
		// List: kept boxed via the canonical converter.
		{name: "list", set: true, val: lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2), lpg.Int64Value(3)}), wantKind: expr.KindList},
		// Absent property → NULL.
		{name: "absent_null", set: false, val: lpg.PropertyValue{}, wantKind: expr.KindNull},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gCol := singleValGraph(t, tc.val, tc.set)
			gRow := singleValGraph(t, tc.val, tc.set)

			colRes, colVals := drainScalarProjection(t, gCol, "MATCH (n) RETURN n.val AS v")
			defer func() { _ = colRes.Close() }()
			rowRes, rowVals := drainScalarProjection(t, gRow, "MATCH (n) RETURN coalesce(n.val) AS v")
			defer func() { _ = rowRes.Close() }()

			// The columnar query must actually take the columnar path, and the
			// coalesce query must not, or the comparison proves nothing.
			if colRes.matChunk == nil {
				t.Fatalf("columnar query did not engage the columnar path (matChunk == nil)")
			}
			if rowRes.matChunk != nil {
				t.Fatalf("coalesce query unexpectedly took the columnar path")
			}

			if len(colVals) != 1 || len(rowVals) != 1 {
				t.Fatalf("row counts: columnar=%d row=%d, want 1 each", len(colVals), len(rowVals))
			}
			if !valuesByteIdentical(colVals[0], rowVals[0]) {
				t.Fatalf("byte-identity: columnar=%#v row-path=%#v", colVals[0], rowVals[0])
			}
			gotKind := expr.KindNull
			if colVals[0] != nil {
				gotKind = colVals[0].Kind()
			}
			if gotKind != tc.wantKind {
				t.Fatalf("columnar value Kind = %v, want %v (value=%#v)", gotKind, tc.wantKind, colVals[0])
			}
		})
	}
}

// TestColumnarProjection_ByteIdentity_Heterogeneous drives one column whose "val"
// property carries different kinds across nodes, exercising the dynamic column's
// commit-then-promote path (int → promote to boxed on the first float/string/
// temporal, NULL for absent). The columnar and row results must be element-wise
// byte-identical over the shared scan order.
func TestColumnarProjection_ByteIdentity_Heterogeneous(t *testing.T) {
	build := func() *lpg.Graph[string, float64] {
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		add := func(key string, v lpg.PropertyValue, set bool) {
			if err := g.AddNode(key); err != nil {
				t.Fatalf("AddNode(%s): %v", key, err)
			}
			if set {
				if err := g.SetNodeProperty(key, "val", v); err != nil {
					t.Fatalf("SetNodeProperty(%s): %v", key, err)
				}
			}
		}
		add("a", lpg.Int64Value(1), true)                                          // commits column to int64
		add("b", lpg.Int64Value(1000), true)                                       // int64, above small-int cache
		add("c", lpg.Float64Value(2.5), true)                                      // promotes column to boxed
		add("d", lpg.StringValue("x"), true)                                       // boxed string
		add("e", lpg.BoolValue(true), true)                                        // boxed bool
		add("f", lpg.PropertyValue{}, false)                                       // absent → NULL
		add("g", lpg.DateValue(time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)), true) // boxed temporal
		add("h", lpg.Float64Value(math.NaN()), true)                               // boxed NaN
		return g
	}

	colRes, colVals := drainScalarProjection(t, build(), "MATCH (n) RETURN n.val AS v")
	defer func() { _ = colRes.Close() }()
	rowRes, rowVals := drainScalarProjection(t, build(), "MATCH (n) RETURN coalesce(n.val) AS v")
	defer func() { _ = rowRes.Close() }()

	if colRes.matChunk == nil {
		t.Fatalf("columnar query did not engage the columnar path (matChunk == nil)")
	}
	if len(colVals) != len(rowVals) {
		t.Fatalf("row counts differ: columnar=%d row=%d", len(colVals), len(rowVals))
	}
	for i := range colVals {
		if !valuesByteIdentical(colVals[i], rowVals[i]) {
			t.Fatalf("row %d byte-identity: columnar=%#v row-path=%#v", i, colVals[i], rowVals[i])
		}
	}
}
