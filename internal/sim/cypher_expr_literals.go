package sim

import (
	"context"
	"fmt"
	"sort"
)

// exprLiteralCase is one self-contained expression probe: a graph-independent
// query whose result is a pure function of the query text, and the expected
// canonical col-0 rendering(s) — the sorted multiset of expr.Value.String()
// across every returned row. The expected values were captured empirically from
// the engine, so the check is an ABSOLUTE oracle (the answer is a known
// constant), not a differential.
type exprLiteralCase struct {
	query string
	want  []string // sorted col-0 String() values across all rows
}

// exprLiteralCases is the battery of graph-independent expression probes closing
// the DST's read-expression coverage gap: UNION / UNION ALL (CY4); CASE, list
// comprehension, reduce and the all/any/none/single quantifiers (CY9); the
// scalar/list/string/math function surface (CY11); list subscript, list slice
// and map projection (CY12); and temporal constructors + component access +
// duration (CY15). Each expected value is the exact canonical rendering the
// engine produces, verified empirically.
var exprLiteralCases = []exprLiteralCase{
	// CY4 — UNION / UNION ALL.
	{"RETURN 1 AS x UNION RETURN 2 AS x", []string{"1", "2"}},
	{"UNWIND [1,2,2] AS x RETURN x UNION UNWIND [2,3] AS y RETURN y", []string{"1", "2", "3"}}, // dedup
	{"UNWIND [1,1] AS x RETURN x UNION ALL UNWIND [1] AS y RETURN y", []string{"1", "1", "1"}}, // keeps dups

	// CY9 — CASE, comprehensions, reduce, quantifiers.
	{"RETURN CASE WHEN true THEN 'a' ELSE 'b' END", []string{`"a"`}},
	{"RETURN CASE 2 WHEN 1 THEN 'x' WHEN 2 THEN 'y' ELSE 'z' END", []string{`"y"`}},
	{"RETURN [x IN range(1,5) WHERE x % 2 = 0 | x*10]", []string{"[20, 40]"}},
	{"RETURN reduce(s=0, x IN range(1,4) | s + x)", []string{"10"}},
	{"RETURN any(x IN [1,2,3] WHERE x > 2)", []string{"true"}},
	{"RETURN all(x IN [2,4,6] WHERE x % 2 = 0)", []string{"true"}},
	{"RETURN none(x IN [1,3] WHERE x % 2 = 0)", []string{"true"}},
	{"RETURN single(x IN [1,2,3] WHERE x = 2)", []string{"true"}},

	// CY11 — scalar / list / string / math functions.
	{"RETURN abs(-3)", []string{"3"}},
	{"RETURN toInteger('42')", []string{"42"}},
	{"RETURN toFloat('1.5')", []string{"1.5"}},
	{"RETURN toString(42)", []string{`"42"`}},
	{"RETURN substring('hello', 1, 3)", []string{`"ell"`}},
	{"RETURN size([1,2,3])", []string{"3"}},
	{"RETURN coalesce(null, 7)", []string{"7"}},
	{"RETURN head([9,8,7])", []string{"9"}},
	{"RETURN last([9,8,7])", []string{"7"}},
	{"RETURN toUpper('hi')", []string{`"HI"`}},
	{"RETURN toLower('HI')", []string{`"hi"`}},
	{"RETURN split('a,b,c', ',')", []string{`["a", "b", "c"]`}},
	{"RETURN reverse([1,2,3])", []string{"[3, 2, 1]"}},
	{"RETURN trim('  hi  ')", []string{`"hi"`}},
	{"RETURN ceil(1.2)", []string{"2"}},
	{"RETURN floor(1.8)", []string{"1"}},
	{"RETURN round(2.5)", []string{"3"}},
	{"RETURN sqrt(9.0)", []string{"3"}},
	{"RETURN sign(-5)", []string{"-1"}},
	{"RETURN tail([1,2,3])", []string{"[2, 3]"}},
	{"RETURN 'x' IN ['x','y']", []string{"true"}},

	// CY12 — subscript, slice, map projection.
	{"RETURN [10,20,30][1]", []string{"20"}},
	{"RETURN range(1,10)[2..5]", []string{"[3, 4, 5]"}},
	{"RETURN {a:1,b:2}.a", []string{"1"}},
	{"RETURN {a:1,b:2}.b", []string{"2"}},
	// Map projection: compare the projected key count (order-independent) so the
	// non-deterministic map String() rendering never flakes.
	{"WITH {a:1,b:2,c:3} AS m RETURN size(keys(m{.a,.b}))", []string{"2"}},

	// CY15 — temporal constructors, component access, duration.
	{"RETURN date('2026-07-13')", []string{"2026-07-13"}},
	{"RETURN date('2026-07-13').year", []string{"2026"}},
	{"RETURN duration({days:2}).days", []string{"2"}},
}

// CheckExprLiterals runs the graph-independent expression battery and asserts
// each query's canonical result multiset equals its known-constant expectation.
// It needs no oracle (the answers are pure functions of the query text) and is
// run inside the cypher-surface scenario — periodically, after each
// crash/recovery (proving the recovered engine still evaluates every expression
// shape), and at the end.
func CheckExprLiterals(tick int64, engine *EngineAdapter) []Violation {
	ctx := context.Background()
	var vs []Violation
	for _, c := range exprLiteralCases {
		res, err := engine.Run(ctx, c.query, nil)
		if err != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "expr literal",
				Message: fmt.Sprintf("%q: query error: %v", c.query, err)})
			continue
		}
		var got []string
		for res.Next() {
			got = append(got, res.(*resultAdapter).res.ValueAt(0).String())
		}
		derr := res.Err()
		_ = res.Close()
		if derr != nil {
			vs = append(vs, Violation{Kind: ViolationGraphIntegrity, Tick: tick, Op: "expr literal",
				Message: fmt.Sprintf("%q: drain error: %v", c.query, derr)})
			continue
		}
		sort.Strings(got)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if !equalStrings(got, want) {
			vs = append(vs, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "expr literal",
				Message: fmt.Sprintf("%q: engine=%v, want=%v", c.query, got, want)})
		}
	}
	return vs
}

// equalStrings reports whether two string slices are element-wise equal.
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
