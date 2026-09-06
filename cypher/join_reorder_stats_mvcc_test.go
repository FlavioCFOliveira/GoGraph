package cypher

// join_reorder_stats_mvcc_test.go — rmp #2771.
//
// The property-statistics reorder (rmp #2766) has to reach the same verdict
// whether or not MVCC history happens to be live on the graph it is planning
// over. It did not: [lpgLabelResolver.ResolveLabelCount] delegates to
// [lpg.Graph.LabelCountExact], which is EXACT-OR-NOTHING and declines the moment
// any node-life or label-delta record is unreclaimed; the range estimator read it
// as `n, _ :=`, so the decline arrived as the number zero and the `n <= 0` guard
// demoted the whole estimate to estFallback. The trustworthiness veto then kept
// the written order.
//
// # Why that only showed up under `go test -race ./...`
//
// Nothing in the failing test wrote to the graph after the build. The records the
// gate tripped on were the BUILD's own: version records are reclaimed by an
// ASYNCHRONOUS vacuum goroutine ([Graph.wakeVacuum]), so under whole-module
// parallel load the vacuum lags and the 7 000 birth records the seed pushed are
// still live when the query is planned. Measured on this host with the machine
// deliberately saturated: 303 unreclaimed life records at gate time, and the
// snapshot-pinned count declining as a result. On an idle host the vacuum always
// won the race, so the defect was invisible.
//
// The tests here do not depend on that race. They pin the horizon with an open
// reader and then write, which makes the record UNRECLAIMABLE by construction —
// the same state the loaded gate reaches by accident, reached on purpose.
//
// Several tests install a metrics backend, which is PROCESS-GLOBAL, so — like the
// other backend-installing tests in this package (stats_metrics_test.go) — none of
// them may call t.Parallel(). Race-clean; short layer.

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/stats"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// liveHistory makes MVCC history LIVE on g and keeps it live for the test's
// duration, deterministically.
//
// A write alone is not enough: the vacuum reclaims its records as soon as the
// watermark passes them, so the state would be transient. Registering a reader
// FIRST caps the reclamation watermark at that reader's start instant
// ([mvcc.Horizon.Oldest]), so every record the subsequent write pushes is newer
// than the watermark and cannot be freed while the reader is registered.
//
// The write is a node under a label the queries here never touch, carrying a
// property no statistic tracks, and it is made straight on the graph rather than
// through the engine — so it moves no label count the planner reads, adds no row
// to any result, and bumps no statistic's staleness counter. The ONLY thing it
// changes is that history is live.
//
// It returns the number of live node-life records, so a caller can assert the
// precondition it depends on actually holds rather than assuming it.
func liveHistory(t testing.TB, g *lpg.Graph[string, float64], key string) int64 {
	t.Helper()
	held := g.BeginRead()
	t.Cleanup(func() { g.EndRead(held) })
	mustNode(t, g, key, "ZZUnrelated", "zz", 1)
	n := g.NodeLifeVersionCount()
	if n == 0 {
		t.Fatalf("liveHistory: no node-life record survived the write, so this test "+
			"would exercise the same state as one with no history at all "+
			"(labelDelta=%d)", g.LabelDeltaCount())
	}
	return n
}

// pinnedStatsSource is the resolver the EXECUTION build uses: one pinned to an
// MVCC snapshot, exactly as [Engine.buildReadPhysical] constructs it from the
// snapshot [Engine.Run] began.
func pinnedStatsSource(t testing.TB, e *Engine) *lpgLabelResolver {
	t.Helper()
	snap := e.g.BeginRead()
	t.Cleanup(func() { e.g.EndRead(snap) })
	return &lpgLabelResolver{g: e.g.ReadAt(snap), eng: e}
}

// TestStatsRangeEstimate_ProvenanceSurvivesLiveHistory is the estimator-level
// reproduction: the same graph, the same statistic and the same predicate must
// yield the same PROVENANCE through the present-time resolver EXPLAIN renders
// with and through the snapshot-pinned resolver the execution build uses.
//
// Before rmp #2771 the pinned side answered estFallback the instant any history
// was live, while the live side answered estStats — which is precisely how EXPLAIN
// came to render a swap the engine would not perform.
func TestStatsRangeEstimate_ProvenanceSurvivesLiveHistory(t *testing.T) {
	g := buildRangeSkewGraph(t, 5000, 2000)
	on, _ := statsReorderPair(t, g)

	// The reference: no live history at all.
	quiet, quietErr := statsRangeEstimate(statsTestSource(on), "A", "x", stats.OpGt, expr.IntegerValue(4900))
	if quiet.source != estStats {
		t.Fatalf("with no live history the range estimate is %v (rows=%v), want stats; "+
			"this test cannot detect a regression from a baseline that is already "+
			"demoted", quiet.source, quiet.rows)
	}

	records := liveHistory(t, g, "zz-range-provenance")

	live := statsTestSource(on)
	pinned := pinnedStatsSource(t, on)

	// The precondition this whole task is about: the exact count DECLINES for the
	// pinned view and does not for the present-time one. Asserted, not assumed —
	// if it ever stops declining, these tests would pass for the wrong reason.
	if _, ok := pinned.ResolveLabelCount("A"); ok {
		t.Fatalf("ResolveLabelCount no longer declines for a snapshot-pinned resolver "+
			"with %d live life records, so this test no longer exercises the "+
			"divergence it was written for", records)
	}
	if _, ok := live.ResolveLabelCount("A"); !ok {
		t.Fatal("ResolveLabelCount now declines for the PRESENT-TIME resolver too, so " +
			"EXPLAIN and the run no longer differ here and this test is measuring " +
			"the wrong thing")
	}

	gotLive, errLive := statsRangeEstimate(live, "A", "x", stats.OpGt, expr.IntegerValue(4900))
	gotPin, errPin := statsRangeEstimate(pinned, "A", "x", stats.OpGt, expr.IntegerValue(4900))

	if gotPin.source != gotLive.source {
		t.Errorf("provenance differs between the two resolvers with %d live life "+
			"records: present-time=%v, snapshot-pinned=%v. EXPLAIN reads the first "+
			"and the execution build reads the second, so a reader is shown a plan "+
			"the engine does not run.", records, gotLive.source, gotPin.source)
	}
	if gotPin.source != quiet.source {
		t.Errorf("live MVCC history changed the provenance of a range estimate over an "+
			"unchanged graph and an unchanged statistic: %v with history, %v without",
			gotPin.source, quiet.source)
	}
	if gotPin.rows != quietErrRows(quiet) || gotLive.rows != quietErrRows(quiet) {
		t.Errorf("row estimate changed with live history: quiet=%v live=%v pinned=%v",
			quiet.rows, gotLive.rows, gotPin.rows)
	}
	if errLive != quietErr || errPin != quietErr {
		t.Errorf("certified error changed with live history: quiet=%v live=%v pinned=%v",
			quietErr, errLive, errPin)
	}
}

// quietErrRows is a named accessor for the reference row count, so the comparison
// above reads as a comparison against the reference rather than against a field.
func quietErrRows(reference estimate) float64 { return reference.rows }

// TestStatsRangeEstimate_EmptyLabelIsNotADeclinedCount separates the two facts the
// old `n, _ :=` conflated.
//
// A label that genuinely holds zero live nodes must STILL demote — there is no
// population to be selective over — and it must be distinguishable from a count
// that merely could not be obtained. The estimate alone cannot tell them apart
// (both are estFallback with zero rows), so the reason counters are the oracle.
func TestStatsRangeEstimate_EmptyLabelIsNotADeclinedCount(t *testing.T) {
	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	g := buildRangeSkewGraph(t, 5000, 2000)
	on, _ := statsReorderPair(t, g)

	// A genuinely EMPTY label. :A holds 5000 nodes and has a statistic; delete them
	// all and the statistic remains while the population goes to zero. Statistics
	// are NOT refreshed afterwards, on purpose: the point is a live statistic over a
	// label that no longer has any rows.
	for i := 0; i < 5000; i++ {
		g.RemoveNode("a" + strconv.Itoa(i))
	}
	// The resolver is built AFTER the deletions: one built before would read a view
	// of the instant before them.
	src := statsTestSource(on)
	// The precondition is on the POPULATION, not on the exact count. Deleting 5000
	// nodes leaves node-life records behind, so the exact count may well decline
	// here — and that is exactly the point: the population is still KNOWN to be
	// zero, because the bitmap route answers where the count does not.
	if pop := resolveLabelPopulation(src, "A"); !pop.known || pop.n != 0 {
		t.Fatalf("after deleting every :A node the population is {n:%v known:%v}, want "+
			"a KNOWN zero; this test cannot exercise the empty-label case", pop.n, pop.known)
	}

	empty0 := probe.counter(statsMetricLookupFallbackEmptyLabel)
	noCount0 := probe.counter(statsMetricLookupFallbackNoCount)
	e, _ := statsRangeEstimate(src, "A", "x", stats.OpGt, expr.IntegerValue(4900))
	if e.source != estFallback {
		t.Errorf("a label with zero live nodes yielded %v, want fallback: there is no "+
			"population for a selectivity to be a fraction OF", e.source)
	}
	if got := probe.counter(statsMetricLookupFallbackEmptyLabel); got != empty0+1 {
		t.Errorf("empty_label counter = %d, want %d: the demotion was not attributed to "+
			"the empty label", got, empty0+1)
	}
	if got := probe.counter(statsMetricLookupFallbackNoCount); got != noCount0 {
		t.Errorf("no_count counter = %d, want unchanged %d: an empty label is not an "+
			"unobtainable count and must not be reported as one", got, noCount0)
	}
}

// TestStatsRangeEstimate_DeclinedCountIsCountedAndRescued asserts the other half:
// a DECLINED exact count is observable, and it does not demote the estimate.
func TestStatsRangeEstimate_DeclinedCountIsCountedAndRescued(t *testing.T) {
	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	g := buildRangeSkewGraph(t, 5000, 2000)
	on, _ := statsReorderPair(t, g)
	liveHistory(t, g, "zz-declined-count")
	pinned := pinnedStatsSource(t, on)

	declined0 := probe.counter(statsMetricLabelCountDeclined)
	noCount0 := probe.counter(statsMetricLookupFallbackNoCount)
	e, _ := statsRangeEstimate(pinned, "A", "x", stats.OpGt, expr.IntegerValue(4900))

	if got := probe.counter(statsMetricLabelCountDeclined); got != declined0+1 {
		t.Errorf("label_count.declined = %d, want %d: an operator cannot see that the "+
			"zero-alloc exact count is unavailable", got, declined0+1)
	}
	if e.source != estStats {
		t.Errorf("a declined exact count demoted the estimate to %v; the count is "+
			"resolvable by another route and the estimate must survive", e.source)
	}
	if got := probe.counter(statsMetricLookupFallbackNoCount); got != noCount0 {
		t.Errorf("no_count counter = %d, want unchanged %d: the count WAS obtained, "+
			"just not by the cheap route", got, noCount0)
	}
}

// TestStatsFallbackReasons_PartitionTheTotal pins the invariant that makes the
// reason counters readable: every estFallback verdict increments the total AND
// exactly one reason, so the reasons sum to the total. Without it a reader of the
// metrics could not tell an unattributed demotion from an absent one.
func TestStatsFallbackReasons_PartitionTheTotal(t *testing.T) {
	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	g := buildRangeSkewGraph(t, 5000, 2000)
	on, _ := statsReorderPair(t, g)
	src := statsTestSource(on)

	// Drive every reason that is reachable from this boundary.
	// no_statistic: an untracked property, and a bound in no histogram domain.
	statsRangeEstimate(src, "A", "never_written", stats.OpGt, expr.IntegerValue(1))
	statsRangeEstimate(src, "A", "x", stats.OpGt, expr.BoolValue(true))
	statsEqualityEstimate(src, "A", "never_written", expr.IntegerValue(1))
	// no_count: a source that can neither count nor produce a bitmap.
	statsRangeEstimate(countlessStatsSource{on}, "A", "x", stats.OpGt, expr.IntegerValue(4900))
	statsEqualityEstimate(countlessStatsSource{on}, "A", "x", expr.IntegerValue(123456))

	total := probe.counter(statsMetricLookupFallback)
	sum := probe.counter(statsMetricLookupFallbackNoStatistic) +
		probe.counter(statsMetricLookupFallbackEmptyLabel) +
		probe.counter(statsMetricLookupFallbackNoCount) +
		probe.counter(statsMetricLookupFallbackStale)
	if total == 0 {
		t.Fatal("no fallback was recorded at all, so this test proves nothing about " +
			"how fallbacks are attributed")
	}
	if sum != total {
		t.Errorf("the reason counters do not partition the total: total=%d sum=%d "+
			"(no_statistic=%d empty_label=%d no_count=%d stale=%d)", total, sum,
			probe.counter(statsMetricLookupFallbackNoStatistic),
			probe.counter(statsMetricLookupFallbackEmptyLabel),
			probe.counter(statsMetricLookupFallbackNoCount),
			probe.counter(statsMetricLookupFallbackStale))
	}
	if probe.counter(statsMetricLookupFallbackNoCount) == 0 {
		t.Error("no_count never fired, so the counter that distinguishes an " +
			"unobtainable count from an absent statistic is vacuous here")
	}
	if probe.counter(statsMetricLookupFallbackNoStatistic) == 0 {
		t.Error("no_statistic never fired, so the two causes were not both exercised")
	}
}

// countlessStatsSource is a [statsSource] that shares a real engine's collector and
// registry but can supply NEITHER an exact label count NOR a label bitmap. It is
// the only shape for which N is genuinely unknowable, and it exists so the
// no_count demotion has a reachable exercise.
type countlessStatsSource struct{ eng *Engine }

func (c countlessStatsSource) Statistics() *statsCollector { return c.eng.statsCollector.Load() }

func (c countlessStatsSource) ResolveLabelID(name string) (uint32, bool) {
	lid, ok := c.eng.g.Registry().Lookup(name)
	return uint32(lid), ok
}

func (c countlessStatsSource) ResolvePropertyID(name string) (uint32, bool) {
	pid, ok := c.eng.g.PropertyKeys().Lookup(name)
	return uint32(pid), ok
}

func (c countlessStatsSource) ResolveLabelCount(string) (int64, bool) { return 0, false }

// TestJoinReorderStats_HistogramRangeDrivesASwapWithLiveHistory is the
// end-to-end reproduction, and the acceptance gate for the fix.
//
// It is TestJoinReorderStats_HistogramRangeDrivesASwap with MVCC history
// deliberately live at plan time. Before rmp #2771 the EXPLAIN surfaces rendered
// the swap (they resolve through a present-time resolver) while the run did not
// take it (it resolves through the snapshot-pinned one), so the build counter
// never moved.
func TestJoinReorderStats_HistogramRangeDrivesASwapWithLiveHistory(t *testing.T) {
	g := buildRangeSkewGraph(t, 5000, 2000)
	on, off := statsReorderPair(t, g)
	records := liveHistory(t, g, "zz-reorder-swap")
	const q = "MATCH (a:A) WHERE a.x > 4900 MATCH (b:B {y: 7}) RETURN a.x AS ax, b.y AS bv"

	lg, err := on.ExplainLogical(q, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstr(lg, "stats, err=") {
		t.Fatalf("expected a histogram (estStats) estimate on the decision path:\n%s", lg)
	}

	before := joinReorderBuildCount.Load()
	gotOn := sortedRows(t, on, q)
	if joinReorderBuildCount.Load() == before {
		t.Fatalf("EXPLAIN renders the histogram-driven reorder but the RUN did not take "+
			"it, with %d live MVCC life records at plan time. The two read the same "+
			"statistic through different resolvers, and only the execution one "+
			"declines its label count.\n%s", records, lg)
	}
	gotOff := sortedRows(t, off, q)
	if len(gotOn) != 99 {
		t.Fatalf("row count = %d, want 99 (x in 4901..4999 crossed with one b)", len(gotOn))
	}
	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch: reorder=%d default=%d", len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("bag row %d differs:\n  reorder = %s\n  default = %s", i, gotOn[i], gotOff[i])
		}
	}
	if !plansDriveWith(mustExplainTable(t, on, q, nil), "Selection") {
		t.Fatal("expected a Selection arm to drive after the swap")
	}
	// The precondition, asserted after the fact so a future change that stops the
	// count declining cannot leave this test silently exercising the quiet path.
	if _, ok := pinnedStatsSource(t, on).ResolveLabelCount("A"); ok {
		t.Errorf("ResolveLabelCount no longer declines for a snapshot-pinned resolver "+
			"with %d live life records, so this test no longer reproduces the "+
			"divergence", records)
	}
}

// TestJoinReorderStats_ReorderVerdictIsIndependentOfLiveHistory is the
// differential form: the SAME query on the SAME data must take the same drive
// order whether or not history is live. It is the property the sprint's second
// objective needs, stated directly rather than inferred from a counter.
func TestJoinReorderStats_ReorderVerdictIsIndependentOfLiveHistory(t *testing.T) {
	const q = "MATCH (a:A) WHERE a.x > 4900 MATCH (b:B {y: 7}) RETURN a.x AS ax, b.y AS bv"

	quietG := buildRangeSkewGraph(t, 5000, 2000)
	quietEng, _ := statsReorderPair(t, quietG)
	quietPlan := mustExplainTable(t, quietEng, q, nil)
	quietBefore := joinReorderBuildCount.Load()
	quietRows := sortedRows(t, quietEng, q)
	quietFired := joinReorderBuildCount.Load() != quietBefore

	liveG := buildRangeSkewGraph(t, 5000, 2000)
	liveEng, _ := statsReorderPair(t, liveG)
	liveHistory(t, liveG, "zz-verdict-parity")
	livePlan := mustExplainTable(t, liveEng, q, nil)
	liveBefore := joinReorderBuildCount.Load()
	liveRows := sortedRows(t, liveEng, q)
	liveFired := joinReorderBuildCount.Load() != liveBefore

	if quietFired != liveFired {
		t.Errorf("the reorder fired=%v with a quiet graph and fired=%v with live MVCC "+
			"history over identical data: the planner's use of its own statistics "+
			"depends on unrelated concurrent activity", quietFired, liveFired)
	}
	if quietPlan != livePlan {
		t.Errorf("EXPLAIN renders a different plan with live history:\nQUIET:\n%s\nLIVE:\n%s",
			quietPlan, livePlan)
	}
	if len(quietRows) != len(liveRows) {
		t.Fatalf("row counts differ: quiet=%d live=%d", len(quietRows), len(liveRows))
	}
	for i := range quietRows {
		if quietRows[i] != liveRows[i] {
			t.Fatalf("bag row %d differs:\n  quiet = %s\n  live  = %s", i, quietRows[i], liveRows[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Gates added for the sites the first mutation sweep left alive (rmp #2771).
// Each one names the mutation it exists to kill.
// ---------------------------------------------------------------------------

// countingLabelSource is a [statsSource] that CAN answer the exact live count and
// records every label-bitmap materialisation asked of it. It exists because a
// resolver that answered exactly and STILL built a bitmap would be behaviourally
// identical and quietly wasteful — invisible to every correctness assertion.
type countingLabelSource struct {
	eng         *Engine
	count       int64
	countOK     bool
	nilBitmap   bool
	bitmapCalls int
}

func (c *countingLabelSource) Statistics() *statsCollector { return c.eng.statsCollector.Load() }

func (c *countingLabelSource) ResolveLabelID(name string) (uint32, bool) {
	lid, ok := c.eng.g.Registry().Lookup(name)
	return uint32(lid), ok
}

func (c *countingLabelSource) ResolvePropertyID(name string) (uint32, bool) {
	pid, ok := c.eng.g.PropertyKeys().Lookup(name)
	return uint32(pid), ok
}

func (c *countingLabelSource) ResolveLabelCount(string) (int64, bool) {
	return c.count, c.countOK
}

func (c *countingLabelSource) ResolveLabelBitmap(string) *roaring64.Bitmap {
	c.bitmapCalls++
	if c.nilBitmap {
		return nil
	}
	return roaring64.New()
}

// TestResolveLabelPopulation_PrefersTheZeroAllocCount is the efficiency gate on
// the resolution ORDER. The bitmap route is exact and therefore correct, so a
// version that always took it would pass every correctness assertion here while
// materialising a corrected bitmap on every estimate — the allocation rmp #2392
// measured at 4.1 GB over one example run.
func TestResolveLabelPopulation_PrefersTheZeroAllocCount(t *testing.T) {
	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	g := buildRangeSkewGraph(t, 200, 10)
	on, _ := statsReorderPair(t, g)
	src := &countingLabelSource{eng: on, count: 200, countOK: true}

	before := probe.counter(statsMetricLabelCountDeclined)
	pop := resolveLabelPopulation(src, "A")
	if !pop.known || pop.n != 200 {
		t.Fatalf("population = {n:%v known:%v}, want a known 200", pop.n, pop.known)
	}
	if src.bitmapCalls != 0 {
		t.Errorf("the exact count answered, yet %d label bitmap(s) were materialised: "+
			"the zero-allocation path is what keeps the estimator off the clone "+
			"rmp #2392 measured at 4.1 GB", src.bitmapCalls)
	}
	if got := probe.counter(statsMetricLabelCountDeclined); got != before {
		t.Errorf("label_count.declined = %d, want unchanged %d: nothing declined", got, before)
	}
}

// TestResolveLabelPopulation_NilBitmapIsAKnownZero pins the defensive branch. The
// production resolver answers an unknown label with an EMPTY bitmap rather than a
// nil one, so this case is unreachable through it — which is exactly why it needs
// an explicit exercise: an interface permits nil, and [labelCardinalityEstimate]
// treats that as a known zero. The two must not disagree.
func TestResolveLabelPopulation_NilBitmapIsAKnownZero(t *testing.T) {
	g := buildRangeSkewGraph(t, 200, 10)
	on, _ := statsReorderPair(t, g)
	src := &countingLabelSource{eng: on, countOK: false, nilBitmap: true}

	pop := resolveLabelPopulation(src, "A")
	if !pop.known || pop.n != 0 {
		t.Errorf("a nil bitmap yielded {n:%v known:%v}, want a KNOWN zero — the same "+
			"answer labelCardinalityEstimate gives, so the estimator and the drain "+
			"cannot disagree about an unknown label", pop.n, pop.known)
	}
	if src.bitmapCalls == 0 {
		t.Error("the bitmap route was never taken, so this test did not reach the " +
			"branch it exists to pin")
	}
}

// TestPopulationFromDrain_OnlyAnExactDrainIsACount pins which drains may be read
// as N. A drain that is not estExact is not a live-node count — it is the
// fallback constant [labelCardinalityEstimate] returns for a nil resolver — and
// reading it as one would put a fabricated zero back on the decision path.
func TestPopulationFromDrain_OnlyAnExactDrainIsACount(t *testing.T) {
	for _, tt := range []struct {
		name      string
		drain     estimate
		wantKnown bool
		wantN     float64
	}{
		{"an exact drain IS the count", estimate{rows: 5000, source: estExact}, true, 5000},
		{"an exact zero is a known zero", estimate{rows: 0, source: estExact}, true, 0},
		{"a fallback drain is not a count", estimate{rows: 5000, source: estFallback}, false, 0},
		{"a stats drain is not a count", estimate{rows: 5000, source: estStats}, false, 0},
		{"a heuristic drain is not a count", estimate{rows: 5000, source: estHeuristic}, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := populationFromDrain(tt.drain)
			if got.known != tt.wantKnown {
				t.Errorf("known = %v, want %v", got.known, tt.wantKnown)
			}
			if got.known && got.n != tt.wantN {
				t.Errorf("n = %v, want %v", got.n, tt.wantN)
			}
		})
	}
}

// TestStatsEstimates_UnknowableCountIsNotAnEmptyLabel is the per-provider form of
// the distinction, and it is what stops the two facts collapsing back together.
//
// An unknowable N and an empty label both end in estFallback with zero rows, so
// the ESTIMATE cannot tell them apart; only the reason can. Both providers are
// exercised, because the range path and the equality path guard N separately.
func TestStatsEstimates_UnknowableCountIsNotAnEmptyLabel(t *testing.T) {
	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	g := buildRangeSkewGraph(t, 200, 10)
	on, _ := statsReorderPair(t, g)
	src := countlessStatsSource{on}

	t.Run("range", func(t *testing.T) {
		noCount0 := probe.counter(statsMetricLookupFallbackNoCount)
		empty0 := probe.counter(statsMetricLookupFallbackEmptyLabel)
		noStat0 := probe.counter(statsMetricLookupFallbackNoStatistic)
		e, _ := statsRangeEstimate(src, "A", "x", stats.OpGt, expr.IntegerValue(100))
		if e.source != estFallback {
			t.Errorf("source = %v, want fallback: N is not knowable here", e.source)
		}
		if got := probe.counter(statsMetricLookupFallbackNoCount); got != noCount0+1 {
			t.Errorf("no_count = %d, want %d: an unobtainable N was not attributed to "+
				"the count", got, noCount0+1)
		}
		if got := probe.counter(statsMetricLookupFallbackEmptyLabel); got != empty0 {
			t.Errorf("empty_label = %d, want unchanged %d: :A holds 200 live nodes and "+
				"is not empty", got, empty0)
		}
		if got := probe.counter(statsMetricLookupFallbackNoStatistic); got != noStat0 {
			t.Errorf("no_statistic = %d, want unchanged %d: (A, x) HAS a statistic",
				got, noStat0)
		}
	})

	t.Run("equality", func(t *testing.T) {
		noCount0 := probe.counter(statsMetricLookupFallbackNoCount)
		// A literal absent from the MCV list, so the path reaches the 1/NDV average
		// and needs N. An MCV hit would answer exactly and never look at N.
		e := statsEqualityEstimate(src, "A", "x", expr.IntegerValue(987654))
		if e.source != estFallback {
			t.Errorf("source = %v, want fallback. estHeuristic with zero rows is what "+
				"the discarded ok flag used to produce: a row count no data supports",
				e.source)
		}
		if e.rows != 0 {
			t.Errorf("rows = %v, want 0 alongside the fallback", e.rows)
		}
		if got := probe.counter(statsMetricLookupFallbackNoCount); got != noCount0+1 {
			t.Errorf("no_count = %d, want %d", got, noCount0+1)
		}
	})
}

// TestStatsFallbackReasons_StalenessIsAttributed drives the one reason the other
// tests cannot reach without real write traffic: a statistic whose accumulated
// writes have closed the firing region. It is the fourth member of the partition.
func TestStatsFallbackReasons_StalenessIsAttributed(t *testing.T) {
	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	const n = 400
	e, _, _ := seedPersonGraph(t, n, 0.30)
	ctx := context.Background()
	if err := e.RefreshStatistics(ctx); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}
	src := statsTestSource(e)
	if est, _ := statsRangeEstimate(src, "Person", "age", stats.OpLt, expr.IntegerValue(30)); est.source != estStats {
		t.Fatalf("fresh: source = %v, want stats", est.source)
	}

	threshold := statsRangeBreakEven - 1.0/float64(statsHistogramBuckets)
	crossAt := int64(threshold*float64(n)) + 1
	for i := int64(0); i < crossAt+2; i++ {
		q := fmt.Sprintf("MATCH (p:Person {name:'name-%d'}) SET p.age = %d", i, 200+i)
		if _, err := e.RunInTx(ctx, q, nil); err != nil {
			t.Fatalf("SET write %d: %v", i, err)
		}
	}

	stale0 := probe.counter(statsMetricLookupFallbackStale)
	est, _ := statsRangeEstimate(statsTestSource(e), "Person", "age", stats.OpLt, expr.IntegerValue(30))
	if est.source != estFallback {
		t.Fatalf("after crossing the staleness threshold: source = %v, want fallback", est.source)
	}
	if got := probe.counter(statsMetricLookupFallbackStale); got != stale0+1 {
		t.Errorf("stale = %d, want %d: a staleness demotion carried no reason, so the "+
			"reason counters no longer partition the total", got, stale0+1)
	}
}

// TestQueryPath_ReusesTheDrainRatherThanReResolvingN is the efficiency gate on the
// QUERY path (as opposed to the rendering paths).
//
// The reorder gate already holds the label's live count as the component's drain,
// resolved through whichever route answered. Re-resolving it inside the estimator
// would be behaviourally identical and would build a SECOND corrected bitmap per
// filtered component per query whenever the cheap count declines — which under a
// concurrent writer is the normal state. The declined counter is emitted only by
// [resolveLabelPopulation], so its silence during a run is the proof that the
// query path never reached it.
func TestQueryPath_ReusesTheDrainRatherThanReResolvingN(t *testing.T) {
	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	g := buildRangeSkewGraph(t, 5000, 2000)
	on, _ := statsReorderPair(t, g)
	liveHistory(t, g, "zz-drain-reuse")
	const q = "MATCH (a:A) WHERE a.x > 4900 MATCH (b:B {y: 7}) RETURN a.x AS ax, b.y AS bv"

	// Warm the plan cache, so the measured run is a steady-state execution.
	sortedRows(t, on, q)

	before := probe.counter(statsMetricLabelCountDeclined)
	reorders := joinReorderBuildCount.Load()
	sortedRows(t, on, q)
	if joinReorderBuildCount.Load() == reorders {
		t.Fatal("the reorder did not fire, so this run never consulted the statistics " +
			"and the measurement below is vacuous")
	}
	if got := probe.counter(statsMetricLabelCountDeclined); got != before {
		t.Errorf("label_count.declined rose by %d during one execution: the query path "+
			"re-resolved N instead of reusing the drain, which builds a second "+
			"corrected bitmap per filtered component per query", got-before)
	}
}

// TestStatsMetricNames_AreStable pins the wire names. The constants are what the
// tests reference, so a renamed value changes what every scrape and dashboard sees
// while every assertion in this package still passes.
func TestStatsMetricNames_AreStable(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
		{statsMetricLookup, "cypher.stats.lookup"},
		{statsMetricLookupFallback, "cypher.stats.lookup.fallback"},
		{statsMetricLookupFallbackNoStatistic, "cypher.stats.lookup.fallback.no_statistic"},
		{statsMetricLookupFallbackEmptyLabel, "cypher.stats.lookup.fallback.empty_label"},
		{statsMetricLookupFallbackNoCount, "cypher.stats.lookup.fallback.no_count"},
		{statsMetricLookupFallbackStale, "cypher.stats.lookup.fallback.stale"},
		{statsMetricLabelCountDeclined, "cypher.stats.label_count.declined"},
	} {
		if tt.got != tt.want {
			t.Errorf("metric name = %q, want %q. docs/metrics.md documents the wanted "+
				"name; renaming the value silently breaks every existing scrape",
				tt.got, tt.want)
		}
	}
}

// TestStatsFreeEngine_EmitsNothingFromTheReorderGate pins the zero-cost contract
// on an engine that never refreshed statistics.
//
// The reorder gate reaches the providers through the caller-supplied-N entry
// points, which is a SECOND way into them, so the collector-present guard has to
// be honoured on both. Without it a stats-free engine counts a lookup against a
// collector it does not have — the observability surface is supposed to be
// invisible until statistics exist.
func TestStatsFreeEngine_EmitsNothingFromTheReorderGate(t *testing.T) {
	probe := newStatsMetricProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	g := buildRangeSkewGraph(t, 2000, 200)
	// Deliberately NOT statsReorderPair: no RefreshStatistics, so the lazy
	// collector is never allocated.
	eng := NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	if eng.StatsTrackedPairs() != 0 {
		t.Fatal("this engine already holds statistics, so it is not the stats-free " +
			"case the guard exists for")
	}
	const q = "MATCH (a:A) WHERE a.x > 1900 MATCH (b:B {y: 7}) RETURN a.x AS ax, b.y AS bv"
	sortedRows(t, eng, q)

	if got := probe.counter(statsMetricLookup); got != 0 {
		t.Errorf("cypher.stats.lookup = %d on a stats-free engine, want 0: a lookup was "+
			"counted against a collector that does not exist", got)
	}
	if got := probe.counter(statsMetricLookupFallback); got != 0 {
		t.Errorf("cypher.stats.lookup.fallback = %d on a stats-free engine, want 0", got)
	}
	if got := probe.counter(statsMetricLabelCountDeclined); got != 0 {
		t.Errorf("cypher.stats.label_count.declined = %d on a stats-free engine, want 0: "+
			"nothing should have resolved a population at all", got)
	}
}
