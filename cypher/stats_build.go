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

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/stats"
)

// statsScanPollMask sets the cancellation-check granularity of the rebuild scan:
// ctx is polled every 4096 nodes, matching the CREATE INDEX backfill loops.
const statsScanPollMask = 0xFFF

// RefreshStatistics rebuilds the planner statistics for every (label, property)
// pair currently present on a live node, publishing a fresh snapshot atomically.
// It is the explicit maintenance entry point: statistics are best-effort and never
// maintained by a background goroutine, so a caller (a maintenance task, a
// scheduled job, or a test) drives the rebuild.
//
// The scan runs under [lpg.Graph.View], so it observes one consistent snapshot and
// does not block concurrent writers (which serialise elsewhere). Statistics built
// here ship INERT: no query-path consumer reads them yet (#2099 is the intended
// consumer), so a rebuild changes no plan. It honours context cancellation,
// returning ctx.Err() without publishing a partial snapshot.
func (e *Engine) RefreshStatistics(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	snap, err := e.buildStatsSnapshot(ctx)
	if err != nil {
		return err
	}
	// Lazily allocate the collector on this first successful rebuild (task #2101):
	// a stats-free engine never reaches here, so it never constructs a collector.
	// A cancelled or failed build returns above without publishing, leaving the
	// collector nil.
	e.statsCollectorOrInit().Publish(snap)
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

// buildStatsSnapshot performs the consistent read scan and returns the fresh
// per-(label, property) statistics map, ready for Publish. It captures the graph
// generation and each label's exact live count once, inside the View, so the
// (g0, N0) stamps are consistent with the scanned snapshot.
func (e *Engine) buildStatsSnapshot(ctx context.Context) (map[stats.Key]*stats.Stats[expr.Value], error) {
	g := e.g
	byKey := make(map[stats.Key]*statsAccum)
	labelN := make(map[uint32]int64)
	var generation uint64
	var scanErr error

	g.View(func() {
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
	})
	if scanErr != nil {
		return nil, scanErr
	}

	out := make(map[stats.Key]*stats.Stats[expr.Value], len(byKey))
	for k, acc := range byKey {
		out[k] = acc.finalize(generation, labelN[k.Label])
	}
	return out, nil
}
