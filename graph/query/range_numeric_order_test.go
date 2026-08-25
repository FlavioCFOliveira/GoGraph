package query_test

// range_numeric_order_test.go — black-box regression cover for [query.WithRange]
// under openCypher's single numeric order (task #2600).
//
// Before #2600 the predicate required the stored value and BOTH bounds to share
// a PropertyValue kind. That made the scan arm disagree with the index-served
// arm, with the Cypher engine, and with the specification: openCypher orders
// INTEGER and FLOAT in one numeric order — the only off-diagonal entry of the
// comparability matrix in the normative CIP "Comparability and Orderability",
// pinned by the TCK in expressions/comparison/Comparison2.feature ("Comparing
// across types yields null, except numbers") and Comparison1.feature
// ("1 = 1.0" is true).
//
// Every case below is asserted over the CROSS PRODUCT of
//
//   - four index shapes: none, the engine-shaped float64 numeric companion, an
//     int64-keyed btree (no longer consulted since #2600), and a string-keyed
//     btree; and
//   - two query shapes: Vertex(label, pred) — where a covering index may serve
//     the predicate — and Vertex(label).Vertex(pred), where labelsInPreds is
//     empty so no index can be used and the per-node comparison always runs.
//
// All eight arms must return the SAME set, and that set is stated explicitly
// rather than derived from one of the arms, so the test cannot pass by two
// broken arms agreeing.

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

const (
	rnLabel = "Sensor"
	rnProp  = "v"
)

// The large-integer values the openCypher TCK uses to forbid a float64
// promotion: rnBigAbove and rnBigBelow are distinct integers that both round to
// rnPow62, so an implementation that widens the integer conflates all three.
const (
	rnPow62    int64 = 4611686018427387904 // 2^62, exactly representable
	rnBigAbove int64 = 4611686018427387905 // 2^62 + 1
	rnBigBelow int64 = 4611686018427387900 // 2^62 - 4
)

// buildMixedValueGraph builds a :Sensor directory whose `v` property carries a
// deliberately mixed set of values: integers and floats that must compare in one
// numeric order, the three large integers that share a float64 key, a NaN, a
// string, a bool, and a node with no `v` at all.
func buildMixedValueGraph(tb testing.TB) (*lpg.Graph[string, int64], *csr.CSR[int64]) {
	tb.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	values := []struct {
		key string
		v   lpg.PropertyValue
	}{
		{"i5", lpg.Int64Value(5)},
		{"i6", lpg.Int64Value(6)},
		{"f5", lpg.Float64Value(5)},
		{"f55", lpg.Float64Value(5.5)},
		{"pow62", lpg.Int64Value(rnPow62)},
		{"bigAbove", lpg.Int64Value(rnBigAbove)},
		{"bigBelow", lpg.Int64Value(rnBigBelow)},
		{"nan", lpg.Float64Value(math.NaN())},
		{"str5", lpg.StringValue("5")},
		{"yes", lpg.BoolValue(true)},
	}
	for _, e := range values {
		if err := g.SetNodeLabel(e.key, rnLabel); err != nil {
			tb.Fatalf("SetNodeLabel %s: %v", e.key, err)
		}
		if err := g.SetNodeProperty(e.key, rnProp, e.v); err != nil {
			tb.Fatalf("SetNodeProperty %s: %v", e.key, err)
		}
	}
	// A labelled node with NO `v`: every bound test over an absent property must
	// be false, on both the seek and the scan arm.
	if err := g.SetNodeLabel("bare", rnLabel); err != nil {
		tb.Fatalf("SetNodeLabel bare: %v", err)
	}
	return g, csr.BuildFromAdjList(g.AdjList())
}

// projNumericUnified mirrors cypher/index_binding.go projectNumericPropValue:
// BOTH integer and float values are keyed under one float64 order, and NaN is
// never indexed. It is the projection the numeric arm's coverage contract
// requires (see index_seek.go btreeRanger).
func projNumericUnified(pv lpg.PropertyValue) (float64, bool) {
	switch pv.Kind() {
	case lpg.PropInt64:
		i, ok := pv.Int64()
		if !ok {
			return 0, false
		}
		return float64(i), true
	case lpg.PropFloat64:
		f, ok := pv.Float64()
		if !ok || math.IsNaN(f) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// projInt64Only keys ONLY integer values — the shape whose range arm #2600
// removed, because it cannot hold the float-valued nodes a numeric range must
// also match.
func projInt64Only(pv lpg.PropertyValue) (int64, bool) {
	if pv.Kind() != lpg.PropInt64 {
		return 0, false
	}
	return pv.Int64()
}

// projStringOnly keys only string values, as a Cypher btree CREATE INDEX does.
func projStringOnly(pv lpg.PropertyValue) (string, bool) {
	if pv.Kind() != lpg.PropString {
		return "", false
	}
	return pv.String()
}

// rnIndexShapes are the four index configurations every case is run against.
var rnIndexShapes = []struct {
	name   string
	attach func(testing.TB, *lpg.Graph[string, int64])
}{
	{name: "no-index", attach: nil},
	{name: "numeric-companion-float64", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachBTreeIndex(tb, g, rnLabel, rnProp, "sensor_v_btree_num", projNumericUnified)
	}},
	{name: "int64-keyed-btree", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachBTreeIndex(tb, g, rnLabel, rnProp, "sensor_v_btree_int", projInt64Only)
	}},
	{name: "string-keyed-btree", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachBTreeIndex(tb, g, rnLabel, rnProp, "sensor_v_btree_str", projStringOnly)
	}},
	{name: "string-and-numeric-btrees", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachBTreeIndex(tb, g, rnLabel, rnProp, "sensor_v_btree_str", projStringOnly)
		attachBTreeIndex(tb, g, rnLabel, rnProp, "sensor_v_btree_num", projNumericUnified)
	}},
}

func TestWithRange_UnifiedNumericOrder(t *testing.T) {
	t.Parallel()

	// Precondition: the three large integers must really share one float64 key,
	// or the boundary cases below would not discriminate a promoting comparison.
	if float64(rnBigAbove) != float64(rnPow62) || float64(rnBigBelow) != float64(rnPow62) {
		t.Fatalf("premise broken: %d, %d and %d no longer share a float64 key",
			rnBigAbove, rnBigBelow, rnPow62)
	}

	nan := lpg.Float64Value(math.NaN())
	cases := []struct {
		name   string
		lo, hi lpg.PropertyValue
		want   []string
	}{
		// One numeric order: a float bound matches an integer value and back.
		{"float point over mixed numerics", lpg.Float64Value(5), lpg.Float64Value(5),
			[]string{"f5", "i5"}},
		{"int point over mixed numerics", lpg.Int64Value(5), lpg.Int64Value(5),
			[]string{"f5", "i5"}},
		{"float window over mixed numerics", lpg.Float64Value(5), lpg.Float64Value(6),
			[]string{"f5", "f55", "i5", "i6"}},

		// The two bound tests are INDEPENDENT, so the bounds need not share a
		// kind (CIP "Comparability and Orderability": each comparison stands on
		// its own).
		{"mixed bounds int lo / float hi", lpg.Int64Value(5), lpg.Float64Value(5.5),
			[]string{"f5", "f55", "i5"}},
		{"mixed bounds float lo / int hi", lpg.Float64Value(5.5), lpg.Int64Value(6),
			[]string{"f55", "i6"}},

		// EXACT above 2^53: the three large integers share a float64 key, so only
		// an exact comparison can separate them.
		{"float bound at 2^62", lpg.Float64Value(float64(rnPow62)), lpg.Float64Value(float64(rnPow62)),
			[]string{"pow62"}},
		{"int bound at 2^62+1", lpg.Int64Value(rnBigAbove), lpg.Int64Value(rnBigAbove),
			[]string{"bigAbove"}},
		{"int bound at 2^62-4", lpg.Int64Value(rnBigBelow), lpg.Int64Value(rnBigBelow),
			[]string{"bigBelow"}},
		{"int window spanning the three", lpg.Int64Value(rnBigBelow), lpg.Int64Value(rnBigAbove),
			[]string{"bigAbove", "bigBelow", "pow62"}},

		// NaN: every comparison against it is FALSE, never null, so nothing
		// matches — and the NaN-valued node matches no ordinary window either.
		{"NaN lower bound", nan, lpg.Float64Value(5), nil},
		{"NaN upper bound", lpg.Float64Value(5), nan, nil},
		{"NaN both bounds", nan, nan, nil},
		{"window that would contain NaN if it were ordered",
			lpg.Float64Value(math.Inf(-1)), lpg.Float64Value(math.Inf(1)),
			[]string{"bigAbove", "bigBelow", "f5", "f55", "i5", "i6", "pow62"}},

		// Strings order among themselves only: the string-valued node matches a
		// string window and no numeric one.
		{"string window", lpg.StringValue("1"), lpg.StringValue("9"), []string{"str5"}},

		// Cross-family bounds can never both hold, whatever the value.
		{"int lo / string hi", lpg.Int64Value(1), lpg.StringValue("9"), nil},
		{"string lo / float hi", lpg.StringValue("1"), lpg.Float64Value(9), nil},

		// Kinds openCypher does not order at all.
		{"bool bounds", lpg.BoolValue(false), lpg.BoolValue(true), nil},
		{"bytes bounds", lpg.BytesValue([]byte{0}), lpg.BytesValue([]byte{9}), nil},
	}

	nonEmpty := 0
	for _, tc := range cases {
		if len(tc.want) > 0 {
			nonEmpty++
		}
		for _, shape := range rnIndexShapes {
			t.Run(fmt.Sprintf("%s/%s", tc.name, shape.name), func(t *testing.T) {
				t.Parallel()
				g, c := buildMixedValueGraph(t)
				if shape.attach != nil {
					shape.attach(t, g)
				}
				eng := query.New(g, c)
				label := query.WithLabel[string, int64](rnLabel)
				pred := query.WithRange[string, int64](rnProp, tc.lo, tc.hi)

				// Arm 1: the predicate sits in the SAME Vertex call as the label,
				// so a covering index may serve it.
				seek := collectSorted(eng.Match().Vertex(label, pred))
				// Arm 2: a SECOND Vertex call, so labelsInPreds is empty and both
				// trySeekProperty and trySeekRange refuse — the per-node comparison
				// is the only path.
				scan := collectSorted(eng.Match().Vertex(label).Vertex(pred))

				want := append([]string(nil), tc.want...)
				sort.Strings(want)
				if !equalStrings(seek, want) {
					t.Fatalf("seek arm = %v, want %v", seek, want)
				}
				if !equalStrings(scan, want) {
					t.Fatalf("scan arm = %v, want %v", scan, want)
				}
			})
		}
	}
	if nonEmpty == 0 {
		t.Fatal("no case expects a non-empty answer; the whole table would be vacuous")
	}
}

// TestWithRange_AbsentPropertyNeverMatches pins that the labelled node with no
// `v` property is excluded on every arm. It is separate from the table because
// the assertion is about a node that appears in NO expected answer above, which
// is easy to satisfy by accident: here it is the ONLY node in the graph.
func TestWithRange_AbsentPropertyNeverMatches(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("bare", rnLabel); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	eng := query.New(g, c)
	label := query.WithLabel[string, int64](rnLabel)

	// The label alone must find it, or the negative result below is vacuous.
	if all := collectSorted(eng.Match().Vertex(label)); len(all) != 1 || all[0] != "bare" {
		t.Fatalf("the label seed found %v, want [bare]; the range assertion would be vacuous", all)
	}
	pred := query.WithRange[string, int64](rnProp, lpg.Int64Value(math.MinInt64), lpg.Int64Value(math.MaxInt64))
	if got := collectSorted(eng.Match().Vertex(label, pred)); len(got) != 0 {
		t.Fatalf("a node with no %q property matched the widest possible range: %v", rnProp, got)
	}
}
