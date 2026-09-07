package cypher

// stats_equality_freshness_test.go — the staleness screen on the most-common-value
// equality estimate (rmp #2772).
//
// Layer: short.
//
// # What was wrong
//
// `statsEqualityEstimateInner` returned an MCV hit tagged estExact from whatever
// snapshot was last published, with no staleness term on that path at all, while its
// sibling `statsRangeEstimateInner` demoted a stale histogram to estFallback. Since
// rmp #2765 the tag decides how the figure RENDERS — estExact prints as a bare number
// with no approximation marker, everything else prints with a tilde or not at all —
// so an arbitrarily drifted count was shown to a reader as ground truth. Measured on
// the rmp #2767 fixture before the fix: `Est.Rows = 1000` beside a measured 10, in
// the EXPLAIN tree, in ExplainTable and in ProfileTable alike.
//
// # What each gate rules out
//
//   - a STALE count is no longer rendered as an unmarked exact number, on all three
//     surfaces (a fix applied to only one of them would leave the other two lying);
//   - a FRESH count still is, so the fix demotes on staleness rather than on the
//     mere presence of a statistic;
//   - GROWTH alone demotes, which is what pins the DENOMINATOR: the live count on its
//     own calls this case fresh, and the count it then certifies is wrong;
//   - an EMPTIED label demotes, and names that reason rather than "stale";
//   - the demotion is counted, and its reason counter partitions the total;
//   - the planner-side screen [reorderStatsFreshness] is now a proven no-op on the
//     equality path, which is why its call site there was removed;
//   - the one equality verdict whose treatment that removal changes is pinned.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// mcvQuery is the predicate every gate below renders and runs. 'hot' is the value
// the most-common-value list records, so the estimate under test is always the MCV
// hit and never the 1/NDV average.
const mcvQuery = `MATCH (p:Person) WHERE p.grp = 'hot' RETURN p`

// seedMCVEngine builds an engine over `hot` :Person nodes carrying grp='hot' and
// `cold` carrying a distinct value each, with statistics refreshed.
//
// The distinct cold values are not decoration: they push the NDV high enough that
// the 1/NDV average is far from the MCV count, so a defect that read the wrong
// provider could not land on a plausible number by accident.
func seedMCVEngine(t *testing.T, hot, cold int) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	add := func(key, grp string, rank int64) {
		t.Helper()
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", key, err)
		}
		if err := g.SetNodeProperty(key, "grp", lpg.StringValue(grp)); err != nil {
			t.Fatalf("SetNodeProperty(%s, grp): %v", key, err)
		}
		if err := g.SetNodeProperty(key, "rank", lpg.Int64Value(rank)); err != nil {
			t.Fatalf("SetNodeProperty(%s, rank): %v", key, err)
		}
	}
	for i := 0; i < hot; i++ {
		add(fmt.Sprintf("hot%d", i), "hot", int64(i))
	}
	for i := 0; i < cold; i++ {
		add(fmt.Sprintf("cold%d", i), fmt.Sprintf("cold%d", i), -1)
	}
	e := NewEngine(g)
	t.Cleanup(func() { _ = e.Close() })
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	return e
}

// mcvEstimate returns the equality provider's verdict for 'hot', resolving N the way
// the RENDERING path does.
func mcvEstimate(t *testing.T, e *Engine) (estimate, statsFallbackReason) {
	t.Helper()
	src := liveResolver(e)
	return statsEqualityEstimateInner(src, "Person", "grp",
		expr.StringValue("hot"), resolveLabelPopulation(src, "Person"))
}

// estCellFor returns the Est.Rows cell of the first row of a rendered table whose
// Operator cell names op. It locates BOTH columns by their header rather than by a
// fixed index, because Est.Rows is conditional (rmp #2765) and ProfileTable carries
// four further columns that ExplainTable does not.
func estCellFor(t *testing.T, table, op string) string {
	t.Helper()
	var header []string
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if header == nil {
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			header = cells
			continue
		}
		opCol, estCol := -1, -1
		for i, h := range header {
			switch h {
			case "Operator":
				opCol = i
			case "Est.Rows":
				estCol = i
			}
		}
		if opCol < 0 || estCol < 0 {
			t.Fatalf("the rendered table has no Operator and Est.Rows pair; "+
				"there is no cell to read:\n%s", table)
		}
		if estCol < len(cells) && strings.Contains(cells[opCol], op) {
			return strings.TrimSpace(cells[estCol])
		}
	}
	t.Fatalf("no row named %q in:\n%s", op, table)
	return ""
}

// dirtyByReplacingGrp rewrites grp on n of the 'hot' nodes, through the engine's own
// write path so the staleness bookkeeping a real workload drives is the one exercised.
func dirtyByReplacingGrp(t *testing.T, e *Engine, n int) {
	t.Helper()
	q := fmt.Sprintf(
		`MATCH (p:Person) WHERE p.grp = 'hot' AND p.rank < %d SET p.grp = 'moved'`, n)
	if _, err := e.RunInTx(context.Background(), q, nil); err != nil {
		t.Fatalf("dirtying write: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The honesty gates
// ─────────────────────────────────────────────────────────────────────────────

// TestStatsEqualityFreshness_StaleMCVIsNotRenderedAsExact is the acceptance gate.
//
// All three surfaces are asserted, and separately. They are three different
// renderers over two different plans — the free-form physical tree
// ([Engine.Explain]), the LOGICAL table ([Engine.ExplainTable]) and the PHYSICAL
// profiled table ([Engine.ProfileTable]) — and a fix applied to one of them would
// leave the other two presenting the same stale count as ground truth.
func TestStatsEqualityFreshness_StaleMCVIsNotRenderedAsExact(t *testing.T) {
	e := seedMCVEngine(t, 1000, 400)
	dirtyByReplacingGrp(t, e, 990)

	// Premise: the statistic really is stale by the counters it maintains. Without
	// this the gates below could pass because no estimate was derivable at all.
	st, ok := lookupStats(liveResolver(e), "Person", "grp")
	if !ok {
		t.Fatal("the (Person, grp) statistic vanished; the fixture has no subject")
	}
	if st.Delta() == 0 {
		t.Fatalf("delta = 0 after 990 property writes; the fixture is not stale and "+
			"this gate would pass for the wrong reason (deletes=%d)", st.Deletes())
	}

	tree, err := e.Explain(mcvQuery, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if strings.Contains(tree, "est. rows=1000 exact") {
		t.Errorf("the EXPLAIN tree still presents the stale most-common-value count "+
			"as an exact, maintained number; 10 rows match the predicate:\n%s", tree)
	}

	if got := estCellFor(t, mustExplainTable(t, e, mcvQuery, nil), "Selection"); got != "-" {
		t.Errorf("ExplainTable Est.Rows for the Selection = %q, want %q. A bare number "+
			"in that column claims a maintained exact count, and this one is 100x wrong",
			got, "-")
	}

	if got := estCellFor(t, mustProfileTable(t, e, mcvQuery), "Filter"); got != "-" {
		t.Errorf("ProfileTable Est.Rows for the Filter = %q, want %q — printed here it "+
			"sits immediately left of the measured Rows, which is the comparison the "+
			"whole column exists to make", got, "-")
	}
}

// TestStatsEqualityFreshness_FreshMCVIsStillRenderedAsExact is the other half of the
// distinction, and the reason the gate above is not satisfied by deleting the MCV
// path outright: a count from a snapshot nothing has invalidated is still exact, and
// still renders as a bare number on every surface.
func TestStatsEqualityFreshness_FreshMCVIsStillRenderedAsExact(t *testing.T) {
	e := seedMCVEngine(t, 1000, 400)

	tree, err := e.Explain(mcvQuery, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(tree, "est. rows=1000 exact") {
		t.Errorf("the EXPLAIN tree does not present the FRESH most-common-value count "+
			"as exact; the screen demotes on something other than staleness:\n%s", tree)
	}

	if got := estCellFor(t, mustExplainTable(t, e, mcvQuery, nil), "Selection"); got != "1000" {
		t.Errorf("ExplainTable Est.Rows for the Selection = %q, want %q (a bare number, "+
			"no approximation marker)", got, "1000")
	}

	if got := estCellFor(t, mustProfileTable(t, e, mcvQuery), "Filter"); got != "1000" {
		t.Errorf("ProfileTable Est.Rows for the Filter = %q, want %q", got, "1000")
	}
}

// TestStatsEqualityFreshness_GrowthAloneDemotesTheMCVCount pins the DENOMINATOR, and
// it is the gate that distinguishes the two formulations the module had to choose
// between.
//
// [statsRangeEstimateInner] divides Δ by the LIVE label count. [reorderStatsFreshness]
// divides by the smaller of the live count and the statistic's build-time count.
// Growth is where they disagree: a bundle built over 1000 rows all carrying
// grp='hot', grown by 100 rows that also carry it, has Δ = 100 against a live count
// of 1100. Δ/live = 0.0909 is UNDER the 0.0961 threshold and Δ/N0 = 0.1000 is over
// it — so the live count alone calls the snapshot fresh, and certifies 1000 as an
// exact count for a predicate that now matches 1100.
//
// The window is narrow by construction and the premise is asserted, because a
// fixture that drifted out of it would leave this gate passing while testing nothing.
func TestStatsEqualityFreshness_GrowthAloneDemotesTheMCVCount(t *testing.T) {
	e := seedMCVEngine(t, 1000, 0)
	for i := 0; i < 100; i++ {
		if _, err := e.RunInTx(context.Background(), `CREATE (p:Person {grp:'hot', rank:-1})`, nil); err != nil {
			t.Fatalf("growing write: %v", err)
		}
	}

	src := liveResolver(e)
	st, ok := lookupStats(src, "Person", "grp")
	if !ok {
		t.Fatal("the (Person, grp) statistic vanished; the fixture has no subject")
	}
	live := resolveLabelPopulation(src, "Person")
	thr := statsRangeBreakEven - 1.0/float64(statsHistogramBuckets)
	byLive := float64(st.Delta()) / live.n
	byBuild := float64(st.Delta()) / float64(st.LabelCount())
	if st.Deletes() != 0 || !live.known || !(byLive < thr && byBuild >= thr) {
		t.Fatalf("the isolating window is gone: delta=%d n0=%d live=%v(known=%v) "+
			"deletes=%d -> byLive=%.4f byBuild=%.4f threshold=%.4f",
			st.Delta(), st.LabelCount(), live.n, live.known, st.Deletes(), byLive, byBuild, thr)
	}

	if e, _ := mcvEstimate(t, e); e.source != estFallback {
		t.Errorf("the provider tagged the grown-past count %v, want %v. The live count "+
			"alone puts this snapshot inside the firing region, and the count it "+
			"certifies is short by every row added since the build",
			e.source, estFallback)
	}
}

// TestStatsEqualityFreshness_EmptiedLabelDemotesAndNamesTheReason gates the
// zero-denominator branch, on the one route that can reach it.
//
// Deleting the nodes does not reach it: a node's removal removes its properties
// through the same write hook, so Δ and the delete counter both move and the delete
// tolerance demotes first (measured on this engine: DETACH DELETE of 990 :Person rows
// reported delta=990 deletes=990). Removing the LABEL touches no property, so both
// counters stay at zero, the snapshot is pristine by its own measure, and the live
// count is nevertheless zero.
func TestStatsEqualityFreshness_EmptiedLabelDemotesAndNamesTheReason(t *testing.T) {
	e := seedMCVEngine(t, 1000, 0)
	if _, err := e.RunInTx(context.Background(), `MATCH (p:Person) REMOVE p:Person`, nil); err != nil {
		t.Fatalf("REMOVE label: %v", err)
	}

	src := liveResolver(e)
	st, ok := lookupStats(src, "Person", "grp")
	if !ok {
		t.Fatal("the (Person, grp) statistic vanished; the fixture has no subject")
	}
	live := resolveLabelPopulation(src, "Person")
	if st.Delta() != 0 || st.Deletes() != 0 || st.LabelCount() <= 0 || !live.known || live.n != 0 {
		t.Fatalf("the isolating premise is gone: delta=%d deletes=%d n0=%d live=%v(known=%v); "+
			"this gate only covers the zero-denominator branch while the counters are clean",
			st.Delta(), st.Deletes(), st.LabelCount(), live.n, live.known)
	}

	got, why := mcvEstimate(t, e)
	if got.source != estFallback {
		t.Errorf("an emptied label's most-common-value count was tagged %v, want %v — "+
			"no row can carry the value, whatever the snapshot recorded", got.source, estFallback)
	}
	if why != statsFallbackEmptyLabel {
		t.Errorf("the demotion reason is %d, want statsFallbackEmptyLabel (%d). The "+
			"reason counters partition the fallback total, so reporting an empty label "+
			"as staleness misdirects the caller this surface exists to inform",
			why, statsFallbackEmptyLabel)
	}
}

// TestStatsEqualityFreshness_DemotionIsCountedAndNamed holds the observability half:
// a demoted MCV hit must increment the fallback total AND exactly the counter that
// names staleness, or the reason counters stop partitioning the total.
func TestStatsEqualityFreshness_DemotionIsCountedAndNamed(t *testing.T) {
	e := seedMCVEngine(t, 1000, 400)
	dirtyByReplacingGrp(t, e, 990)

	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	defer cmetrics.SetBackend(nil)

	src := liveResolver(e)
	if got := statsEqualityEstimate(src, "Person", "grp", expr.StringValue("hot")); got.source != estFallback {
		t.Fatalf("the provider returned %v, want %v; there is no demotion to count",
			got.source, estFallback)
	}
	if got := probe.counter(statsMetricLookupFallbackStale); got != 1 {
		t.Errorf("%s = %d, want 1", statsMetricLookupFallbackStale, got)
	}
	if got := probe.counter(statsMetricLookupFallback); got != 1 {
		t.Errorf("%s = %d, want 1 — the reason counters must sum to the total",
			statsMetricLookupFallback, got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The consequences for the rmp #2766 plan decision
// ─────────────────────────────────────────────────────────────────────────────

// TestReorderStatsFreshness_EqualityScreenIsRedundant is the evidence for removing
// [reorderStatsFreshness] from the equality branch of [reorderFilteredRows].
//
// Both now apply [statsSnapshotFresh] to the same statistic with the same
// population — the reorder gate resolves N through [populationFromDrain] and the
// provider is handed that same value — so screening a screened verdict cannot change
// it. The equivalence is asserted over every state the screen distinguishes rather
// than argued, because "provably a no-op" is exactly the claim a later change to
// either side would silently falsify.
func TestReorderStatsFreshness_EqualityScreenIsRedundant(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dirty func(t *testing.T, e *Engine)
	}{
		{"fresh", func(*testing.T, *Engine) {}},
		{"stale-by-writes", func(t *testing.T, e *Engine) { dirtyByReplacingGrp(t, e, 990) }},
		{"emptied-label", func(t *testing.T, e *Engine) {
			if _, err := e.RunInTx(context.Background(), `MATCH (p:Person) REMOVE p:Person`, nil); err != nil {
				t.Fatalf("REMOVE label: %v", err)
			}
		}},
		{"deleted-nodes", func(t *testing.T, e *Engine) {
			if _, err := e.RunInTx(context.Background(),
				`MATCH (p:Person) WHERE p.grp = 'hot' DETACH DELETE p`, nil); err != nil {
				t.Fatalf("DETACH DELETE: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := seedMCVEngine(t, 1000, 400)
			tc.dirty(t, e)
			src := liveResolver(e)
			drain := labelCardinalityEstimate(src, "Person")
			bare := statsEqualityEstimateWith(src, "Person", "grp",
				expr.StringValue("hot"), populationFromDrain(drain))
			screened := reorderStatsFreshness(src, "Person", "grp", drain, bare)
			if screened != bare {
				t.Errorf("the planner screen changed the provider's verdict from %+v to "+
					"%+v; it is not the no-op its removal from the equality path assumes",
					bare, screened)
			}
		})
	}
}

// TestStatsEqualityFreshness_NaNLiteralSurvivesStaleness pins the ONE equality
// verdict whose treatment the removal changes, so the change is recorded rather than
// discovered.
//
// `n.p = NaN` is false for every row under openCypher — NaN is equal to nothing,
// itself included — so the provider answers exactly zero rows, tagged estExact, from
// the semantics and not from the statistic. [reorderStatsFreshness] used to demote
// that verdict along with every other, because it screened on the statistic's age
// without regard to whether the number had been read from it. Nothing about a
// stale snapshot makes `= NaN` match a row, so the demotion was a loss and its
// removal is the correction.
func TestStatsEqualityFreshness_NaNLiteralSurvivesStaleness(t *testing.T) {
	e := seedMCVEngine(t, 1000, 400)
	dirtyByReplacingGrp(t, e, 990)
	src := liveResolver(e)

	scan := &ir.NodeByLabelScan{NodeVar: "p", Label: "Person"}
	sel := &ir.Selection{Child: scan, PredicateExpr: &ast.BinaryOp{
		Operator: "=",
		Left:     &ast.Property{Receiver: &ast.Variable{Name: "p"}, Key: "grp"},
		Right:    &ast.FloatLiteral{Value: math.NaN()},
	}}
	got, _ := reorderFilteredRows(sel, scan, src, nil, labelCardinalityEstimate(src, "Person"))
	if got.source != estExact || got.rows != 0 {
		t.Errorf("the reorder gate's estimate for `= NaN` over a stale statistic is "+
			"%+v, want {rows:0 source:exact}. No row can equal NaN, so the count is a "+
			"fact of the language and cannot go stale", got)
	}
}
