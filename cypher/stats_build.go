package cypher

// stats_build.go — the off-write-path statistics rebuild scan (tasks #2097 /
// #2098, design docs/statistics-design.md §2). [Engine.RefreshStatistics] is the
// single, explicit entry point: it performs one consistent O(V·labels·props) read
// scan under the graph's read barrier, builds fresh NDV / MCV / equi-depth-
// histogram estimators per (label, property), stamps each with the current
// generation and the exact live label count (g0, N0), and publishes them in one
// atomic snapshot swap. It spawns no goroutine — absence or staleness of a
// statistic is harmless (a consumer falls back to its exact-count plan), so a
// bounded, caller-driven rebuild is preferred over any background worker.

import (
	"context"
	"sort"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/stats"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// statsScanPollMask sets the cancellation-check granularity of the rebuild scan:
// ctx is polled every 4096 nodes, matching the CREATE INDEX backfill loops.
const statsScanPollMask = 0xFFF

// RefreshStatisticsLocked is [Engine.RefreshStatistics] for a caller that ALREADY holds
// the visibility barrier — specifically db.stats.refresh(), which runs inside query
// execution (#2196).
//
// It exists because visMu is a non-re-entrant sync.RWMutex: taking it again from a
// goroutine already inside Graph.View would DEADLOCK the engine. The re-entrancy guard
// turns that into a panic, but only in a debug or race build — a production binary would
// hang. So the barrier-taking and barrier-free entry points must be distinct, and the
// caller has to pick correctly.
//
// Correctness is unchanged: the scan only reads, and the caller's read barrier already
// pins the consistent snapshot it needs.
func (e *Engine) RefreshStatisticsLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	start := time.Now()
	byKey, labelN, generation, scanErr := e.scanStatsLocked(ctx)
	if scanErr != nil {
		return scanErr
	}
	e.statsCollectorOrInit().Publish(finishStatsSnapshot(byKey, labelN, generation))
	cmetrics.IncCounter(statsMetricRefresh, 1)
	cmetrics.ObserveLatency(statsMetricRefreshLatency, time.Since(start))
	return nil
}

// RefreshStatistics rebuilds the planner statistics for every (label, property)
// pair currently present on a live node, publishing a fresh snapshot atomically.
// It is the explicit maintenance entry point: statistics are best-effort and never
// maintained by a background goroutine, so a caller (a maintenance task, a
// scheduled job, or a test) drives the rebuild.
//
// The scan resolves against one pinned snapshot, so it observes a consistent
// instant and does not block concurrent writers (which serialise elsewhere). It
// takes no barrier — see the note on the internal builder below for why wrapping
// it in the old lpg.Graph.View would not have given the property it claimed.
//
// Statistics built
// here ship INERT: no query-path consumer reads them yet (#2099 is the intended
// consumer), so a rebuild changes no plan. It honours context cancellation,
// returning ctx.Err() without publishing a partial snapshot.
func (e *Engine) RefreshStatistics(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	start := time.Now()
	snap, err := e.buildStatsSnapshot(ctx)
	if err != nil {
		return err
	}
	// Lazily allocate the collector on this first successful rebuild (task #2101):
	// a stats-free engine never reaches here, so it never constructs a collector.
	// A cancelled or failed build returns above without publishing, leaving the
	// collector nil.
	e.statsCollectorOrInit().Publish(snap)
	// Observability (#2102): count the successful rebuild and observe its latency.
	// A cancelled or failed build returned above without publishing and is not
	// counted. RefreshStatistics is caller-driven and off the write path, so this
	// emission never perturbs a write (BenchmarkEngWriteAutocommit stays flat).
	cmetrics.IncCounter(statsMetricRefresh, 1)
	cmetrics.ObserveLatency(statsMetricRefreshLatency, time.Since(start))
	return nil
}

// statsFreqCell is one distinct value and its exact row count within a
// (label, property) accumulator. Values that fold to the same equivalence hash
// are chained and disambiguated by expr.Equivalent, so a hash collision never
// merges two distinct values.
type statsFreqCell struct {
	value expr.Value
	count int64
}

// statsAccum accumulates the raw material for one (label, property)'s estimators
// during the scan: the running NDV sketch and the exact per-value frequencies
// (which feed both the MCV top-k and the equi-depth histograms). It is built
// single-threaded during the scan and consumed once at finalisation.
type statsAccum struct {
	ndv  *stats.HLL
	freq map[uint64][]statsFreqCell
}

func newStatsAccum() *statsAccum {
	return &statsAccum{ndv: stats.NewHLL(), freq: make(map[uint64][]statsFreqCell)}
}

// feed folds one present property value into the accumulator. Every non-null
// value updates the NDV sketch (NaN included — it is a distinct value). NaN is
// excluded from the frequency table, because it drives neither an equality match
// (= NaN is false) nor a range numerator (a range predicate yields null on NaN).
func (a *statsAccum) feed(v expr.Value) {
	h := expr.EquivalentHash(v)
	a.ndv.Insert(h)
	if isNaNValue(v) {
		return
	}
	chain := a.freq[h]
	for i := range chain {
		if statsEquivalent(chain[i].value, v) {
			chain[i].count++
			a.freq[h] = chain
			return
		}
	}
	a.freq[h] = append(chain, statsFreqCell{value: v, count: 1})
}

// finalize turns the accumulator into an immutable Stats bundle: the exact top-k
// MCV (min-heap over the frequencies), one equi-depth histogram per comparable
// value-domain with the MCV values isolated into singleton buckets, and the
// (g0, N0) stamp.
func (a *statsAccum) finalize(generation uint64, labelCount int64) *stats.Stats[expr.Value] {
	// Flatten the frequency table into MCV entries and select the exact top-k.
	var entries []stats.MCVEntry[expr.Value]
	for h, chain := range a.freq {
		for i := range chain {
			entries = append(entries, stats.MCVEntry[expr.Value]{
				Value: chain[i].value,
				Hash:  h,
				Count: chain[i].count,
			})
		}
	}
	mcv := stats.BuildTopK(entries, statsMCVSize)

	// The MCV values are the heavy values the histogram isolates into singleton
	// buckets so the 1/B bound survives skew (design §1). Over-isolating on a hash
	// collision is harmless (one extra singleton bucket, still correct).
	heavy := make(map[uint64]struct{}, mcv.Len())
	for _, ent := range mcv.Entries() {
		heavy[ent.Hash] = struct{}{}
	}
	isHeavy := func(v expr.Value) bool {
		_, ok := heavy[expr.EquivalentHash(v)]
		return ok
	}

	// Partition the distinct values by comparable domain and build one histogram
	// each. NaN is already absent from freq, so a numeric histogram never counts a
	// row a range predicate would yield null on (design §5.2).
	byDomain := map[stats.Domain][]statsFreqCell{}
	for _, chain := range a.freq {
		for i := range chain {
			if dom, ok := statsDomainOf(chain[i].value); ok {
				byDomain[dom] = append(byDomain[dom], chain[i])
			}
		}
	}
	hists := make(map[stats.Domain]*stats.Histogram[expr.Value], len(byDomain))
	for dom, cells := range byDomain {
		sort.Slice(cells, func(i, j int) bool {
			return statsCompare(cells[i].value, cells[j].value) < 0
		})
		values := make([]expr.Value, len(cells))
		freqs := make([]int64, len(cells))
		for i := range cells {
			values[i] = cells[i].value
			freqs[i] = cells[i].count
		}
		hists[dom] = stats.BuildEquiDepth(values, freqs, statsHistogramBuckets, isHeavy)
	}

	return stats.NewStats(stats.Input[expr.Value]{
		NDV:        a.ndv,
		MCV:        mcv,
		Histograms: hists,
		Generation: generation,
		LabelCount: labelCount,
		Buckets:    statsHistogramBuckets,
	})
}

// buildStatsSnapshot runs the statistics scan and returns the fresh
// per-(label, property) statistics map, ready for Publish. It captures the graph
// generation and each label's exact live count once, so the (g0, N0) stamps come
// from the same pass as the scanned rows.
//
// It takes NO BARRIER. This used to wrap the scan in lpg.Graph.View and claim the
// result was a consistent read of a graph in which nothing is half-applied. THAT WAS
// FALSE: Graph.View acquires visMu SHARED, and since rmp #2304 an ordinary write holds
// it shared too, so the two never excluded each other. The View bought no consistency
// this scan did not already have — as [Engine.scanStatsLocked] states for the in-query
// caller, the scan reads the PRESENT while writers may be mid-apply.
//
// That is tolerable here for the reason given there and nowhere else: these are
// best-effort APPROXIMATE planner statistics, consumed only as cardinality estimates,
// so a torn estimate costs the planner slightly stale numbers and can never make a
// query return a wrong answer. A scan that needed real consistency would take an MVCC
// snapshot; that is a deliberate future change, not something the View was providing.
func (e *Engine) buildStatsSnapshot(ctx context.Context) (map[stats.Key]*stats.Stats[expr.Value], error) {
	byKey, labelN, generation, scanErr := e.scanStatsLocked(ctx)
	if scanErr != nil {
		return nil, scanErr
	}

	return finishStatsSnapshot(byKey, labelN, generation), nil
}

// finishStatsSnapshot converts the scan's accumulators into the published snapshot. It is
// shared by the barrier-taking and barrier-free refresh paths so both produce an identical
// snapshot from identical input (#2196).
func finishStatsSnapshot(
	byKey map[stats.Key]*statsAccum, labelN map[uint32]int64, generation uint64,
) map[stats.Key]*stats.Stats[expr.Value] {
	out := make(map[stats.Key]*stats.Stats[expr.Value], len(byKey))
	for k, acc := range byKey {
		out[k] = acc.finalize(generation, labelN[k.Label])
	}
	return out
}

// scanStatsLocked is the body of the statistics scan, WITHOUT acquiring the visibility
// barrier: the caller must already hold it (#2196).
//
// The split exists because the scan has two callers with opposite needs.
// [Engine.RefreshStatistics] is invoked from outside any barrier and must take one, so it
// wraps this in Graph.View. db.stats.refresh() runs INSIDE query execution and must NOT,
// because visMu is not re-entrant: a second acquisition from the same goroutine deadlocks
// the engine. The re-entrancy guard catches that as a panic, but only in a debug/race
// build; a production binary would simply hang.
//
// # What the in-query caller actually holds (rmp #2290, #2304)
//
// This used to say that caller was "already inside Graph.View", and it no longer is:
// [Engine.Run] has taken no barrier since rmp #2290 — it reads through an MVCC snapshot —
// and rmp #2304 made ordinary writes shared, so nothing on the read path excludes a writer
// any more. An in-query refresh therefore scans the PRESENT while writers may be
// mid-apply, and can accumulate an estimate from a transaction that is not yet visible.
//
// That is tolerable here and nowhere else: these are the best-effort APPROXIMATE planner
// statistics (docs/statistics-design.md), consumed only as cardinality estimates. A torn
// estimate makes the planner choose from slightly stale numbers, which is what an
// approximate statistic is for; it can never make a query return a wrong answer.
//
// It applies to BOTH callers. This used to end by saying the caller-driven
// [Engine.RefreshStatistics] still takes the exclusive View and so still scans a graph in
// which nothing is half-applied — which contradicted the paragraph above it and was
// false twice over: View acquires visMu SHARED, not exclusively, and a shared holder
// excludes neither an ordinary write nor anything else. That View has been removed, so
// the two paths now scan identically and the "Locked" suffix records history rather than
// a lock.
//
// Publishing the result is an atomic pointer swap on engine state, not graph state, so it
// is safe with or without a barrier.
func (e *Engine) scanStatsLocked(ctx context.Context) (
	byKey map[stats.Key]*statsAccum, labelN map[uint32]int64, generation uint64, scanErr error,
) {
	g := e.g
	byKey = make(map[stats.Key]*statsAccum)
	labelN = make(map[uint32]int64)
	generation = g.TopoGeneration()
	mapper := g.AdjList().Mapper()
	reg := g.Registry()
	pk := g.PropertyKeys()
	nodeIdx := g.NodeIndex()

	i := 0
	mapper.Walk(func(id graph.NodeID, key string) bool {
		if i&statsScanPollMask == 0 {
			if err := ctx.Err(); err != nil {
				scanErr = err
				return false
			}
		}
		i++
		if g.IsTombstoned(id) {
			return true
		}
		labels := g.NodeLabels(key)
		if len(labels) == 0 {
			return true
		}
		props := g.NodeProperties(key)
		if len(props) == 0 {
			return true
		}
		for _, lname := range labels {
			lid, ok := reg.Lookup(lname)
			if !ok {
				continue
			}
			lidU := uint32(lid)
			if _, seen := labelN[lidU]; !seen {
				labelN[lidU] = int64(nodeIdx.Count(lidU))
			}
			for pname, pv := range props {
				pid, ok := pk.Lookup(pname)
				if !ok {
					continue
				}
				v := lpgPropToExpr(pv)
				if expr.IsNull(v) {
					continue
				}
				k := stats.Key{Label: lidU, Prop: uint32(pid)}
				acc := byKey[k]
				if acc == nil {
					acc = newStatsAccum()
					byKey[k] = acc
				}
				acc.feed(v)
			}
		}
		return true
	})
	return byKey, labelN, generation, scanErr
}
