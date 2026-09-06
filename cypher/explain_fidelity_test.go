package cypher_test

// explain_fidelity_test.go — rmp #2720.
//
// [cypher.Engine.explainPhysical] claims to build the physical operator tree
// "exactly as the read path builds it". Nothing tested that claim against a real
// EXECUTION: the existing gates compare EXPLAIN against a runtime counter for two
// specific substitutions (explain_physical_test.go), which proves the rendering
// names the right operator but not that the WHOLE tree matches.
//
// The oracle here is the plan a run actually built. PROFILE installs its
// instrumentation inside the same buildReadPhysical the un-prefixed read path
// uses and then executes, so the tree it captures is the tree that ran; EXPLAIN
// builds and throws away. Stripping the measurements off the profiled tree leaves
// a structure that must be identical to EXPLAIN's, operator for operator and
// detail for detail. It is not a comparison of two renderings: one side ran.
//
// Three conditions are covered because each could break the claim on its own:
//
//   - COLD, with an empty plan cache;
//   - WARM, on a plan-cache HIT, where EXPLAIN and the run read a cached
//     planCacheEntry rather than re-analysing;
//   - PARAMETERISED, where an access-path gate reads a parameter's VALUE. The
//     sprint's own history records a parameter that full-scanned where the
//     identical literal seeked, which is exactly the shape that would make EXPLAIN
//     lie about a run.
//
// The fourth test records the one case where EXPLAIN and the run genuinely
// DIVERGE, and pins it so it cannot be quietly forgotten.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// planShape renders a captured tree's STRUCTURE — operator names, their details
// and their nesting — with every measurement omitted, so a plan built by EXPLAIN
// and a plan captured from a real run are directly comparable.
func planShape(n *exec.PlanNode, depth int) string {
	var b strings.Builder
	b.WriteString(strings.Repeat("  ", depth))
	b.WriteString(n.Name)
	if n.Detail != "" {
		b.WriteString(" [" + n.Detail + "]")
	}
	b.WriteByte('\n')
	for i := range n.Children {
		b.WriteString(planShape(&n.Children[i], depth+1))
	}
	return b.String()
}

// explainShape returns the shape of the plan `EXPLAIN <query>` reports. EXPLAIN
// executes nothing, so this is a plan built and discarded.
func explainShape(t *testing.T, eng *cypher.Engine, query string, params map[string]expr.Value) string {
	t.Helper()
	r, err := eng.Run(context.Background(), "EXPLAIN "+query, params)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v", query, err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
	}
	if err := r.Err(); err != nil {
		t.Fatalf("EXPLAIN %s drain: %v", query, err)
	}
	p := r.Plan()
	if p == nil {
		t.Fatalf("EXPLAIN %s returned no plan", query)
	}
	return planShape(p, 0)
}

// executedShape returns the shape of the plan that ACTUALLY RAN, captured by
// PROFILE from the executed operator tree.
func executedShape(t *testing.T, eng *cypher.Engine, query string, params map[string]expr.Value) string {
	t.Helper()
	r, err := eng.Run(context.Background(), "PROFILE "+query, params)
	if err != nil {
		t.Fatalf("PROFILE %s: %v", query, err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
	}
	if err := r.Err(); err != nil {
		t.Fatalf("PROFILE %s drain: %v", query, err)
	}
	p := r.Profile()
	if p == nil {
		t.Fatalf("PROFILE %s returned no profile", query)
	}
	if !p.Profiled {
		t.Fatalf("PROFILE %s captured a tree with no measurements, so it did not "+
			"execute and cannot serve as the oracle", query)
	}
	return planShape(p, 0)
}

// seedFidelityGraph builds 300 :P nodes over 30 age buckets with an index on
// age, which is enough for the index-seek and join substitutions to fire.
func seedFidelityGraph(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	for i := 0; i < 300; i++ {
		runHonestyWrite(t, eng, fmt.Sprintf("CREATE (:P {age: %d, name: 'n%d'})", i%30, i))
	}
	runHonestyWrite(t, eng, "CREATE INDEX FOR (n:P) ON (n.age)")
	return eng
}

// statsReorderQuery is a disjoint two-component MATCH whose drive order the
// planner decides from a PROPERTY STATISTIC: the :A arm is filtered by a range
// predicate the equi-depth histogram estimates at ~98 of 5 000 rows, the :B arm is
// an MCV-exact single row, and promoting :B pays the 5 000-row scan once instead
// of 98 times. It is the shape rmp #2766 added and rmp #2771 had to make
// deterministic.
const statsReorderQuery = "MATCH (a:A) WHERE a.x > 4900 MATCH (b:B {y: 7}) " +
	"RETURN a.x AS ax, b.y AS bv"

// seedStatsReorderEngine builds the graph statsReorderQuery plans over: 5 000 :A
// nodes carrying a uniform integer the histogram summarises well, and 2 000 :B
// nodes with a distinct y so `b.y = 7` is an MCV-exact single row. Statistics are
// refreshed, because the reorder consults them and an engine that never refreshed
// has no collector at all.
//
// withLiveHistory decides whether MVCC version records are LIVE when the query is
// planned. That is not decoration: [lpg.Graph.LabelCountExact] declines the moment
// any node-life or label-delta record is unreclaimed, and the rendering surfaces
// and the execution build resolve their label counts through DIFFERENT resolvers —
// [cypher.Engine.ExplainLogical] and [cypher.Engine.ExplainTable] read present-time,
// where the count does not decline, while the build reads the query's pinned
// snapshot, where it does. Before rmp #2771 that made EXPLAIN render a reorder the
// engine would not perform.
//
// History is made live deterministically rather than hoped for: a reader is
// registered FIRST, which caps the reclamation watermark, and only then is a node
// written — so the record it pushes is newer than the watermark and the vacuum
// cannot free it while the reader is registered. The node carries a label no query
// here mentions, so it changes no result and no count the planner reads.
func seedStatsReorderEngine(t *testing.T, withLiveHistory bool) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	seedLabelled := func(prefix, label, prop string, n int) {
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("%s%d", prefix, i)
			if err := g.AddNode(key); err != nil {
				t.Fatal(err)
			}
			if err := g.SetNodeLabel(key, label); err != nil {
				t.Fatal(err)
			}
			if err := g.SetNodeProperty(key, prop, lpg.Int64Value(int64(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	seedLabelled("a", "A", "x", 5000)
	seedLabelled("b", "B", "y", 2000)

	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	if err := eng.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	if withLiveHistory {
		held := g.BeginRead()
		t.Cleanup(func() { g.EndRead(held) })
		if err := g.AddNode("zz-unrelated"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel("zz-unrelated", "ZZUnrelated"); err != nil {
			t.Fatal(err)
		}
		if n := g.NodeLifeVersionCount(); n == 0 {
			t.Fatal("no node-life record survived the write, so this engine is not in " +
				"the live-history state the test asked for")
		}
	}
	return eng
}

// labelScanPattern matches a rendered label-scan leaf in any of the plan
// renderings and captures the LABEL. The renderings do not agree on the detail
// syntax — the physical tree PROFILE and EXPLAIN capture prints `[B]`, while the
// logical tree and the table print `[b:B]` — so the variable prefix is optional.
var labelScanPattern = regexp.MustCompile(`NodeByLabelScan \[(?:[^:\]]*:)?([^\]]+)\]`)

// scanOrder is the sequence of labels a rendering scans, in the order it prints
// them. The renderings use different syntax, so their texts cannot be compared
// directly; the DRIVE ORDER can, and it is the thing the reorder changes.
func scanOrder(rendering string) []string {
	m := labelScanPattern.FindAllStringSubmatch(rendering, -1)
	out := make([]string, 0, len(m))
	for _, g := range m {
		out = append(out, g[1])
	}
	return out
}

func sameOrder(a, b []string) bool {
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

// TestExplainFidelity_MatchesTheTreeThatRuns is the acceptance gate for the
// claim on Engine.explainPhysical.
func TestExplainFidelity_MatchesTheTreeThatRuns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		query  string
		params map[string]expr.Value
		// seed builds the engine for this case. nil means seedFidelityGraph, the
		// :P graph the index and join substitutions need.
		seed func(*testing.T) *cypher.Engine
	}{
		{
			name:  "literal equality reaching an index",
			query: "MATCH (n:P) WHERE n.age = 7 RETURN n.name",
		},
		{
			name:   "the same predicate supplied as a parameter",
			query:  "MATCH (n:P) WHERE n.age = $a RETURN n.name",
			params: map[string]expr.Value{"a": expr.IntegerValue(7)},
		},
		{
			name:  "an equi-join the planner may substitute",
			query: "MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)",
		},
		{
			name:   "a range predicate from a parameter",
			query:  "MATCH (n:P) WHERE n.age > $lo RETURN n.name",
			params: map[string]expr.Value{"lo": expr.IntegerValue(25)},
		},
		{
			name:  "an expansion",
			query: "MATCH (a:P)-[:KNOWS]->(b) RETURN b.name",
		},
		{
			// rmp #2771. The drive order here is decided from a PROPERTY STATISTIC
			// rather than from an exact count, and the statistic's denominator is a
			// live label count that DECLINES whenever MVCC history is unreclaimed —
			// so the graph is seeded with history deliberately live.
			//
			// Both sides of this comparison pin a snapshot (runExplainPrefixed and
			// PROFILE alike), so they agreed even while the defect was live: this
			// case is a FORWARD guard against a rendering path that resolves counts
			// differently from the build, not a reproduction of it. The reproduction
			// is TestExplainFidelity_StatisticsDrivenReorderMatchesTheTreeThatRuns
			// below, which compares the PRESENT-TIME rendering surfaces instead.
			name:  "a reorder driven by a property statistic, with MVCC history live",
			query: statsReorderQuery,
			seed:  func(t *testing.T) *cypher.Engine { return seedStatsReorderEngine(t, true) },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			seed := c.seed
			if seed == nil {
				seed = seedFidelityGraph
			}
			eng := seed(t)

			// COLD: nothing has planned this query yet.
			cold := explainShape(t, eng, c.query, c.params)
			coldRan := executedShape(t, eng, c.query, c.params)
			if cold != coldRan {
				t.Errorf("COLD: EXPLAIN renders a plan the run did not build.\n"+
					"EXPLAIN:\n%s\nEXECUTED:\n%s", cold, coldRan)
			}

			// WARM: both sides now read a cached planCacheEntry. A cache that
			// carried a decision taken for a different binding would show here.
			warm := explainShape(t, eng, c.query, c.params)
			warmRan := executedShape(t, eng, c.query, c.params)
			if warm != warmRan {
				t.Errorf("WARM (plan-cache hit): EXPLAIN renders a plan the run did not "+
					"build.\nEXPLAIN:\n%s\nEXECUTED:\n%s", warm, warmRan)
			}
			if cold != warm {
				t.Errorf("EXPLAIN reports a different plan cold and warm, so the plan "+
					"cache changes what a diagnostic reports.\nCOLD:\n%s\nWARM:\n%s",
					cold, warm)
			}
		})
	}
}

// TestExplainFidelity_UnboundParameterDivergesFromTheRun pins the ONE case where
// EXPLAIN's plan is not the plan that runs.
//
// EXPLAIN deliberately does not require parameters — plan_prefix.go documents
// that, and Neo4j behaves the same way — but an access-path gate that reads a
// parameter's VALUE cannot fire without one, so the plan rendered for an unbound
// `$a` is a full label scan while the run with `$a` bound seeks an index. Nothing
// in the rendered output says the plan was chosen without the value.
//
// The test asserts the divergence rather than the absence of it, so that a future
// change which closes the gap (by requiring the parameter, by rendering a
// warning, or by planning the same way in both cases) FAILS here and is
// reconciled with the documentation deliberately.
func TestExplainFidelity_UnboundParameterDivergesFromTheRun(t *testing.T) {
	t.Parallel()
	eng := seedFidelityGraph(t)
	const q = "MATCH (n:P) WHERE n.age = $a RETURN n.name"
	bound := map[string]expr.Value{"a": expr.IntegerValue(7)}

	unbound := explainShape(t, eng, q, nil)
	withValue := explainShape(t, eng, q, bound)
	ran := executedShape(t, eng, q, bound)

	if withValue != ran {
		t.Fatalf("EXPLAIN with the parameter BOUND already disagrees with the run, so "+
			"this test is measuring the wrong thing.\nEXPLAIN:\n%s\nEXECUTED:\n%s",
			withValue, ran)
	}
	if unbound == ran {
		t.Errorf("EXPLAIN with an UNBOUND parameter now renders the same plan the "+
			"bound run builds. That is an improvement, not a failure — but it "+
			"contradicts the note on Engine.runExplainPrefixed and the audit in "+
			"docs/explain-profile-honesty-audit-2026-09-03.md, both of which record "+
			"the divergence as current behaviour. Update them with this change.\n%s",
			unbound)
	}
	// The specific shape of the divergence, so the record is concrete rather than
	// merely "they differ".
	if !strings.Contains(unbound, "NodeByLabelScan") {
		t.Errorf("the unbound plan no longer scans; the divergence has changed shape "+
			"and the documentation describing it is now wrong:\n%s", unbound)
	}
	if !strings.Contains(ran, "NodeByIndexRangeScan") && !strings.Contains(ran, "NodeByIndexSeek") {
		t.Errorf("the bound run no longer seeks an index, so this case no longer "+
			"demonstrates a plan a reader of EXPLAIN would be misled about:\n%s", ran)
	}
}

// TestExplainFidelity_StatisticsDrivenReorderMatchesTheTreeThatRuns closes the
// gap the acceptance table above cannot see (rmp #2771).
//
// [cypher.Engine.ExplainLogical] and [cypher.Engine.ExplainTable] are the surfaces
// a reader reaches for when asking "what will this query do", and they are the two
// that do NOT pin a snapshot: they resolve label counts through a present-time
// resolver, deliberately, because a diagnostic is not required to be consistent.
// The execution build resolves through the query's pinned snapshot.
//
// That difference was invisible until a plan decision started depending on a
// PROPERTY STATISTIC (rmp #2766), because the statistics' denominator is a live
// label count and [lpg.Graph.LabelCountExact] declines the moment MVCC history is
// live — for the pinned resolver, and not for the present-time one. The estimate
// was therefore demoted on one side and not the other, and EXPLAIN named a drive
// order the engine did not take.
//
// The comparison is on DRIVE ORDER rather than on rendered text: the three
// surfaces print different syntax, and the order of the scans is the thing the
// reorder actually changes.
func TestExplainFidelity_StatisticsDrivenReorderMatchesTheTreeThatRuns(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		live bool
	}{
		{name: "quiet graph", live: false},
		{name: "MVCC history live at plan time", live: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng := seedStatsReorderEngine(t, c.live)

			ran := executedShape(t, eng, statsReorderQuery, nil)
			ranOrder := scanOrder(ran)
			if len(ranOrder) != 2 {
				t.Fatalf("the executed plan scans %d labels, want 2; this query no longer "+
					"has the two-component shape the reorder acts on:\n%s", len(ranOrder), ran)
			}

			logical, err := eng.ExplainLogical(statsReorderQuery, nil)
			if err != nil {
				t.Fatalf("ExplainLogical: %v", err)
			}
			if got := scanOrder(logical); !sameOrder(got, ranOrder) {
				t.Errorf("ExplainLogical names the drive order %v, the run took %v. A "+
					"reader is shown a plan the engine does not run.\nEXPLAIN:\n%s\nEXECUTED:\n%s",
					got, ranOrder, logical, ran)
			}

			table, err := eng.ExplainTable(statsReorderQuery, nil)
			if err != nil {
				t.Fatalf("ExplainTable: %v", err)
			}
			if got := scanOrder(table); !sameOrder(got, ranOrder) {
				t.Errorf("ExplainTable names the drive order %v, the run took %v.\n"+
					"EXPLAIN:\n%s\nEXECUTED:\n%s", got, ranOrder, table, ran)
			}

			// The EXPLAIN-prefixed surface pins a snapshot, so it must match the run
			// exactly — operator for operator, not merely in drive order.
			if prefixed := explainShape(t, eng, statsReorderQuery, nil); prefixed != ran {
				t.Errorf("EXPLAIN <query> renders a plan the run did not build.\n"+
					"EXPLAIN:\n%s\nEXECUTED:\n%s", prefixed, ran)
			}
		})
	}
}

// TestExplainFidelity_StatisticsDrivenReorderIsDeterministic asserts the property
// the fidelity above rests on: the drive order the engine chooses for a
// statistics-driven reorder must be the same whether or not MVCC history happens
// to be live. Without it the two surfaces could agree run by run and still name
// different plans on different runs (rmp #2771).
func TestExplainFidelity_StatisticsDrivenReorderIsDeterministic(t *testing.T) {
	t.Parallel()
	quiet := executedShape(t, seedStatsReorderEngine(t, false), statsReorderQuery, nil)
	live := executedShape(t, seedStatsReorderEngine(t, true), statsReorderQuery, nil)
	// Guard against a vacuous pass: two empty orders compare equal, so a rendering
	// change that stopped naming the scans at all would silently satisfy this.
	if got := scanOrder(quiet); len(got) != 2 {
		t.Fatalf("the executed plan scans %d labels, want 2; this comparison would be "+
			"vacuous:\n%s", len(got), quiet)
	}
	if !sameOrder(scanOrder(quiet), scanOrder(live)) {
		t.Errorf("the executed drive order is %v on a quiet graph and %v with live MVCC "+
			"history over identical data: the plan depends on unrelated concurrent "+
			"activity.\nQUIET:\n%s\nLIVE:\n%s",
			scanOrder(quiet), scanOrder(live), quiet, live)
	}
}
