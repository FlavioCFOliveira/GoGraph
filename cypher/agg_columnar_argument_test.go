package cypher_test

// agg_columnar_argument_test.go — correctness gate for the columnar (unboxed)
// aggregate-ARGUMENT filler and for group-key-free (global) columnar aggregation
// (#2185).
//
// Before #2185 the aggregation pre-projection filled its grouping key unboxed but
// every aggregate argument through the boxed row evaluator, and declined outright
// when there was no grouping key. #2185 gives a `node.prop` argument over a
// NodeID-emitting child the same unboxed filler the key already used, and admits the
// group-key-free shape (which forms exactly one group).
//
// The correctness argument is byte-identity with the row-at-a-time path, and these
// tests PIN it differentially: each case runs the same aggregation twice — once in
// the shape the columnar builder accepts (`min(n.v)`), once in a shape it rejects
// (`min(coalesce(n.v, n.v))`, whose argument is neither a bare variable nor a
// property access, so aggArgBudgetSafe declines the WHOLE aggregation to the row
// path) — and asserts identical results. coalesce(x, x) is an exact identity for
// every value including NULL, so the two arms differ only in physical path.
//
// The cases cover every branch of the filler's classification: plain int64/float64/
// bool/string land in the chunk's typed column unboxed; an absent property, a
// temporal, a list and a byte string route through the canonical lpgPropToExpr
// boxing; and mixed int/float inputs exercise sum's exact-int64-then-promote rule
// (CIP2016-06-14) and min/max's total ordering.
//
// Layer: short.

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runAggRows runs query and returns one line per result row listing every column in
// cols, sorted so row order is irrelevant.
func runAggRows(t *testing.T, g *lpg.Graph[string, float64], query string, cols ...string) []string {
	t.Helper()
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer res.Close()
	var out []string
	for res.Next() {
		rec := res.Record()
		line := ""
		for _, c := range cols {
			line += fmt.Sprintf("%s=%v ", c, rec[c])
		}
		out = append(out, line)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", query, err)
	}
	sort.Strings(out)
	return out
}

// assertArgColumnarMatchesFallback asserts that an aggregation whose argument is the
// bare property `n.<prop>` (the columnar-eligible shape) returns exactly what the
// same aggregation returns with the argument wrapped in coalesce(x, x) — an exact
// identity that forces the whole aggregation onto the boxed row path.
//
// aggTemplate must contain exactly one %s placeholder for the argument expression,
// e.g. "min(%s) AS m". groupClause is prepended verbatim when non-empty (e.g.
// "n.g AS g, ") and its columns must be listed first in cols.
func assertArgColumnarMatchesFallback(
	t *testing.T,
	g *lpg.Graph[string, float64],
	prop, groupClause, aggTemplate string,
	cols ...string,
) []string {
	t.Helper()
	arg := "n." + prop
	columnar := runAggRows(t, g,
		"MATCH (n) RETURN "+groupClause+fmt.Sprintf(aggTemplate, arg), cols...)
	fallback := runAggRows(t, g,
		"MATCH (n) RETURN "+groupClause+fmt.Sprintf(aggTemplate, "coalesce("+arg+", "+arg+")"), cols...)
	if fmt.Sprint(columnar) != fmt.Sprint(fallback) {
		t.Fatalf("columnar != row fallback for %q\n  columnar: %v\n  fallback: %v",
			fmt.Sprintf(aggTemplate, arg), columnar, fallback)
	}
	return columnar
}

// pv wraps a property value for gArg. [lpg.PropertyValue] is a struct, so "absent"
// cannot be spelt as a nil value — absent is spelt as a nil *lpg.PropertyValue,
// produced by [absent].
func pv(v lpg.PropertyValue) *lpg.PropertyValue { return &v }

// absent marks a node that carries NO "v" property, so the aggregate argument
// evaluates to NULL on that row.
func absent() *lpg.PropertyValue { return nil }

// gArg builds a graph whose i-th node carries grouping property "g" (i % groups) and
// argument property "v" set to vals[i]. An [absent] entry leaves "v" unset on that
// node, which is how the NULL-argument branch is exercised.
func gArg(t *testing.T, groups int, vals ...*lpg.PropertyValue) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i, v := range vals {
		k := fmt.Sprintf("n%03d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Int64Value(int64(i%groups))); err != nil {
			t.Fatalf("SetNodeProperty g: %v", err)
		}
		if v == nil {
			continue // absent "v" → NULL argument
		}
		if err := g.SetNodeProperty(k, "v", *v); err != nil {
			t.Fatalf("SetNodeProperty v: %v", err)
		}
	}
	return g
}

// intList builds a PropList of the given integers.
func intList(xs ...int64) *lpg.PropertyValue {
	elems := make([]lpg.PropertyValue, len(xs))
	for i, x := range xs {
		elems[i] = lpg.Int64Value(x)
	}
	return pv(lpg.ListValue(elems))
}

// argValueKinds enumerates the property kinds the unboxed filler must classify. Each
// entry is a graph whose "v" values are all of one shape, plus a name for the subtest.
func argValueKinds(t *testing.T) []struct {
	name string
	g    *lpg.Graph[string, float64]
} {
	t.Helper()
	day := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	return []struct {
		name string
		g    *lpg.Graph[string, float64]
	}{
		{"int64", gArg(t, 3, pv(lpg.Int64Value(7)), pv(lpg.Int64Value(-3)), pv(lpg.Int64Value(0)), pv(lpg.Int64Value(1<<53)), pv(lpg.Int64Value(42)))},
		{"float64", gArg(t, 3, pv(lpg.Float64Value(1.5)), pv(lpg.Float64Value(-0.5)), pv(lpg.Float64Value(0)), pv(lpg.Float64Value(1e300)), pv(lpg.Float64Value(2.25)))},
		{"mixed int and float", gArg(t, 3, pv(lpg.Int64Value(1)), pv(lpg.Float64Value(1.0)), pv(lpg.Int64Value(2)), pv(lpg.Float64Value(2.5)), pv(lpg.Int64Value(-4)))},
		{"bool", gArg(t, 2, pv(lpg.BoolValue(true)), pv(lpg.BoolValue(false)), pv(lpg.BoolValue(true)))},
		{"string", gArg(t, 2, pv(lpg.StringValue("b")), pv(lpg.StringValue("a")), pv(lpg.StringValue("")), pv(lpg.StringValue("c")))},
		{"all NULL (property absent)", gArg(t, 2, absent(), absent(), absent(), absent())},
		{"some NULL", gArg(t, 3, pv(lpg.Int64Value(5)), absent(), pv(lpg.Int64Value(-1)), absent(), pv(lpg.Float64Value(2.5)))},
		{"temporal", gArg(t, 2, pv(lpg.TimeValue(day)), pv(lpg.TimeValue(day.Add(48*time.Hour))), pv(lpg.TimeValue(day.Add(-24*time.Hour))))},
		{"date", gArg(t, 2, pv(lpg.DateValue(day)), pv(lpg.DateValue(day.Add(48*time.Hour))), pv(lpg.DateValue(day.Add(-24*time.Hour))))},
		{"list", gArg(t, 2, intList(1, 2), intList(3), intList())},
		{"bytes", gArg(t, 2, pv(lpg.BytesValue([]byte{1, 2})), pv(lpg.BytesValue([]byte{3})), pv(lpg.BytesValue(nil)))},
		{"heterogeneous", gArg(t, 3, pv(lpg.Int64Value(1)), pv(lpg.StringValue("s")), pv(lpg.BoolValue(true)), pv(lpg.Float64Value(2.5)), absent())},
	}
}

// argAggregates enumerates the aggregations whose argument column the filler feeds:
// the five SoA kernels, the boxed kernel (collect/stDev/percentile), and the
// DISTINCT-wrapped variants which route through the boxed kernel too.
var argAggregates = []struct {
	name     string
	template string
	col      string
}{
	{"min", "min(%s) AS a", "a"},
	{"max", "max(%s) AS a", "a"},
	{"sum", "sum(%s) AS a", "a"},
	{"avg", "avg(%s) AS a", "a"},
	{"count", "count(%s) AS a", "a"},
	{"collect", "collect(%s) AS a", "a"},
	{"stDev", "stDev(%s) AS a", "a"},
	{"percentileCont", "percentileCont(%s, 0.5) AS a", "a"},
	{"count distinct", "count(DISTINCT %s) AS a", "a"},
	{"sum distinct", "sum(DISTINCT %s) AS a", "a"},
	{"collect distinct", "collect(DISTINCT %s) AS a", "a"},
}

// TestColumnarAggArgument_GroupedDifferential pins byte-identity between the unboxed
// argument filler and the boxed row path for every (value kind × aggregate) pair,
// grouped by a second property.
func TestColumnarAggArgument_GroupedDifferential(t *testing.T) {
	t.Parallel()
	for _, kind := range argValueKinds(t) {
		for _, agg := range argAggregates {
			t.Run(kind.name+"/"+agg.name, func(t *testing.T) {
				assertArgColumnarMatchesFallback(t, kind.g, "v", "n.g AS g, ", agg.template, "g", agg.col)
			})
		}
	}
}

// TestColumnarAggArgument_GlobalDifferential is the same matrix without a grouping
// key — the shape tryBuildColumnarAggInput declined outright before #2185.
func TestColumnarAggArgument_GlobalDifferential(t *testing.T) {
	t.Parallel()
	for _, kind := range argValueKinds(t) {
		for _, agg := range argAggregates {
			t.Run(kind.name+"/"+agg.name, func(t *testing.T) {
				assertArgColumnarMatchesFallback(t, kind.g, "v", "", agg.template, agg.col)
			})
		}
	}
}

// TestColumnarAggArgument_MultipleAggregates pins a projection carrying several
// aggregates at once, so the per-slot argument column layout (keyCols then one column
// per aggregate) is exercised with a mix of unboxed and boxed fillers side by side.
func TestColumnarAggArgument_MultipleAggregates(t *testing.T) {
	t.Parallel()
	g := gArg(t, 3, pv(lpg.Int64Value(1)), pv(lpg.Float64Value(2.5)), absent(), pv(lpg.Int64Value(-7)), pv(lpg.Int64Value(1<<53)), pv(lpg.Float64Value(0.5)))

	grouped := runAggRows(t, g,
		"MATCH (n) RETURN n.g AS g, count(*) AS c, min(n.v) AS mn, max(n.v) AS mx, sum(n.v) AS s, avg(n.v) AS av, collect(n.v) AS co",
		"g", "c", "mn", "mx", "s", "av", "co")
	fallback := runAggRows(t, g,
		"MATCH (n) RETURN n.g AS g, count(*) AS c, min(coalesce(n.v, n.v)) AS mn, max(coalesce(n.v, n.v)) AS mx, sum(coalesce(n.v, n.v)) AS s, avg(coalesce(n.v, n.v)) AS av, collect(coalesce(n.v, n.v)) AS co",
		"g", "c", "mn", "mx", "s", "av", "co")
	if fmt.Sprint(grouped) != fmt.Sprint(fallback) {
		t.Fatalf("multi-aggregate columnar != fallback\n  columnar: %v\n  fallback: %v", grouped, fallback)
	}

	global := runAggRows(t, g,
		"MATCH (n) RETURN count(*) AS c, min(n.v) AS mn, max(n.v) AS mx, sum(n.v) AS s, avg(n.v) AS av",
		"c", "mn", "mx", "s", "av")
	globalFallback := runAggRows(t, g,
		"MATCH (n) RETURN count(*) AS c, min(coalesce(n.v, n.v)) AS mn, max(coalesce(n.v, n.v)) AS mx, sum(coalesce(n.v, n.v)) AS s, avg(coalesce(n.v, n.v)) AS av",
		"c", "mn", "mx", "s", "av")
	if fmt.Sprint(global) != fmt.Sprint(globalFallback) {
		t.Fatalf("multi-aggregate global columnar != fallback\n  columnar: %v\n  fallback: %v", global, globalFallback)
	}
}

// TestColumnarAggArgument_ExactInt64Sum pins CIP2016-06-14 exact integer SUM through
// the unboxed argument column: a sum of large int64 values must stay an exact INTEGER,
// never round through float64.
func TestColumnarAggArgument_ExactInt64Sum(t *testing.T) {
	t.Parallel()
	// Three values whose exact sum needs more than float64's 53-bit mantissa.
	const big = int64(1) << 53
	g := gArg(t, 1, pv(lpg.Int64Value(big)), pv(lpg.Int64Value(1)), pv(lpg.Int64Value(1)))
	got := assertArgColumnarMatchesFallback(t, g, "v", "", "sum(%s) AS a", "a")
	want := []string{fmt.Sprintf("a=%d ", big+2)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("exact int64 sum: got %v, want %v (a float64 round-trip would give %d)",
			got, want, int64(float64(big+2)))
	}
}

// TestColumnarAggArgument_EmptyInput pins the group-key-free neutral row: a global
// aggregate over an empty match must still emit exactly one row, with each
// aggregate's neutral value. This is the case the chunk consume phase now synthesises
// itself, and it must agree with the row path and with GlobalAggregateAdapter.
func TestColumnarAggArgument_EmptyInput(t *testing.T) {
	t.Parallel()
	g := gArg(t, 1, pv(lpg.Int64Value(1)), pv(lpg.Int64Value(2)))

	// A label no node carries makes the input empty while keeping the plan shape.
	for _, tc := range []struct {
		agg  string
		want string
	}{
		{"count(n.v) AS a", "a=0 "},
		{"count(*) AS a", "a=0 "},
		{"sum(n.v) AS a", "a=0 "},
		{"min(n.v) AS a", "a=<null> "},
		{"max(n.v) AS a", "a=<null> "},
		{"avg(n.v) AS a", "a=<null> "},
		{"collect(n.v) AS a", "a=[] "},
	} {
		t.Run(tc.agg, func(t *testing.T) {
			got := runAggRows(t, g, "MATCH (n:NoSuchLabel) RETURN "+tc.agg, "a")
			if len(got) != 1 {
				t.Fatalf("global aggregate over empty input must emit exactly 1 row, got %d: %v", len(got), got)
			}
			fallback := runAggRows(t, g,
				"MATCH (n:NoSuchLabel) RETURN "+replaceArg(tc.agg), "a")
			if fmt.Sprint(got) != fmt.Sprint(fallback) {
				t.Fatalf("empty-input columnar != fallback: %v vs %v", got, fallback)
			}
		})
	}
}

// replaceArg rewrites `n.v` to the coalesce identity so the aggregation falls onto the
// boxed row path, leaving count(*) (which has no argument) untouched.
func replaceArg(agg string) string {
	const from = "n.v"
	const to = "coalesce(n.v, n.v)"
	out := ""
	for {
		i := indexOf(agg, from)
		if i < 0 {
			return out + agg
		}
		out += agg[:i] + to
		agg = agg[i+len(from):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestColumnarAggArgument_CrossBatchTypeChange forces the dynamic argument column to
// commit int64 in the first FillChunk batch and float64 in a later one, so the
// kernels' int-then-promote handling is exercised across the batch boundary — the
// argument-column counterpart of TestColumnarAggGrouping_CrossBatchTypeChange.
func TestColumnarAggArgument_CrossBatchTypeChange(t *testing.T) {
	t.Parallel()
	const per = 4200 // > DefaultChunkCapacity (4096), so at least two batches
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < per; i++ {
		k := fmt.Sprintf("i%05d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Int64Value(0)); err != nil {
			t.Fatalf("SetNodeProperty g: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty v: %v", err)
		}
	}
	for i := 0; i < per; i++ {
		k := fmt.Sprintf("f%05d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeProperty(k, "g", lpg.Int64Value(0)); err != nil {
			t.Fatalf("SetNodeProperty g: %v", err)
		}
		if err := g.SetNodeProperty(k, "v", lpg.Float64Value(float64(i)+0.5)); err != nil {
			t.Fatalf("SetNodeProperty v: %v", err)
		}
	}
	for _, agg := range []string{"min(%s) AS a", "max(%s) AS a", "sum(%s) AS a", "avg(%s) AS a", "count(%s) AS a"} {
		t.Run(agg, func(t *testing.T) {
			assertArgColumnarMatchesFallback(t, g, "v", "n.g AS g, ", agg, "g", "a")
			assertArgColumnarMatchesFallback(t, g, "v", "", agg, "a")
		})
	}
}

// TestColumnarAggArgument_RelationshipPropertyArgument pins the guard that keeps the
// unboxed filler node-only: an aggregate argument reading a RELATIONSHIP property
// must still produce the row path's result. aggNodePropertyItem declines a non-node
// receiver, so the aggregation falls back — this test proves the fallback is correct
// rather than merely taken.
func TestColumnarAggArgument_RelationshipPropertyArgument(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < 4; i++ {
		if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := g.AddEdge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1), 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		if err := g.SetEdgeProperty(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1), "w", lpg.Int64Value(int64(i*10))); err != nil {
			t.Fatalf("SetEdgeProperty: %v", err)
		}
	}
	got := runAggRows(t, g, "MATCH (a)-[r]->(b) RETURN sum(r.w) AS s, min(r.w) AS mn, count(r.w) AS c", "s", "mn", "c")
	want := []string{"s=30 mn=0 c=3 "}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("relationship-property aggregate: got %v, want %v", got, want)
	}
}
