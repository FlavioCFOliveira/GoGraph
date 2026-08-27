package cypher_test

// numeric_equality_seek_test.go — rmp #2169 (round-3 comparative audit).
//
// A numeric equality predicate never reached an index. A Cypher hash index is
// string-only, so MATCH (a:P {id: 250}) full-scanned the label even when the
// property carried a btree index whose float64 numeric companion could answer
// it — while the SAME predicate spelled as "a.id >= 250 AND a.id <= 250"
// already seeked. Equality is now rewritten to the degenerate closed range
// [v, v] over that companion.
//
// The risk this file exists to fence is CORRECTNESS, not speed. The companion
// keys on float64, so:
//
//   - openCypher numeric equality is CROSS-TYPE (5 = 5.0 is TRUE), so the seek
//     must return integer- and float-valued matches alike. It does, because the
//     companion indexes both under one numeric order.
//   - above 2^53 distinct int64 values share a float64 image, so the seek
//     returns extra candidates. The range seek always retains the original
//     predicate as a residual Filter, which applies the exact int/float
//     comparator, so 2^53+1 = 2^53 stays FALSE despite the shared bucket.
//
// Both are verified DIFFERENTIALLY: every query runs against two graphs built
// from identical data, one with the index (seek plan) and one without (scan
// plan), and the row sets must be identical. The plans are asserted to actually
// differ, so the comparison cannot pass vacuously.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedNumericSeekGraph builds the fixture. The label population must clear
// rangeSeekMinLabelPopulation for the selectivity gate to admit any seek
// at all; withIndex selects the seek or the scan arm of the differential.
//
// The data deliberately mixes types and straddles the float64 integer limit:
//   - 2000 integer-valued nodes, v = 1..2000
//   - 50 float-valued nodes, v = 1.5 .. 50.5
//   - one float 5.0, so v = 5 has both an integer and a float match
//   - 2^53, 2^53+1 and 2^53+2, which all collide in float64 space
func seedNumericSeekGraph(t *testing.T, withIndex bool) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)

	run := func(q string) {
		t.Helper()
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("seed %q result: %v", q, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("seed %q close: %v", q, err)
		}
	}

	run(`UNWIND range(1, 2000) AS i CREATE (:P {v: i, tag: 'i' + toString(i)})`)
	run(`UNWIND range(1, 50) AS i CREATE (:P {v: i + 0.5, tag: 'f' + toString(i)})`)
	run(`CREATE (:P {v: 5.0, tag: 'float5'})`)
	run(`CREATE (:P {v: 9007199254740992, tag: 'p53'}),
	            (:P {v: 9007199254740993, tag: 'p53plus1'}),
	            (:P {v: 9007199254740994, tag: 'p53plus2'})`)

	if withIndex {
		run(`CREATE INDEX idx_p_v FOR (n:P) ON (n.v) OPTIONS {indexType: 'btree'}`)
	}
	return eng
}

// tagsFor runs query and returns the sorted "tag" column.
func tagsFor(t *testing.T, eng *cypher.Engine, query string, params map[string]any) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, params)
	if err != nil {
		t.Fatalf("RunAny(%q): %v", query, err)
	}
	defer res.Close()
	var got []string
	for res.Next() {
		rec := res.Record()
		v, ok := rec["tag"]
		if !ok {
			t.Fatalf("query %q returned no tag column", query)
		}
		got = append(got, valueText(v))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error for %q: %v", query, err)
	}
	sort.Strings(got)
	return got
}

// valueText renders a returned column as a comparable string. The tag column is
// always a string; anything else is rendered distinguishably so a mismatch
// reports what it actually saw rather than an empty value.
func valueText(v any) string {
	switch s := v.(type) {
	case expr.StringValue:
		return string(s)
	case string:
		return s
	default:
		return fmt.Sprintf("<non-string %T: %v>", v, v)
	}
}

// numericSeekCases are the predicates under test, with the row set each must
// return. Every case is checked against BOTH arms of the differential.
var numericSeekCases = []struct {
	name  string
	query string
	// wantSeek is true when the indexed arm must plan an index access path.
	wantSeek bool
	want     []string
}{
	// Cross-type equality: openCypher says 5 = 5.0, so an integer literal must
	// match the float-valued node too, and vice versa.
	{"int_literal_crosstype", `MATCH (a:P) WHERE a.v = 5 RETURN a.tag AS tag`, true,
		[]string{"float5", "i5"}},
	{"float_literal_crosstype", `MATCH (a:P) WHERE a.v = 5.0 RETURN a.tag AS tag`, true,
		[]string{"float5", "i5"}},

	// The inline property-map form desugars to the same predicate.
	{"inline_map_int", `MATCH (a:P {v: 1500}) RETURN a.tag AS tag`, true,
		[]string{"i1500"}},
	{"inline_map_float", `MATCH (a:P {v: 10.5}) RETURN a.tag AS tag`, true,
		[]string{"f10"}},

	// Plain integer and float equality.
	{"int_equality", `MATCH (a:P) WHERE a.v = 1999 RETURN a.tag AS tag`, true,
		[]string{"i1999"}},
	{"float_equality", `MATCH (a:P) WHERE a.v = 42.5 RETURN a.tag AS tag`, true,
		[]string{"f42"}},

	// Mirrored operand order.
	{"mirrored_literal_left", `MATCH (a:P) WHERE 1999 = a.v RETURN a.tag AS tag`, true,
		[]string{"i1999"}},

	// THE 2^53 BOUNDARY. float64(2^53) == float64(2^53+1), so the seek returns
	// both candidates and the residual filter must admit exactly one.
	{"pow53_exact", `MATCH (a:P) WHERE a.v = 9007199254740992 RETURN a.tag AS tag`, true,
		[]string{"p53"}},
	{"pow53_plus_one", `MATCH (a:P) WHERE a.v = 9007199254740993 RETURN a.tag AS tag`, true,
		[]string{"p53plus1"}},
	{"pow53_plus_two", `MATCH (a:P) WHERE a.v = 9007199254740994 RETURN a.tag AS tag`, true,
		[]string{"p53plus2"}},

	// No match at all: the seek must not invent rows, and an empty range must
	// not be planned as a seek (the gate declines a zero count).
	{"no_match", `MATCH (a:P) WHERE a.v = 999999 RETURN a.tag AS tag`, false, nil},

	// A non-selective equality is still correct; whether it seeks is the cost
	// model's business, so wantSeek is not asserted for it above.

	// The range path must be unchanged by the equality rewrite.
	{"closed_range", `MATCH (a:P) WHERE a.v >= 100 AND a.v <= 103 RETURN a.tag AS tag`, true,
		[]string{"i100", "i101", "i102", "i103"}},

	// NaN never equals anything, including itself: the seek must decline and
	// the scan must return nothing.
	{"nan_equality", `MATCH (a:P) WHERE a.v = (0.0 / 0.0) RETURN a.tag AS tag`, false, nil},
}

// TestNumericEqualitySeek_DifferentialAgainstScan is the acceptance test for
// #2169: identical results from the seek and the scan plan, on identical data.
func TestNumericEqualitySeek_DifferentialAgainstScan(t *testing.T) {
	indexed := seedNumericSeekGraph(t, true)
	plain := seedNumericSeekGraph(t, false)

	for _, tc := range numericSeekCases {
		t.Run(tc.name, func(t *testing.T) {
			gotIndexed := tagsFor(t, indexed, tc.query, nil)
			gotPlain := tagsFor(t, plain, tc.query, nil)

			if !equalStrings(gotIndexed, gotPlain) {
				t.Fatalf("%s: index arm returned %v, scan arm returned %v — the seek changed the result",
					tc.query, gotIndexed, gotPlain)
			}
			if !equalStrings(gotIndexed, tc.want) {
				t.Fatalf("%s: returned %v, want %v", tc.query, gotIndexed, tc.want)
			}
		})
	}
}

// TestNumericEqualitySeek_PlanUsesIndex pins acceptance criterion (1): the plan
// for an integer and a float equality shows an index access path. It also proves
// the differential above is not vacuous — the two arms really do plan
// differently.
func TestNumericEqualitySeek_PlanUsesIndex(t *testing.T) {
	indexed := seedNumericSeekGraph(t, true)
	plain := seedNumericSeekGraph(t, false)

	for _, tc := range numericSeekCases {
		if !tc.wantSeek {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			withIdx, err := indexed.Explain(tc.query, nil)
			if err != nil {
				t.Fatalf("Explain(%q) on indexed graph: %v", tc.query, err)
			}
			if !strings.Contains(withIdx, "NodeByIndexRangeScan") {
				t.Fatalf("Explain(%q) on indexed graph shows no index access path:\n%s", tc.query, withIdx)
			}

			withoutIdx, err := plain.Explain(tc.query, nil)
			if err != nil {
				t.Fatalf("Explain(%q) on plain graph: %v", tc.query, err)
			}
			if strings.Contains(withoutIdx, "NodeByIndexRangeScan") {
				t.Fatalf("Explain(%q) on the UNINDEXED graph shows an index access path; "+
					"the differential is comparing two identical plans:\n%s", tc.query, withoutIdx)
			}
		})
	}
}

// TestNumericEqualitySeek_ParameterisedSeek covers the $param operand form,
// which is the shape a driver actually sends.
func TestNumericEqualitySeek_ParameterisedSeek(t *testing.T) {
	indexed := seedNumericSeekGraph(t, true)
	plain := seedNumericSeekGraph(t, false)

	const query = `MATCH (a:P) WHERE a.v = $v RETURN a.tag AS tag`
	for _, tc := range []struct {
		name string
		// raw is what a driver sends (RunAny binds it); boxed is the same value
		// in the planner's own representation, which Explain takes directly.
		raw   any
		boxed expr.Value
		want  []string
	}{
		{"int_param", int64(1999), expr.IntegerValue(1999), []string{"i1999"}},
		{"float_param", 42.5, expr.FloatValue(42.5), []string{"f42"}},
		{"crosstype_int_param", int64(5), expr.IntegerValue(5), []string{"float5", "i5"}},
		{"pow53_plus_one_param", int64(9007199254740993), expr.IntegerValue(9007199254740993),
			[]string{"p53plus1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{"v": tc.raw}
			gotIndexed := tagsFor(t, indexed, query, params)
			gotPlain := tagsFor(t, plain, query, params)
			if !equalStrings(gotIndexed, gotPlain) {
				t.Fatalf("index arm returned %v, scan arm returned %v", gotIndexed, gotPlain)
			}
			if !equalStrings(gotIndexed, tc.want) {
				t.Fatalf("returned %v, want %v", gotIndexed, tc.want)
			}

			plan, err := indexed.Explain(query, map[string]expr.Value{"v": tc.boxed})
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if !strings.Contains(plan, "NodeByIndexRangeScan") {
				t.Fatalf("parameterised equality did not plan an index access path:\n%s", plan)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
