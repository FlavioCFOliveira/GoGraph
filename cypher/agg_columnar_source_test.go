package cypher

// agg_columnar_source_test.go — regression tests for rmp #2655: the columnar read
// chain under an EagerAggregation, and the per-row byte guard the columnar
// aggregation pre-projection was missing.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// aggSourceQueries are the aggregate shapes whose read subtree the #2655
// recogniser must claim. Each names the operator chain the plan MUST contain.
var aggSourceQueries = []struct {
	name string
	q    string
	want []string
}{
	// count(<bare pattern-bound var>) is normalised to count(*) before any recogniser
	// runs (rmp #2657), so this shape now reaches exec.CountRows and needs no
	// pre-projection at all — the columnar chain still has to reach under the
	// aggregate, which is what this table is for.
	{"count_var", `MATCH (n:P) WHERE n.age > 10 RETURN count(n) AS c`,
		[]string{"CountRows", "ColumnarFilter"}},
	{"count_star", `MATCH (n:P) WHERE n.age > 10 RETURN count(*) AS c`,
		[]string{"CountRows", "ColumnarFilter"}},
	{"count_prop", `MATCH (n:P) WHERE n.age > 10 RETURN count(n.age) AS c`,
		[]string{"ColumnarProject", "ColumnarFilter"}},
	{"grouped_count_star", `MATCH (n:P) WHERE n.age > 10 RETURN n.name AS nm, count(*) AS c`,
		[]string{"ColumnarProject", "ColumnarFilter"}},
	{"grouped_count_prop", `MATCH (n:P) WHERE n.age > 10 RETURN n.name AS nm, count(n.age) AS c`,
		[]string{"ColumnarProject", "ColumnarFilter"}},
	{"minmax", `MATCH (n:P) WHERE n.age > 10 RETURN min(n.age) AS lo, max(n.age) AS hi`,
		[]string{"ColumnarProject", "ColumnarFilter"}},
	{"sumavg", `MATCH (n:P) WHERE n.age > 10 RETURN sum(n.big) AS s, avg(n.score) AS a`,
		[]string{"ColumnarProject", "ColumnarFilter"}},
	{"expand_count_star", `MATCH (a:P)-[:K]->(b:P) WHERE b.age > 10 RETURN count(*) AS c`,
		[]string{"CountRows", "ColumnarFilter", "columnarExpand"}},
	// Likewise normalised by #2657: b is bound by a non-optional ir.Expand.
	{"expand_count_var", `MATCH (a:P)-[:K]->(b:P) WHERE b.age > 10 RETURN count(b) AS c`,
		[]string{"CountRows", "ColumnarFilter", "columnarExpand"}},
	{"expand_grouped", `MATCH (a:P)-[:K]->(b:P) WHERE b.age > 10 RETURN b.name AS nm, count(*) AS c`,
		[]string{"ColumnarProject", "ColumnarFilter", "columnarExpand"}},
}

// TestAggColumnarSource_PlansColumnarUnderAggregate pins the defect #2655 closed:
// an aggregate with a WHERE built a row exec.Filter, which is not a ChunkProducer,
// so the columnar pre-projection declined and the whole aggregate ran fully boxed.
// The columnar chain was structurally UNREACHABLE from under an aggregate.
func TestAggColumnarSource_PlansColumnarUnderAggregate(t *testing.T) {
	t.Parallel()
	g := profileCorpusGraph(t)
	eng := NewEngine(g)
	for _, tc := range aggSourceQueries {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			plan := explainOK(t, eng, tc.q)
			for _, op := range tc.want {
				if !strings.Contains(plan, op) {
					t.Fatalf("plan for %q is missing %s — the columnar chain did not "+
						"reach under the aggregate:\n%s", tc.q, op, plan)
				}
			}
			if strings.Contains(plan, "\nFilter") || strings.Contains(plan, "─ Filter") {
				t.Fatalf("plan for %q still carries a row Filter:\n%s", tc.q, plan)
			}
		})
	}
}

// TestAggColumnarSource_DifferentialAgainstRowBuild is the correctness gate: with
// the post-build decline seam set, every shape falls back to the byte-identical
// serial build, so the two arms must agree row for row. It covers the grouped,
// DISTINCT and NULL-carrying forms explicitly, because those are where a
// column-major group key or an unboxed argument could diverge from the boxed path.
func TestAggColumnarSource_DifferentialAgainstRowBuild(t *testing.T) {
	t.Parallel()
	g := aggDiffGraph(t)
	queries := []string{
		// The recognised shapes.
		`MATCH (n:P) WHERE n.age > 10 RETURN count(n) AS c`,
		`MATCH (n:P) WHERE n.age > 10 RETURN count(*) AS c`,
		`MATCH (n:P) WHERE n.age > 10 RETURN count(n.opt) AS c`,
		`MATCH (n:P) WHERE n.age > 10 RETURN n.grp AS g, count(*) AS c ORDER BY g`,
		`MATCH (n:P) WHERE n.age > 10 RETURN n.grp AS g, count(n.opt) AS c ORDER BY g`,
		`MATCH (n:P) WHERE n.age > 10 RETURN n.opt AS o, count(*) AS c ORDER BY o`,
		// NULL semantics: opt is absent on some nodes, so it groups as NULL and
		// count(n.opt) must skip it.
		`MATCH (n:P) WHERE n.age >= 0 RETURN n.opt AS o, count(n.opt) AS c ORDER BY o`,
		// DISTINCT, on a property and on a bare node variable.
		`MATCH (n:P) WHERE n.age > 10 RETURN count(DISTINCT n.grp) AS c`,
		`MATCH (n:P) WHERE n.age > 10 RETURN count(DISTINCT n) AS c`,
		`MATCH (n:P) WHERE n.age > 10 RETURN n.grp AS g, count(DISTINCT n.opt) AS c ORDER BY g`,
		// Every other aggregate kernel over the same source.
		`MATCH (n:P) WHERE n.age > 10 RETURN min(n.age) AS lo, max(n.age) AS hi`,
		`MATCH (n:P) WHERE n.age > 10 RETURN min(n.opt) AS lo, max(n.opt) AS hi`,
		`MATCH (n:P) WHERE n.age > 10 RETURN sum(n.age) AS s, avg(n.score) AS a`,
		`MATCH (n:P) WHERE n.age > 10 RETURN collect(n.grp) AS cs`,
		// A bare node variable as a grouping key and as a min/max argument: the
		// count(bare var) chunk filler MUST NOT be reachable from these.
		`MATCH (n:P) WHERE n.age > 38 RETURN n AS who, count(*) AS c ORDER BY who`,
		`MATCH (n:P) WHERE n.age > 10 RETURN min(n) AS lo, max(n) AS hi`,
		`MATCH (n:P) WHERE n.age > 10 RETURN count(n) AS c, min(n.age) AS lo`,
		// An aggregate argument the columnar PRE-PROJECTION declines (a computed
		// expression is not budget-safe): the row exec.Project is then stacked over
		// the ColumnarFilter, which must behave exactly as the exec.Filter it replaces.
		`MATCH (n:P) WHERE n.age > 10 RETURN sum(n.age * 2) AS s`,
		`MATCH (n:P) WHERE n.age > 10 RETURN n.grp AS g, count(n.age + 1) AS c ORDER BY g`,
		// Mixed-type property: the unboxed fast path must hand these to the boxed
		// fallback and still agree.
		`MATCH (n:P) WHERE n.age > 10 RETURN n.mixed AS m, count(*) AS c ORDER BY m`,
		// The Expand arm.
		`MATCH (a:P)-[:K]->(b:P) WHERE b.age > 10 RETURN count(*) AS c`,
		`MATCH (a:P)-[:K]->(b:P) WHERE b.age > 10 RETURN count(b) AS c`,
		`MATCH (a:P)-[:K]->(b:P) WHERE b.age > 10 RETURN b.grp AS g, count(*) AS c ORDER BY g`,
		`MATCH (a:P)-[:K]->(b:P) WHERE b.age > 10 RETURN min(b.age) AS lo, max(b.age) AS hi`,
		// An empty result: the group-key-free neutral row must still be synthesised.
		`MATCH (n:P) WHERE n.age > 9999 RETURN count(n) AS c`,
		`MATCH (n:P) WHERE n.age > 9999 RETURN count(*) AS c`,
		`MATCH (n:P) WHERE n.age > 9999 RETURN min(n.age) AS lo, sum(n.age) AS s`,
	}
	columnar := NewEngine(g)
	declined := NewEngine(g)
	declined.forceColumnarChainDeclineForTest = true
	for _, q := range queries {
		q := q
		t.Run(q, func(t *testing.T) {
			// POSITIVE CONTROL. A differential whose two arms compile to the same plan
			// compares a program with itself and cannot fail. Assert the arms really
			// are different programs before comparing their answers.
			colPlan := explainOK(t, columnar, q)
			rowPlan := explainOK(t, declined, q)
			if !strings.Contains(colPlan, "ColumnarFilter") {
				t.Fatalf("%q does not plan the columnar chain, so this arm is not the one "+
					"under test:\n%s", q, colPlan)
			}
			if strings.Contains(rowPlan, "ColumnarFilter") || strings.Contains(rowPlan, "columnarExpand") {
				t.Fatalf("the decline seam did not take effect for %q; both arms are the "+
					"columnar plan:\n%s", q, rowPlan)
			}
			want := runRows(t, declined, q)
			got := runRows(t, columnar, q)
			if len(want) == 0 {
				t.Fatalf("%q produced no rows in the row arm, so a regression would be invisible", q)
			}
			if !strings.EqualFold(strings.Join(sortedCopy(got), "\n"), strings.Join(sortedCopy(want), "\n")) {
				t.Fatalf("columnar and row arms disagree for %q\ncolumnar: %v\nrow:      %v\n"+
					"--- columnar plan ---\n%s\n--- row plan ---\n%s",
					q, got, want, explainOK(t, columnar, q), explainOK(t, declined, q))
			}
		})
	}
}

// TestAggColumnarSource_BareVarCountGate pins the one correctness gate the
// count(bare var) chunk filler carries: it hands the kernel a RAW NodeID column,
// which only [exec.countKernel] may see, because it reads nothing but the validity
// bitmap. Reaching minMaxKernel it would order NODE IDS and answer with the wrong
// node; reaching sumKernel it would add them up.
func TestAggColumnarSource_BareVarCountGate(t *testing.T) {
	t.Parallel()
	g := aggDiffGraph(t)
	eng := NewEngine(g)
	// min/max over a bare node variable must still return a NODE, never an integer.
	rows := runRows(t, eng, `MATCH (n:P) WHERE n.age > 10 RETURN min(n) AS lo`)
	if len(rows) != 1 {
		t.Fatalf("min(n) returned %d rows, want 1", len(rows))
	}
	if !strings.Contains(rows[0], "node") {
		t.Fatalf("min(n) returned %q — a raw NodeID column reached minMaxKernel", rows[0])
	}
	// count(DISTINCT n) dedups on the argument's VALUE, so it must not take the
	// raw-id filler either; the answer is the number of distinct nodes.
	distinct := runRows(t, eng, `MATCH (n:P) WHERE n.age > 10 RETURN count(DISTINCT n) AS c`)
	plain := runRows(t, eng, `MATCH (n:P) WHERE n.age > 10 RETURN count(n) AS c`)
	if len(distinct) != 1 || len(plain) != 1 || distinct[0] != plain[0] {
		t.Fatalf("count(DISTINCT n)=%v but count(n)=%v; every matched node is distinct", distinct, plain)
	}
}

// TestAggColumnarSource_YieldsToAccessPaths pins the declines carried over from the
// two working recognisers. Each of these plans removes ROWS; the columnar chain only
// removes a constant factor from each row, so it must never pre-empt one (#2204,
// #2077, #2133).
func TestAggColumnarSource_YieldsToAccessPaths(t *testing.T) {
	t.Parallel()
	g := profileCorpusGraph(t)

	t.Run("index_seek", func(t *testing.T) {
		eng := NewEngine(g)
		res, err := eng.RunAny(context.Background(), "CREATE INDEX FOR (p:P) ON (p.name)", nil)
		if err != nil {
			t.Fatalf("CREATE INDEX: %v", err)
		}
		if cerr := res.Close(); cerr != nil {
			t.Fatalf("CREATE INDEX close: %v", cerr)
		}
		const q = `MATCH (n:P) WHERE n.name = 'nm7' RETURN count(n) AS c`
		plan := explainOK(t, eng, q)
		if strings.Contains(plan, "ColumnarFilter") {
			t.Fatalf("the columnar chain pre-empted a covering index seek (#2204):\n%s", plan)
		}
		if rows := runRows(t, eng, q); len(rows) != 1 || rows[0] != "1" {
			t.Fatalf("seek plan answered %v, want [1]", rows)
		}
	})

	t.Run("min_label_reanchor", func(t *testing.T) {
		// Q is a third of P, so the re-anchor moves the scan to Q.
		eng := NewEngine(g)
		const q = `MATCH (n:P:Q) WHERE n.age > 10 RETURN count(n) AS c`
		plan := explainOK(t, eng, q)
		if strings.Contains(plan, "ColumnarFilter") {
			t.Fatalf("the columnar chain pre-empted the min-label re-anchor (#2077/#2133):\n%s", plan)
		}
		if !strings.Contains(plan, "[Q]") {
			t.Fatalf("expected the scan to be re-anchored on the smaller label Q:\n%s", plan)
		}
	})
}

// TestColumnarAggPreProjection_RowByteBudgetParity is the #2655 blocking
// precondition, as a regression test.
//
// buildEagerAggregation wrapped the ROW pre-projection in applyProjectionRowBudget
// and the COLUMNAR one in nothing, and the aggregation's own WithByteBudget does not
// subsume it: that charges the RETAINED GROUP KEYS once per NEW GROUP (#1841) and
// never charges an aggregate ARGUMENT column at all. MEASURED before the fix, with
// MaxResultBytes=100000 and a property whose estimate is 320016 bytes:
// `RETURN count(n.big)` SUCCEEDED on the columnar path while the identical query
// with a WHERE — which forced the row path — returned ErrProjectionRowTooLarge.
//
// Every triple below must now reach the SAME verdict, whichever pre-projection runs.
//
// EXTENDED for rmp #2668 with the THIRD pre-projection: the one
// tryBuildParallelAggregateScan's sub-plan factory rebuilds per morsel, which was
// also wrapped in nothing. #2655 could not have widened to it, because that path
// requires a BARE scan and so is excluded by the very WHERE the row arm needs. The
// third arm is therefore an UNLABELLED bare scan on an engine whose parallel
// threshold the fixture exceeds; the parallel path declining would make it a fourth
// serial arm, so [TestParallelAggPreProjection_ThreeSitesIdenticalVerdict] asserts
// engagement from the query's own physical plan and additionally pins the three error
// strings byte-identical.
//
// MEASURED with the per-worker guard unwired: 2 of the 3 new parallel arms go red. The
// third, `RETURN n.big AS b, count(*)`, still refuses without it, because the retained
// GROUPING KEY is charged by exec.ParallelAggregateScan.WithByteBudget; only the
// aggregate ARGUMENT arms depend on the new guard.
func TestColumnarAggPreProjection_RowByteBudgetParity(t *testing.T) {
	t.Parallel()
	const (
		// > psTestThreshold (50) so the third arm's unlabelled scan engages the
		// morsel-parallel aggregate path. The labelled arms are unaffected: their
		// engine keeps the default 50 000 threshold.
		nodes   = 60
		listLen = 20000 // 16 + 20000*16 = 320016 estimated bytes
		ceiling = 100000
	)
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < nodes; i++ {
		k := fmt.Sprintf("p%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(k, "tag", lpg.Int64Value(int64(i%2))); err != nil {
			t.Fatalf("SetNodeProperty tag: %v", err)
		}
		if err := g.SetNodeProperty(k, "small", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty small: %v", err)
		}
		elems := make([]lpg.PropertyValue, listLen)
		for j := range elems {
			elems[j] = lpg.Int64Value(int64(j))
		}
		elems[0] = lpg.Int64Value(int64(i)) // distinct per node, so grouping opens one group each
		if err := g.SetNodeProperty(k, "big", lpg.ListValue(elems)); err != nil {
			t.Fatalf("SetNodeProperty big: %v", err)
		}
	}
	eng := NewEngineWithOptions(g, EngineOptions{
		MaxResultBytes:       ceiling,
		GlobalMaxResultBytes: GlobalMaxResultBytesUnlimited,
	})
	// The third arm's engine: the same ceiling, plus a threshold the fixture exceeds
	// so the unlabelled bare scan reaches tryBuildParallelAggregateScan (#2668).
	parEng := NewEngineWithOptions(g, EngineOptions{
		MaxResultBytes:        ceiling,
		GlobalMaxResultBytes:  GlobalMaxResultBytesUnlimited,
		ParallelScanThreshold: psTestThreshold,
	})
	// Each triple is the SAME aggregate; the `tag` predicate exists only to move the
	// query from the columnar pre-projection to the row one, and dropping the label
	// only to move it to the per-worker one. No arm may complete.
	triples := [][3]string{
		{`MATCH (n:P) RETURN count(n.big) AS c`,
			`MATCH (n:P) WHERE n.tag < 9 RETURN count(n.big) AS c`,
			`MATCH (n) RETURN count(n.big) AS c`},
		{`MATCH (n:P) RETURN n.small AS s, count(n.big) AS c`,
			`MATCH (n:P) WHERE n.tag < 9 RETURN n.small AS s, count(n.big) AS c`,
			`MATCH (n) RETURN n.small AS s, count(n.big) AS c`},
		{`MATCH (n:P) RETURN n.big AS b, count(*) AS c`,
			`MATCH (n:P) WHERE n.tag < 9 RETURN n.big AS b, count(*) AS c`,
			`MATCH (n) RETURN n.big AS b, count(*) AS c`},
	}
	for _, triple := range triples {
		for i, q := range triple {
			q := q
			e := eng
			if i == 2 {
				e = parEng
			}
			t.Run(q, func(t *testing.T) {
				res, err := e.Run(context.Background(), q, nil)
				if err != nil {
					return // a build-time refusal is a refusal
				}
				for res.Next() {
				}
				drainErr := res.Err()
				_ = res.Close()
				if drainErr == nil {
					t.Fatalf("%q completed under a %d-byte result budget while carrying a "+
						"320016-byte value: the pre-projection is bounded by nothing.\n%s",
						q, ceiling, explainOK(t, e, q))
				}
			})
		}
	}
}

// aggDiffGraph is the differential fixture: it deliberately carries an ABSENT
// property (opt) so NULL grouping and NULL-skipping counts are exercised, and a
// HETEROGENEOUS property (mixed) so the unboxed comparison fast path has to hand
// rows to the boxed fallback.
func aggDiffGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	const n = 240
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		props := map[string]lpg.PropertyValue{
			"age":   lpg.Int64Value(int64(i % 40)),
			"grp":   lpg.StringValue(fmt.Sprintf("g%d", i%7)),
			"score": lpg.Float64Value(float64(i) / 3.0),
			"big":   lpg.Int64Value(int64(100_000 + i)),
		}
		if i%3 != 0 {
			props["opt"] = lpg.Int64Value(int64(i % 5))
		}
		if i%2 == 0 {
			props["mixed"] = lpg.Int64Value(int64(i % 11))
		} else {
			props["mixed"] = lpg.StringValue(fmt.Sprintf("s%d", i%11))
		}
		for key, v := range props {
			if err := g.SetNodeProperty(k, key, v); err != nil {
				t.Fatalf("SetNodeProperty %s: %v", key, err)
			}
		}
	}
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("n%d", i)
		for d := 1; d <= 3; d++ {
			dst := fmt.Sprintf("n%d", (i+d*11)%n)
			if err := g.AddEdge(src, dst, float64(d)); err != nil {
				t.Fatalf("AddEdge: %v", err)
			}
			g.SetEdgeLabel(src, dst, "K")
		}
	}
	return g
}

// sortedCopy returns a sorted copy so an unordered aggregate's group order cannot
// make two identical multisets compare unequal.
func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}
