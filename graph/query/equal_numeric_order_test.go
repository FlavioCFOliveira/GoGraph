package query_test

// equal_numeric_order_test.go — black-box regression cover for
// [query.WithProperty] under openCypher's single numeric EQUALITY (task #2601).
//
// Task #2600 unified the numeric ORDER for [query.WithRange] and deliberately
// left [query.WithProperty] alone, which created an asymmetry the same task
// recorded as debt: over a stored [lpg.Int64Value] of 5,
//
//	WithRange("v", Float64Value(5), Float64Value(5))   MATCHED
//	WithProperty("v", Float64Value(5))                 did NOT
//
// so the same data answered differently depending on whether the predicate was
// written as an equality or as a degenerate range. Closing that is #2601, and
// [TestWithProperty_AgreesWithDegenerateRange] is the direct assertion of it.
//
// # Which relation this file pins
//
// The relation is EQUALITY — not comparability, and not equivalence. All three
// unify INTEGER and FLOAT, and they differ in two ways that matter here:
//
//   - At NaN. Under equality NaN = NaN is FALSE (IEEE-754, which openCypher
//     adopts), so a NaN expected value matches nothing and a NaN-valued node is
//     matched by nothing. Under EQUIVALENCE NaN is equivalent to NaN — a value
//     set folds it onto itself (cypher/exec/constraints.go floatCanonicalKey) —
//     which is why that canonical-key path is NOT reused as an equality
//     comparator.
//   - In WIDTH. openCypher's equatability is WIDER than its comparability:
//     BOOLEAN, BYTES and TIME values are equal to themselves but are not
//     ordered scalars, so no [query.WithRange] over them can ever hold.
//
// The equality/degenerate-range identity is therefore scoped to the ORDERABLE
// kinds — string and numeric — and the deliberate divergence for the
// equatable-but-not-orderable kinds is pinned separately by
// [TestWithProperty_EquatableButNotOrderableKindsDivergeFromRange], so that a
// later reader cannot mistake it for the #2601 defect coming back.
//
// # Authority (cited, not re-derived)
//
// The TCK has 1 = 1.0 as true (expressions/comparison/Comparison1.feature) and
// an INTEGER-typed 0 = 0.5 as FALSE rather than null (the "Number-typed float
// comparison" scenario). The normative CIP2016-06-14 "Comparability and
// Orderability" says numbers of different types can be equal and compared to
// each other, and its matrix has INTEGER x FLOAT as the only off-diagonal
// entry. Comparison1.feature also pins 4611686018427387905 as NOT equal to
// 4611686018427387900 while both round to the same float64 (2^62), so the
// comparison must be EXACT and can never be a float64 promotion.
//
// # Why the index shapes are swept
//
// Unifying equality changes which index shapes can serve it. A single-kind hash
// index was an exact mirror of a shared-kind equality and is a SUBSET of a
// unified one, and a subset cannot be repaired by a residual filter. Every case
// below therefore runs against every shape that can cover (Sensor, v) — hash
// and btree, single-kind and unified — and against both query shapes, so an arm
// that under- or over-returns fails here rather than only in production.

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

// projFloat64Only keys ONLY float values — the hash shape whose equality arm
// #2601 removed, because under a unified equality it cannot hold the
// integer-valued nodes the predicate must also match.
func projFloat64Only(pv lpg.PropertyValue) (float64, bool) {
	if pv.Kind() != lpg.PropFloat64 {
		return 0, false
	}
	return pv.Float64()
}

// projBoolOnly keys only boolean values.
func projBoolOnly(pv lpg.PropertyValue) (bool, bool) {
	if pv.Kind() != lpg.PropBool {
		return false, false
	}
	return pv.Bool()
}

// eqIndexShapes are the index configurations every equality case is run
// against. They are the union of the range test's btree shapes and the hash
// shapes an equality can be served from, because #2601 had to re-decide BOTH
// families at once:
//
//   - hash-string-keyed stays EXACT: a string equality is not unified with
//     anything, so a bound string hash index remains a faithful mirror.
//   - hash-bool-keyed stays EXACT for the same reason.
//   - hash-int64-keyed and hash-float64-keyed are SUBSETS of a unified numeric
//     equality and must no longer be consulted at all.
//   - hash-numeric-unified (a float64-keyed hash keying BOTH kinds) is a
//     superset rather than a subset, but it is indistinguishable from
//     hash-float64-keyed through the typed read interface, so it is not
//     consulted either. Its 2^62 case below is what proves that matters: all
//     three large integers share one float64 key, so serving it as EXACT would
//     over-return with no residual filter to repair it.
//   - numeric-companion-float64 (the engine's btree) serves a numeric equality
//     as a degenerate range, as a SUPERSET with the per-node comparison as the
//     exact residual filter.
var eqIndexShapes = []struct {
	name   string
	attach func(testing.TB, *lpg.Graph[string, int64])
}{
	{name: "no-index", attach: nil},
	{name: "hash-string-keyed", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachHashIndex(tb, g, rnLabel, rnProp, "sensor_v_hash_str", projStringOnly)
	}},
	{name: "hash-int64-keyed", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachHashIndex(tb, g, rnLabel, rnProp, "sensor_v_hash_int", projInt64Only)
	}},
	{name: "hash-float64-keyed", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachHashIndex(tb, g, rnLabel, rnProp, "sensor_v_hash_f64", projFloat64Only)
	}},
	{name: "hash-numeric-unified", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachHashIndex(tb, g, rnLabel, rnProp, "sensor_v_hash_num", projNumericUnified)
	}},
	{name: "hash-bool-keyed", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachHashIndex(tb, g, rnLabel, rnProp, "sensor_v_hash_bool", projBoolOnly)
	}},
	{name: "numeric-companion-float64", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachBTreeIndex(tb, g, rnLabel, rnProp, "sensor_v_btree_num", projNumericUnified)
	}},
	{name: "int64-keyed-btree", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachBTreeIndex(tb, g, rnLabel, rnProp, "sensor_v_btree_int", projInt64Only)
	}},
	{name: "hash-string-and-numeric-btree", attach: func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachHashIndex(tb, g, rnLabel, rnProp, "sensor_v_hash_str", projStringOnly)
		attachBTreeIndex(tb, g, rnLabel, rnProp, "sensor_v_btree_num", projNumericUnified)
	}},
}

// eqCase is one expected equality answer over [buildMixedValueGraph].
type eqCase struct {
	name string
	v    lpg.PropertyValue
	want []string
}

// eqCases is the table shared by [TestWithProperty_UnifiedNumericEquality] and
// [TestWithProperty_AgreesWithDegenerateRange]. Sharing it is deliberate: the
// identity claim is only worth something over the same corpus and the same
// values the equality answers are pinned at.
//
// Every answer is stated explicitly rather than derived from one of the arms, so
// the table cannot pass by two broken arms agreeing.
func eqCases() []eqCase {
	return []eqCase{
		// ONE numeric order: an INTEGER expected value matches a FLOAT-valued
		// node and back. This is the whole of #2601 in two rows.
		{"int 5 matches both numeric kinds", lpg.Int64Value(5), []string{"f5", "i5"}},
		{"float 5 matches both numeric kinds", lpg.Float64Value(5), []string{"f5", "i5"}},

		// A non-integral float equals only the float-valued node, and an
		// INTEGER-typed comparison against it is FALSE rather than null
		// (Comparison1.feature "Number-typed float comparison").
		{"float 5.5", lpg.Float64Value(5.5), []string{"f55"}},
		{"int 6", lpg.Int64Value(6), []string{"i6"}},
		{"float 6 matches the int-valued node", lpg.Float64Value(6), []string{"i6"}},

		// EXACT above 2^53. The three large integers share one float64 key, so a
		// promoting comparison would return all three for any of these rows, and
		// an index arm trusted as exact would too.
		{"float bound at 2^62", lpg.Float64Value(float64(rnPow62)), []string{"pow62"}},
		{"int at 2^62", lpg.Int64Value(rnPow62), []string{"pow62"}},
		{"int at 2^62+1", lpg.Int64Value(rnBigAbove), []string{"bigAbove"}},
		{"int at 2^62-4", lpg.Int64Value(rnBigBelow), []string{"bigBelow"}},

		// NaN: equality against NaN is FALSE, never null and never true, so a NaN
		// expected value matches nothing at all — including the NaN-valued node.
		{"NaN expected value", lpg.Float64Value(math.NaN()), nil},
		// ... and the NaN-valued node is matched by no ordinary value either.
		{"zero matches nothing in this corpus", lpg.Int64Value(0), nil},

		// Strings equal strings only: only INTEGER x FLOAT unifies, so the
		// string-valued node is not equal to any number and vice versa.
		{"string 5", lpg.StringValue("5"), []string{"str5"}},
		{"string that is no node's value", lpg.StringValue("nope"), nil},
	}
}

// TestWithProperty_UnifiedNumericEquality asserts the equality answer over the
// full cross product of index shapes and query shapes. All arms must return the
// same explicitly-stated set.
func TestWithProperty_UnifiedNumericEquality(t *testing.T) {
	t.Parallel()

	// Precondition for every exactness row: the three large integers must really
	// collapse onto one float64, or those rows would not discriminate a promoting
	// comparison or an over-returning index arm.
	if float64(rnBigAbove) != float64(rnPow62) || float64(rnBigBelow) != float64(rnPow62) {
		t.Fatalf("premise broken: %d, %d and %d no longer share a float64 key",
			rnBigAbove, rnBigBelow, rnPow62)
	}

	nonEmpty := 0
	for _, tc := range eqCases() {
		if len(tc.want) > 0 {
			nonEmpty++
		}
		for _, shape := range eqIndexShapes {
			t.Run(fmt.Sprintf("%s/%s", tc.name, shape.name), func(t *testing.T) {
				t.Parallel()
				g, c := buildMixedValueGraph(t)
				if shape.attach != nil {
					shape.attach(t, g)
				}
				eng := query.New(g, c)
				label := query.WithLabel[string, int64](rnLabel)
				pred := query.WithProperty[string, int64](rnProp, tc.v)

				// Arm 1: the predicate sits in the SAME Vertex call as the label, so
				// a covering index may serve it.
				seek := collectSorted(eng.Match().Vertex(label, pred))
				// Arm 2: a SECOND Vertex call, so labelsInPreds is empty and
				// trySeekProperty refuses — the per-node comparison is the only path.
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

// TestWithProperty_AgreesWithDegenerateRange is the direct assertion of the
// asymmetry #2601 closes: for an ORDERABLE value v, the equality
// WithProperty(k, v) and the degenerate range WithRange(k, v, v) must return the
// same set — on the seek arm and on the scan arm, under every index shape.
//
// Before #2601 the two disagreed for every mixed-kind numeric row: the range
// unified INTEGER and FLOAT (#2600) and the equality still required a shared
// kind.
//
// It is deliberately NOT a claim about every kind. openCypher's equatability is
// wider than its comparability, so for BOOLEAN, BYTES and TIME the two
// predicates legitimately differ — see
// [TestWithProperty_EquatableButNotOrderableKindsDivergeFromRange].
func TestWithProperty_AgreesWithDegenerateRange(t *testing.T) {
	t.Parallel()

	agreedNonEmpty := 0
	for _, tc := range eqCases() {
		if len(tc.want) > 0 {
			agreedNonEmpty++
		}
		for _, shape := range eqIndexShapes {
			t.Run(fmt.Sprintf("%s/%s", tc.name, shape.name), func(t *testing.T) {
				t.Parallel()
				g, c := buildMixedValueGraph(t)
				if shape.attach != nil {
					shape.attach(t, g)
				}
				eng := query.New(g, c)
				label := query.WithLabel[string, int64](rnLabel)
				eq := query.WithProperty[string, int64](rnProp, tc.v)
				rng := query.WithRange[string, int64](rnProp, tc.v, tc.v)

				eqSeek := collectSorted(eng.Match().Vertex(label, eq))
				eqScan := collectSorted(eng.Match().Vertex(label).Vertex(eq))
				rgSeek := collectSorted(eng.Match().Vertex(label, rng))
				rgScan := collectSorted(eng.Match().Vertex(label).Vertex(rng))

				for _, arm := range []struct {
					name string
					got  []string
				}{
					{"equality seek", eqSeek},
					{"equality scan", eqScan},
					{"degenerate-range seek", rgSeek},
					{"degenerate-range scan", rgScan},
				} {
					if !equalStrings(arm.got, eqSeek) {
						t.Fatalf("%s = %v, but the equality seek arm = %v: an equality and the "+
							"degenerate range over the same value must agree (#2601)",
							arm.name, arm.got, eqSeek)
					}
				}
			})
		}
	}
	if agreedNonEmpty == 0 {
		t.Fatal("every case agreed on the EMPTY set; two empty answers agree whatever the " +
			"comparison does, so the identity would be vacuous")
	}
}

// eqKindLabel and eqKindProp scope the equatable-but-not-orderable fixture,
// deliberately away from (Sensor, v) so it cannot be confused with the shared
// mixed-value corpus.
const (
	eqKindLabel = "Widget"
	eqKindProp  = "k"
)

// TestWithProperty_EquatableButNotOrderableKindsDivergeFromRange pins the ONE
// place an equality and a degenerate range must NOT agree, so that a future
// reader does not "fix" it as a leftover of #2601.
//
// openCypher's equatability is wider than its comparability. BOOLEAN, BYTES and
// TIME are equal to themselves but are not ordered scalars, so
// query.compareValues reports every pair over them as unordered and no
// [query.WithRange] over them can hold — while [query.WithProperty] matches. The
// divergence is a property of the two RELATIONS, not of the kinds' unification,
// and #2601 changed neither.
func TestWithProperty_EquatableButNotOrderableKindsDivergeFromRange(t *testing.T) {
	t.Parallel()

	stamp := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		v    lpg.PropertyValue
	}{
		{"bool", lpg.BoolValue(true)},
		{"bytes", lpg.BytesValue([]byte{1, 2, 3})},
		{"time", lpg.TimeValue(stamp)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := lpg.New[string, int64](adjlist.Config{Directed: true})
			if err := g.SetNodeLabel("w", eqKindLabel); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
			if err := g.SetNodeProperty("w", eqKindProp, tc.v); err != nil {
				t.Fatalf("SetNodeProperty: %v", err)
			}
			c := csr.BuildFromAdjList(g.AdjList())
			eng := query.New(g, c)
			label := query.WithLabel[string, int64](eqKindLabel)

			eq := collectSorted(eng.Match().Vertex(label,
				query.WithProperty[string, int64](eqKindProp, tc.v)))
			rg := collectSorted(eng.Match().Vertex(label,
				query.WithRange[string, int64](eqKindProp, tc.v, tc.v)))

			if len(eq) != 1 || eq[0] != "w" {
				t.Fatalf("the equality returned %v, want [w]: openCypher makes %s EQUATABLE",
					eq, tc.name)
			}
			if len(rg) != 0 {
				t.Fatalf("the degenerate range returned %v, want none: openCypher does not ORDER %s, "+
					"so both bound tests are unordered and the predicate cannot hold", rg, tc.name)
			}
		})
	}
}

// TestWithProperty_NegativeZeroEqualsZero pins the IEEE-754 outcome for the one
// numeric pair whose equality is easy to get wrong in the opposite direction
// from NaN: -0.0 and 0 are EQUAL, across kinds as well as within FLOAT.
//
// It matters because the exact comparator truncates towards zero, so -0.0 and
// +0.0 both reduce to an integral part of 0 with no fractional tiebreak, and a
// sign-aware comparison bolted on there would break this.
func TestWithProperty_NegativeZeroEqualsZero(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	values := []struct {
		key string
		v   lpg.PropertyValue
	}{
		{"iz", lpg.Int64Value(0)},
		{"fz", lpg.Float64Value(0)},
		{"fnz", lpg.Float64Value(math.Copysign(0, -1))},
		{"one", lpg.Int64Value(1)},
	}
	for _, e := range values {
		if err := g.SetNodeLabel(e.key, eqKindLabel); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", e.key, err)
		}
		if err := g.SetNodeProperty(e.key, eqKindProp, e.v); err != nil {
			t.Fatalf("SetNodeProperty %s: %v", e.key, err)
		}
	}
	// Premise: the negative-zero node really does carry a negatively-signed zero,
	// or the assertion below proves nothing about sign handling.
	pv, ok := g.GetNodeProperty("fnz", eqKindProp)
	if !ok {
		t.Fatal("fnz has no property")
	}
	f, _ := pv.Float64()
	if !math.Signbit(f) || f != 0 {
		t.Fatalf("premise broken: fnz carries %v (signbit=%v), want a negative zero",
			f, math.Signbit(f))
	}

	c := csr.BuildFromAdjList(g.AdjList())
	eng := query.New(g, c)
	label := query.WithLabel[string, int64](eqKindLabel)
	want := []string{"fnz", "fz", "iz"}
	for _, expected := range []lpg.PropertyValue{
		lpg.Int64Value(0), lpg.Float64Value(0), lpg.Float64Value(math.Copysign(0, -1)),
	} {
		got := collectSorted(eng.Match().Vertex(label,
			query.WithProperty[string, int64](eqKindProp, expected)))
		if !equalStrings(got, want) {
			t.Fatalf("equality against %v returned %v, want %v: -0.0 == 0.0 == 0 under IEEE-754, "+
				"which openCypher adopts", expected, got, want)
		}
	}
}
