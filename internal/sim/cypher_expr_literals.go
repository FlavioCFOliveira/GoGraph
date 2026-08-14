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

	// CY15b — the temporal function surface (rmp #2457). Every expectation below
	// is an ABSOLUTE constant captured from the engine, so a regression in the
	// truncation calendar arithmetic, the duration normalisation, the epoch
	// conversion, or the component accessors fails the run rather than being
	// compared against another engine-derived value.
	//
	// truncate family — date / datetime / localdatetime / localtime / time
	// (cypher/funcs/temporal.go registers all five).
	{"RETURN date.truncate('year', date('2026-07-13'))", []string{"2026-01-01"}},
	{"RETURN date.truncate('month', date('2026-07-13'))", []string{"2026-07-01"}},
	// 2026-07-13 is itself a Monday, so week-truncation is a fixed point.
	{"RETURN date.truncate('week', date('2026-07-13'))", []string{"2026-07-13"}},
	// The optional third argument overrides a component AFTER truncation.
	{"RETURN date.truncate('month', date('2026-07-13'), {day: 5})", []string{"2026-07-05"}},
	{"RETURN datetime.truncate('day', datetime('2026-07-13T14:35:47.123456789Z'))", []string{"2026-07-13T00:00Z"}},
	{"RETURN datetime.truncate('hour', datetime('2026-07-13T14:35:47Z'))", []string{"2026-07-13T14:00Z"}},
	{"RETURN localdatetime.truncate('minute', localdatetime('2026-07-13T14:35:47.5'))", []string{"2026-07-13T14:35"}},
	{"RETURN localdatetime.truncate('day', localdatetime('2026-07-13T14:35:47'))", []string{"2026-07-13T00:00"}},
	{"RETURN localtime.truncate('hour', localtime('14:35:47.25'))", []string{"14:00"}},
	{"RETURN localtime.truncate('second', localtime('14:35:47.25'))", []string{"14:35:47"}},
	{"RETURN time.truncate('minute', time('14:35:47.25Z'))", []string{"14:35Z"}},
	{"RETURN time.truncate('hour', time('14:35:47Z'))", []string{"14:00Z"}},

	// duration.between and the inDays / inMonths / inSeconds projections. The
	// two-argument forms compute the duration between two temporals; the
	// one-argument forms project an existing duration onto a single unit.
	{"RETURN duration.between(date('2026-01-01'), date('2026-03-15'))", []string{"P2M14D"}},
	{"RETURN duration.between(localdatetime('2026-01-01T00:00:00'), localdatetime('2026-01-02T03:04:05'))", []string{"P1DT3H4M5S"}},
	{"RETURN duration.inDays(date('2026-01-01'), date('2026-03-15'))", []string{"P73D"}},
	{"RETURN duration.inMonths(date('2026-01-01'), date('2026-03-15'))", []string{"P2M"}},
	{"RETURN duration.inSeconds(localtime('01:00:00'), localtime('02:30:00'))", []string{"PT1H30M"}},
	{"RETURN duration.inSeconds(duration('PT1H30M'))", []string{"PT1H30M"}},
	// The months stride is NOT rolled into days (it has no fixed day count).
	{"RETURN duration.inDays(duration('P1M40D'))", []string{"P40D"}},
	{"RETURN duration.inMonths(duration('P14M'))", []string{"P1Y2M"}},

	// Epoch constructors.
	{"RETURN datetime.fromepoch(1767225600, 0)", []string{"2026-01-01T00:00Z"}},
	{"RETURN datetime.fromepochmillis(1767225600123)", []string{"2026-01-01T00:00:00.123Z"}},

	// Component access on each temporal type.
	{"RETURN date('2026-07-13').month", []string{"7"}},
	{"RETURN date('2026-07-13').day", []string{"13"}},
	{"RETURN date('2026-07-13').week", []string{"29"}},
	{"RETURN date('2026-07-13').quarter", []string{"3"}},
	{"RETURN date('2026-07-13').dayOfWeek", []string{"1"}},
	{"RETURN localdatetime('2026-07-13T14:35:47.5').hour", []string{"14"}},
	{"RETURN localdatetime('2026-07-13T14:35:47.5').minute", []string{"35"}},
	{"RETURN localdatetime('2026-07-13T14:35:47.5').second", []string{"47"}},
	{"RETURN datetime('2026-07-13T14:35:47+02:00').offsetSeconds", []string{"7200"}},
	{"RETURN datetime('2026-07-13T14:35:47Z').epochSeconds", []string{"1783953347"}},
	{"RETURN datetime('2026-07-13T14:35:47.500Z').epochMillis", []string{"1783953347500"}},
	{"RETURN localtime('14:35:47.25').nanosecond", []string{"250000000"}},
	{"RETURN time('14:35:47Z').hour", []string{"14"}},
	// A duration's components are the CANONICAL ones: months rolls the years in
	// (1Y2M → 14), and seconds rolls the hours and minutes in.
	{"RETURN duration('P1Y2M3DT4H5M6S').years", []string{"1"}},
	{"RETURN duration('P1Y2M3DT4H5M6S').months", []string{"14"}},
	{"RETURN duration('P1Y2M3DT4H5M6S').days", []string{"3"}},
	{"RETURN duration('P1Y2M3DT4H5M6S').hours", []string{"4"}},
	{"RETURN duration('P1Y2M3DT4H5M6S').minutes", []string{"245"}},
	{"RETURN duration('P1Y2M3DT4H5M6S').seconds", []string{"14706"}},

	// Temporal arithmetic and comparison. Adding one month to 31 January
	// overflows the shorter month exactly as time.Date normalisation does.
	{"RETURN date('2026-01-31') + duration('P1M')", []string{"2026-03-03"}},
	{"RETURN date('2026-03-01') - duration('P1D')", []string{"2026-02-28"}},
	{"RETURN localdatetime('2026-01-01T00:00:00') + duration('PT90M')", []string{"2026-01-01T01:30"}},
	{"RETURN date('2026-01-01') < date('2026-01-02')", []string{"true"}},
	{"RETURN date('2026-01-01') = date('2026-01-01')", []string{"true"}},

	// Statement-`now` stability: openCypher requires every "now" constructor
	// within ONE statement to observe the SAME instant. The engine freezes it per
	// query in cypher/stmt_now_reg.go, so these must be true/PT0S regardless of
	// how long the statement takes; a per-call time.Now() would make them flap.
	{"RETURN datetime() = datetime()", []string{"true"}},
	{"RETURN date() = date()", []string{"true"}},
	{"RETURN localdatetime() = localdatetime()", []string{"true"}},
	{"RETURN duration.inSeconds(localtime(), localtime())", []string{"PT0S"}},
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
