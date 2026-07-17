package cypher

// columnar_with_passthrough_test.go — byte-identity guard for the columnar
// scalar-column passthrough over a WITH-projection (#1704 follow-up, task #2045).
//
// When a WITH-projection materialises a scalar column column-major (a
// [exec.ColumnarProject] over a columnar scan/filter chain), a following
// projection that merely selects/reorders/renames those scalar variables
// (`... WITH n.v AS v RETURN v`) now consumes the child column-major and COPIES the
// already-materialised cells unboxed instead of pulling the child row-at-a-time and
// re-boxing every cell at the operator boundary. The copied value must be, at the
// API boundary, byte-identical to what the fully boxed row-at-a-time path produces.
//
// These tests prove that by running the SAME data two ways over the SAME scan:
//
//   - columnar:  MATCH (n) WITH n.v AS v RETURN v
//   - row path:  MATCH (n) WITH coalesce(n.v) AS v RETURN v
//
// coalesce(x) returns x unchanged for a single argument, so the two are
// value-equivalent; but the function call disqualifies the inner columnar
// projection, so the inner WITH is a row Project — which is not a ChunkProducer, so
// the outer RETURN cannot take the scalar-column passthrough either. The whole
// query therefore runs boxed row-at-a-time (the exact pre-change behaviour). Any
// divergence is a byte-identity defect. matChunk asserts the columnar query engaged
// the columnar path and the coalesce query did not, so the comparison is not
// vacuous.

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestColumnarScalarPassthrough_ByteIdentity_Scalars drives every scalar kind
// through a WITH-projection scalar-column passthrough and checks the columnar and
// row paths agree at the sink.
func TestColumnarScalarPassthrough_ByteIdentity_Scalars(t *testing.T) {
	cases := []struct {
		name     string
		set      bool
		val      lpg.PropertyValue
		wantKind expr.Kind
	}{
		{name: "int_zero", set: true, val: lpg.Int64Value(0), wantKind: expr.KindInteger},
		{name: "int_256_above_cache", set: true, val: lpg.Int64Value(256), wantKind: expr.KindInteger},
		{name: "int_negative", set: true, val: lpg.Int64Value(-12345), wantKind: expr.KindInteger},
		{name: "int_max", set: true, val: lpg.Int64Value(math.MaxInt64), wantKind: expr.KindInteger},
		{name: "float_pi", set: true, val: lpg.Float64Value(3.14159), wantKind: expr.KindFloat},
		{name: "float_nan", set: true, val: lpg.Float64Value(math.NaN()), wantKind: expr.KindFloat},
		{name: "float_neg_zero", set: true, val: lpg.Float64Value(math.Copysign(0, -1)), wantKind: expr.KindFloat},
		{name: "bool_true", set: true, val: lpg.BoolValue(true), wantKind: expr.KindBool},
		{name: "string_unicode", set: true, val: lpg.StringValue("héllo wörld 日本語 😀"), wantKind: expr.KindString},
		{name: "string_tag_byte_not_temporal", set: true, val: lpg.StringValue("\x01not-a-date"), wantKind: expr.KindString},
		// Temporal: stored SOH-tagged; the inner projection keeps it boxed as a Date,
		// and the passthrough copies the boxed cell — it must stay a Date, not a raw
		// string.
		{name: "temporal_date", set: true, val: lpg.DateValue(time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)), wantKind: expr.KindDate},
		// List: kept boxed via the canonical converter, copied boxed.
		{name: "list", set: true, val: lpg.ListValue([]lpg.PropertyValue{lpg.Int64Value(1), lpg.Int64Value(2)}), wantKind: expr.KindList},
		{name: "absent_null", set: false, val: lpg.PropertyValue{}, wantKind: expr.KindNull},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			colRes, colVals := drainScalarProjection(t, singleValGraph(t, tc.val, tc.set),
				"MATCH (n) WITH n.val AS v RETURN v")
			defer func() { _ = colRes.Close() }()
			rowRes, rowVals := drainScalarProjection(t, singleValGraph(t, tc.val, tc.set),
				"MATCH (n) WITH coalesce(n.val) AS v RETURN v")
			defer func() { _ = rowRes.Close() }()

			if colRes.matChunk == nil {
				t.Fatalf("columnar WITH-passthrough query did not engage the columnar path (matChunk == nil)")
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

// TestColumnarScalarPassthrough_ByteIdentity_Filtered proves the passthrough is
// byte-identical over the surviving rows when a WHERE precedes the WITH — the full
// scan → ColumnarFilter → ColumnarProject → scalar-passthrough ColumnarProject
// chain — across every kind in the mixed graph.
func TestColumnarScalarPassthrough_ByteIdentity_Filtered(t *testing.T) {
	colRes, colVals := drainFilteredValues(t, mixedFilterGraph(t),
		"MATCH (n) WHERE n.v >= 0 WITH n.v AS v RETURN v")
	defer func() { _ = colRes.Close() }()
	rowRes, rowVals := drainFilteredValues(t, mixedFilterGraph(t),
		"MATCH (n) WHERE coalesce(n.v) >= 0 WITH coalesce(n.v) AS v RETURN v")
	defer func() { _ = rowRes.Close() }()

	if colRes.matChunk == nil {
		t.Fatalf("columnar query did not engage the columnar path (matChunk == nil)")
	}
	if rowRes.matChunk != nil {
		t.Fatalf("coalesce query unexpectedly took the columnar path")
	}
	if len(colVals) != len(rowVals) {
		t.Fatalf("surviving row counts differ: columnar=%d row=%d", len(colVals), len(rowVals))
	}
	for i := range colVals {
		if !valuesByteIdentical(colVals[i], rowVals[i]) {
			t.Fatalf("row %d byte-identity: columnar=%#v row-path=%#v", i, colVals[i], rowVals[i])
		}
	}
}

// heteroTwoPropGraph builds a graph whose nodes carry two properties "v" and "w"
// spanning multiple kinds, so a multi-column passthrough (reorder/select) exercises
// heterogeneous columns and NULLs. Node keys are zero-padded for a stable scan
// order across two builds.
func heteroTwoPropGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	rows := []struct {
		setV, setW bool
		v, w       lpg.PropertyValue
	}{
		{true, true, lpg.Int64Value(1), lpg.StringValue("a")},
		{true, true, lpg.Int64Value(1000), lpg.StringValue("")},
		{true, true, lpg.Float64Value(2.5), lpg.BoolValue(true)},
		{true, false, lpg.StringValue("x"), lpg.PropertyValue{}},
		{false, true, lpg.PropertyValue{}, lpg.Float64Value(math.NaN())},
		{true, true, lpg.DateValue(time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)), lpg.Int64Value(-7)},
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i, r := range rows {
		key := padKey(i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if r.setV {
			if err := g.SetNodeProperty(key, "v", r.v); err != nil {
				t.Fatalf("SetNodeProperty v: %v", err)
			}
		}
		if r.setW {
			if err := g.SetNodeProperty(key, "w", r.w); err != nil {
				t.Fatalf("SetNodeProperty w: %v", err)
			}
		}
	}
	return g
}

// drainTwoCols runs a query projecting columns c0 and c1 and returns the ordered
// per-row (c0, c1) pairs, plus the Result.
func drainTwoCols(t *testing.T, g *lpg.Graph[string, float64], query, c0, c1 string) (*Result, [][2]expr.Value) {
	t.Helper()
	eng := NewEngine(g)
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	var pairs [][2]expr.Value
	for res.Next() {
		v0 := res.ValueAt(0)
		v1 := res.ValueAt(1)
		rec := res.Record()
		n0, _ := rec[c0].(expr.Value)
		n1, _ := rec[c1].(expr.Value)
		if !valuesByteIdentical(v0, n0) || !valuesByteIdentical(v1, n1) {
			t.Fatalf("ValueAt/Record disagree for %q", query)
		}
		pairs = append(pairs, [2]expr.Value{v0, v1})
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	return res, pairs
}

// TestColumnarScalarPassthrough_ReorderRename proves a passthrough that reorders
// and renames two materialised scalar columns stays byte-identical to the row path.
func TestColumnarScalarPassthrough_ReorderRename(t *testing.T) {
	// Columnar: inner WITH materialises v and w; outer RETURN reorders to (w, v)
	// and renames them (b, a). Both items are bare scalar variables → passthrough.
	const colQ = "MATCH (n) WITH n.v AS v, n.w AS w RETURN w AS b, v AS a"
	// Row reference: coalesce disqualifies the inner columnar projection.
	const rowQ = "MATCH (n) WITH coalesce(n.v) AS v, coalesce(n.w) AS w RETURN w AS b, v AS a"

	colRes, colPairs := drainTwoCols(t, heteroTwoPropGraph(t), colQ, "b", "a")
	defer func() { _ = colRes.Close() }()
	rowRes, rowPairs := drainTwoCols(t, heteroTwoPropGraph(t), rowQ, "b", "a")
	defer func() { _ = rowRes.Close() }()

	if colRes.matChunk == nil {
		t.Fatalf("columnar reorder/rename query did not engage the columnar path")
	}
	if rowRes.matChunk != nil {
		t.Fatalf("coalesce query unexpectedly took the columnar path")
	}
	if len(colPairs) != len(rowPairs) {
		t.Fatalf("row counts differ: columnar=%d row=%d", len(colPairs), len(rowPairs))
	}
	for i := range colPairs {
		if !valuesByteIdentical(colPairs[i][0], rowPairs[i][0]) || !valuesByteIdentical(colPairs[i][1], rowPairs[i][1]) {
			t.Fatalf("row %d byte-identity: columnar=%#v row=%#v", i, colPairs[i], rowPairs[i])
		}
	}
}

// TestColumnarScalarPassthrough_SelectSubset proves selecting a subset of the
// materialised scalar columns is byte-identical to the row path.
func TestColumnarScalarPassthrough_SelectSubset(t *testing.T) {
	colRes, colVals := drainScalarProjection(t, heteroTwoPropGraph(t),
		"MATCH (n) WITH n.v AS v, n.w AS w RETURN v")
	defer func() { _ = colRes.Close() }()
	rowRes, rowVals := drainScalarProjection(t, heteroTwoPropGraph(t),
		"MATCH (n) WITH coalesce(n.v) AS v, coalesce(n.w) AS w RETURN v")
	defer func() { _ = rowRes.Close() }()

	if colRes.matChunk == nil {
		t.Fatalf("columnar select-subset query did not engage the columnar path")
	}
	if rowRes.matChunk != nil {
		t.Fatalf("coalesce query unexpectedly took the columnar path")
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
