package cypher

// stats_estimate.go — the approximate-statistics estimate providers (tasks #2097
// / #2098, design docs/statistics-design.md). It turns the best-effort NDV / MCV
// / equi-depth-histogram statistics maintained in graph/index/stats into
// estimate-provenance values (estimate.go), exactly as count_estimate.go does for
// the exact E/D/T counts.
//
// It is the layer that owns the openCypher value semantics the pure stats package
// deliberately does not: the orderability comparator (expr.Compare, whose
// cross-type Integer/Float boundary path is the exact cmpInt64Float64 per
// CIP2016-06-14), the equivalence-consistent hash and equality
// (expr.EquivalentHash / expr.Equivalent, which fold numerically-equal
// Integer/Float, ±0.0 and all NaN bit-patterns together), the exclusion of NaN
// rows from range numerators, and the one-histogram-per-comparable-value-domain
// split.
//
// These providers shipped INERT in #2097/#2098 and stopped being inert in rmp
// #2766. Two things read them now, and only one of them can change a plan:
//
//   - explain_estimate.go annotates the rendered EXPLAIN / PROFILE plan with the
//     estimate and its provenance (#2099 / rmp #2765) — display only.
//   - join_reorder_plan.go consults them for the emitted-row cardinality of a
//     FILTERED component, and may drive a disjoint Cartesian join with the other
//     arm as a result. That decision is vetoed by [planStaysDefault] for every
//     verdict but a FRESH MCV-exact equality or a fresh histogram range, and for the
//     latter it is taken over the certified error interval rather than the point
//     estimate.
//
// The selective range-index seek #2099 was to widen under the §3 upper-confidence-
// bound rule is still NOT wired to them: the seek's own gate remains the exact
// in-range count. The providers are also proven correct in isolation by
// stats_estimate_test.go, independently of either consumer.

import (
	"math"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	labelidx "github.com/FlavioCFOliveira/GoGraph/graph/index/label"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/stats"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Statistics parameters (design docs/statistics-design.md §1).
const (
	// statsHistogramBuckets is B, the equi-depth bucket count. Its reciprocal 1/B
	// is the distribution-free certified absolute selectivity error.
	statsHistogramBuckets = 256
	// statsMCVSize is k, the number of exact most-common values kept per column.
	statsMCVSize = 32
	// statsRangeBreakEven is b, the CSR random-vs-sequential ratio below which a
	// selective range seek is a no-regression win (the exact-count peephole used
	// 0.10). #2099 calibrates it empirically; it is used here only to place the
	// staleness-demotion threshold b − 1/B, and nothing gates on it yet.
	statsRangeBreakEven = 0.10
	// statsDeleteRebuildTol is the fraction of the build-time label count of
	// deletes past which the HLL's inability to delete makes its NDV untrustworthy
	// and a rebuild is due.
	statsDeleteRebuildTol = 0.01
)

// The comparable value-domains a property's values are partitioned into. A single
// histogram is only meaningful within one domain, because a single orderability
// comparator must totally order its boundaries (design §5.2). Numeric unifies
// Integer and Float (openCypher orders them as one Number tier); String is the
// plain-string domain (temporal values decode to their own Kinds and are never
// plain strings, matching the btree string-index projection gate).
const (
	statsDomainNumeric stats.Domain = iota
	statsDomainString
)

// statsCollector is the concrete Collector type the engine holds: per-(label,
// property) statistics keyed by interned ids, over openCypher values.
type statsCollector = stats.Collector[expr.Value]

// statsSource is the capability the statistics providers need from a resolver:
// the engine's statistics collector, stable name→id resolution against the shared
// registry, and the live label count (the denominator N of every selectivity).
// The production *lpgLabelResolver implements it.
//
// ResolveLabelCount is EXACT-OR-NOTHING and its bool is load-bearing: it declines
// whenever any MVCC history is live. Never read it as `n, _ :=` — see
// [labelPopulation] and [resolveLabelPopulation] for what a decline costs and how
// N is resolved instead (rmp #2771).
type statsSource interface {
	Statistics() *statsCollector
	ResolveLabelID(name string) (uint32, bool)
	ResolvePropertyID(name string) (uint32, bool)
	ResolveLabelCount(name string) (int64, bool)
}

// labelPopulation is N — the number of live nodes carrying a label — together
// with whether that number is KNOWN. It is the denominator of every selectivity
// and of the staleness fraction Δ/N (docs/statistics-design.md §0, §3).
//
// # Why the flag exists (rmp #2771)
//
// The design's §0 asserts that "the denominator N is exact", and it is not. The
// exact live count comes from [lpgLabelResolver.ResolveLabelCount], which delegates
// to [lpg.Graph.LabelCountExact] — exact-or-nothing, declining the moment any MVCC
// history is live, because a count has no object to be re-checked against (rmp
// #2290). The estimator took that as `n, _ :=`, so a DECLINE arrived as the number
// ZERO and its `n <= 0` guard demoted the whole estimate to estFallback.
//
// A decline and an empty label are different facts and must not share a
// representation: an empty label really has no rows to be selective over, while a
// decline says nothing about the population at all. Conflating them made the
// planner's use of its own statistics depend on unrelated concurrent activity —
// the same query on the same graph reordered or did not — while EXPLAIN, which
// resolves through a PRESENT-TIME resolver that does not decline, rendered the
// swap either way.
//
// A zero value is the honest "not known", so a caller that forgets to fill it in
// gets the safe demotion rather than a fabricated zero.
type labelPopulation struct {
	// n is the live-node count for the label. It is meaningful only when known.
	n float64
	// known reports whether n was obtained at all.
	known bool
}

// resolveLabelPopulation resolves N for label, and is the ONLY place the exact
// count's decline is handled.
//
// It tries the zero-allocation exact count first. On a DECLINE it falls back to
// the cardinality of the label bitmap the resolver returns, which for the
// resolver's own view is the CORRECTED, exact live count — the same number
// [labelCardinalityEstimate] produces for the drain, so the estimator and the
// component drain cannot disagree about how many rows a label holds.
//
// # Why the bitmap and not a bound
//
// [labelBounder.ResolveLabelCountBound] answers the planner's threshold screens for
// three atomic loads (rmp #2392) and is the WRONG instrument here: N is the
// DENOMINATOR of the staleness fraction, so an upper bound over-states N,
// under-states Δ/N, and would keep the planner trusting a statistic it should have
// demoted. Only a lower bound or the exact count is sound, and a lower bound loose
// enough to be cheap (raw minus the suspect count) collapses towards zero under
// exactly the concurrent activity that makes it necessary — which reinstates the
// non-determinism rather than removing it. The exact count is therefore the only
// candidate, and the bitmap is where it comes from when the cheap path declines.
//
// # What it costs, and who pays
//
// The fallback materialises a corrected bitmap, which is not free — rmp #2392
// measured that pattern at 4.1 GB over one run of examples/35_mvcc_mixed_workload
// when a planner gate reached for it once per query. The QUERY path does not pay
// it: [reorderFilteredRows] supplies N from the component drain, which has already
// resolved the same number by the same route ([populationFromDrain]). This
// function is reached only from the RENDERING paths — EXPLAIN and PROFILE — where
// one bitmap per rendered Selection is the price of showing a reader the estimate
// the engine actually used.
func resolveLabelPopulation(src statsSource, label string) labelPopulation {
	if src == nil {
		return labelPopulation{}
	}
	if n, ok := src.ResolveLabelCount(label); ok {
		return labelPopulation{n: float64(n), known: true}
	}
	cmetrics.IncCounter(statsMetricLabelCountDeclined, 1) // observability (rmp #2771)
	r, ok := src.(labelResolverIface)
	if !ok {
		// Neither an exact count nor a bitmap. N is genuinely unknown, and the
		// caller must demote rather than invent one.
		return labelPopulation{}
	}
	bm := r.ResolveLabelBitmap(label)
	if bm == nil {
		// An unknown label resolves to the empty bitmap (zero live nodes); a nil
		// bitmap is treated the same, exactly as [labelCardinalityEstimate] does.
		return labelPopulation{n: 0, known: true}
	}
	return labelPopulation{n: float64(bm.GetCardinality()), known: true}
}

// populationFromDrain reads N off a component's DRAIN estimate, which is the
// label's live-node count as [labelCardinalityEstimate] resolved it.
//
// It is what keeps the fix free on the query path: the drain has already paid for
// whichever route answered — the zero-alloc count, or the bitmap when that
// declined — so the estimator reuses that number instead of resolving it a second
// time. Only an estExact drain is accepted; anything else (a nil resolver) is not
// a count and yields the honest "not known".
func populationFromDrain(drain estimate) labelPopulation {
	if drain.source != estExact {
		return labelPopulation{}
	}
	return labelPopulation{n: drain.rows, known: true}
}

// statsFallbackReason names WHY a provider returned estFallback, so the
// observability surface can say which of the causes fired rather than only that
// one did (rmp #2771). statsFallbackNone is the non-fallback case and emits
// nothing.
type statsFallbackReason uint8

const (
	statsFallbackNone statsFallbackReason = iota
	// statsFallbackNoStatistic: no usable statistic for the (label, property,
	// domain) the predicate asks about.
	statsFallbackNoStatistic
	// statsFallbackEmptyLabel: N is known and is zero — the label holds no live
	// nodes, so there is no population to be selective over.
	statsFallbackEmptyLabel
	// statsFallbackNoCount: N is not knowable for the resolver in hand.
	statsFallbackNoCount
	// statsFallbackStale: the statistic exists and N is known, but the statistic
	// has drifted past the firing region or its deletes exceed the rebuild
	// tolerance.
	statsFallbackStale
)

// metric returns the counter name that records this reason. Every estFallback
// verdict carries exactly one reason, so the four names partition the total
// [statsMetricLookupFallback].
func (r statsFallbackReason) metric() string {
	switch r {
	case statsFallbackNoStatistic:
		return statsMetricLookupFallbackNoStatistic
	case statsFallbackEmptyLabel:
		return statsMetricLookupFallbackEmptyLabel
	case statsFallbackNoCount:
		return statsMetricLookupFallbackNoCount
	case statsFallbackStale:
		return statsMetricLookupFallbackStale
	default:
		return ""
	}
}

// statsCompare is the orderability comparator the histogram uses over openCypher
// values. It is expr.Compare, whose cross-type Integer/Float path is the exact
// cmpInt64Float64 (CIP2016-06-14) — never a raw IEEE `<`. Callers pass only
// non-null, non-NaN, single-domain values, so the total order is well-defined.
func statsCompare(a, b expr.Value) int { return expr.Compare(a, b) }

// statsEquivalent is the equivalence relation the MCV lookup confirms a hash hit
// with (expr.Equivalent — ±0.0 equal, NaN ≡ NaN, exact Integer/Float).
func statsEquivalent(a, b expr.Value) bool { return expr.Equivalent(a, b) }

// statsDomainOf reports the histogram domain a value belongs to, and false when
// the value is in no histogram domain (Bool, temporal, list — still counted in
// NDV/MCV, but not range-summarised).
func statsDomainOf(v expr.Value) (stats.Domain, bool) {
	switch v.Kind() {
	case expr.KindInteger, expr.KindFloat:
		return statsDomainNumeric, true
	case expr.KindString:
		return statsDomainString, true
	default:
		return 0, false
	}
}

// isNaNValue reports whether v is a floating-point NaN — a row a range predicate
// yields null on, excluded from every histogram numerator (design §5.2).
func isNaNValue(v expr.Value) bool {
	f, ok := v.(expr.FloatValue)
	return ok && math.IsNaN(float64(f))
}

// statsSnapshotFresh reports whether a published statistics snapshot is still
// fresh enough for a verdict a reader — or the planner — may treat as
// trustworthy. It is the module's ONE staleness formulation, and both the
// equality provider and the reorder gate's screen go through it.
//
// The rule is the design's (docs/statistics-design.md §3): demote once the
// accumulated-write fraction Δ/N reaches the firing region b − 1/B, or once the
// accumulated deletes make the HLL's NDV untrustworthy and a rebuild is due.
//
// # The denominator, and why the live count alone is not enough (rmp #2772)
//
// N is the SMALLER of two numbers that are always available:
//
//   - the statistic's own build-time label count ([stats.Stats.LabelCount]), which
//     is recorded in the snapshot and needs no resolver at all; and
//   - the caller's live population, when it is known ([labelPopulation]).
//
// Taking the smaller maximises Δ/N, which is the conservative direction: it demotes
// sooner, never later. The live count ALONE is not sufficient, and the gap is
// measured rather than argued. A bundle built over 1000 `:Person` rows all carrying
// `grp = 'hot'`, grown by 100 further rows that also carry it, has Δ = 100 against a
// live count of 1100: Δ/live = 0.0909 is UNDER the 0.0961 threshold, so the live
// count alone calls the snapshot fresh — and its most-common-value entry then reports
// 1000 rows for a predicate that now matches 1100, rendered as a bare, unmarked
// number. Δ/N0 = 0.1000 is over the threshold and rejects it. That case is
// [TestStatsEqualityFreshness_GrowthAloneDemotesTheMCVCount].
//
// An UPPER bound on the population ([labelBounder.ResolveLabelCountBound], the
// capability built for rmp #2392) is the wrong instrument for the mirror-image
// reason: it over-states N, under-states Δ/N, and would keep the planner trusting a
// statistic it should have demoted (docs/statistics-design.md §0, corrected at rmp
// #2771). Neither number used here is an upper bound — N0 is the exact live count
// stamped at build time, and [labelPopulation] carries an exact live count or the
// honest "not known".
func statsSnapshotFresh(st *stats.Stats[expr.Value], pop labelPopulation) bool {
	if st == nil || st.NeedsRebuildForDeletes(statsDeleteRebuildTol) {
		return false
	}
	n := float64(st.LabelCount())
	if pop.known && pop.n < n {
		n = pop.n
	}
	if n <= 0 {
		// Guard the staleness denominator. Without it the fraction is 0/0 — NaN —
		// and NaN fails every comparison, so a snapshot describing a population that
		// no longer exists would be reported fresh.
		return false
	}
	return float64(st.Delta())/n < statsRangeBreakEven-1.0/float64(statsHistogramBuckets)
}

// statsEqualityEstimate estimates the selectivity of n.<prop> = literal for a
// node of label, as an absolute row count.
//
// An MCV hit yields the EXACT per-value count (estExact — effectively a maintained
// exact for that literal) while the statistic behind it is FRESH, and is demoted to
// estFallback once it is not ([statsSnapshotFresh], rmp #2772). Otherwise the
// estimate is the distribution-average 1/NDV × N, tagged estHeuristic: a NON-gating
// hint, because a specific literal's true frequency can be arbitrarily far from
// N/NDV under skew, so 1/NDV can never certify a no-regression equality decision
// (design §4). A missing statistic yields the safe estimate, and a NaN literal
// yields an exact ZERO that no staleness can touch: `= NaN` is false for every row
// under openCypher, which is a fact of the language and not a reading of the
// statistic.
//
// It wraps the pure estimator with the observability surface (#2102): a lookup
// against a present collector is counted, and an estFallback verdict increments
// the fallback counter and the sibling that names its reason. A stats-free engine
// (no collector) emits nothing.
//
// N is resolved through [resolveLabelPopulation]. A caller that already holds the
// label's live count must use [statsEqualityEstimateWith] instead, so the count is
// resolved once per component rather than once per consumer (rmp #2771).
func statsEqualityEstimate(src statsSource, label, prop string, literal expr.Value) estimate {
	// Guarded before the population is resolved, for the reason given on
	// [statsRangeEstimate].
	if !statsCollectorPresent(src) {
		return estimate{rows: 0, source: estFallback}
	}
	return statsEqualityEstimateWith(src, label, prop, literal, resolveLabelPopulation(src, label))
}

// statsEqualityEstimateWith is [statsEqualityEstimate] against a caller-supplied
// N. It carries its own collector-present guard for the reason given on
// [statsRangeEstimateWith]: it is a second entry point, and the guard is what keeps
// a stats-free engine emitting nothing.
func statsEqualityEstimateWith(src statsSource, label, prop string, literal expr.Value, pop labelPopulation) estimate {
	if !statsCollectorPresent(src) {
		return estimate{rows: 0, source: estFallback}
	}
	cmetrics.IncCounter(statsMetricLookup, 1) // observability (#2102)
	e, why := statsEqualityEstimateInner(src, label, prop, literal, pop)
	return recordStatsFallback(e, why)
}

// statsEqualityEstimateInner is the pure equality estimator; the wrappers above
// add observability. It is called only after the caller has confirmed a present
// collector, so a missing statistic here means the (label, property) pair is
// untracked rather than the engine being stats-free.
//
// It returns the estimate and, when that estimate is estFallback, the reason.
func statsEqualityEstimateInner(src statsSource, label, prop string, literal expr.Value, pop labelPopulation) (estimate, statsFallbackReason) {
	st, ok := lookupStats(src, label, prop)
	if !ok {
		return estimate{rows: 0, source: estFallback}, statsFallbackNoStatistic
	}
	if isNaNValue(literal) {
		// `= NaN` is false for every row (NaN = NaN → false); exactly zero rows.
		return estimate{rows: 0, source: estExact}, statsFallbackNone
	}
	h := expr.EquivalentHash(literal)
	if cnt, hit := st.MCV.Lookup(h, literal, statsEquivalent); hit {
		// The MCV entry is an EXACT per-value count — for the snapshot it was built
		// from, and for no later one. Until rmp #2772 it was returned estExact with no
		// staleness term of any kind, while the sibling range provider demoted a stale
		// histogram, so the two providers disagreed about whether staleness mattered.
		// Since rmp #2765 that tag decides how the figure RENDERS: estExact prints as a
		// bare number with no approximation marker, so an arbitrarily drifted count was
		// presented to the reader as ground truth. Measured on the rmp #2767 fixture:
		// Est.Rows 1000 beside a measured 10, in EXPLAIN and in ProfileTable alike.
		if pop.known && pop.n <= 0 {
			// A label KNOWN to hold no live nodes. Whatever the snapshot recorded for
			// this value, no row can carry it now, and the emptying is invisible to Δ
			// when it was done by removing the LABEL rather than the property.
			return estimate{rows: float64(cnt), source: estFallback}, statsFallbackEmptyLabel
		}
		if !statsSnapshotFresh(st, pop) {
			// A demoted count keeps its number, exactly as [statsRangeEstimateInner]
			// keeps its computed rows: estFallback is what stops the figure being
			// RENDERED and what makes [planStaysDefault] hold the default plan.
			return estimate{rows: float64(cnt), source: estFallback}, statsFallbackStale
		}
		return estimate{rows: float64(cnt), source: estExact}, statsFallbackNone
	}
	// 1/NDV heuristic. Round D̂ to an integer ≥ 1 and guard 1/D̂ (design §5.3).
	d := math.Round(st.NDV.Estimate())
	if d < 1 {
		d = 1
	}
	if !pop.known {
		// N is the scale of the distribution average, so without it there is no
		// number to report. This used to read `n, _ := src.ResolveLabelCount(label)`,
		// which turned a DECLINED count into a fabricated ZERO tagged estHeuristic —
		// a row count no data supports, rendered in EXPLAIN as though it had been
		// derived (rmp #2771).
		return estimate{rows: 0, source: estFallback}, statsFallbackNoCount
	}
	// A label KNOWN to hold zero live nodes keeps the estHeuristic tag it has always
	// had here: the average over an empty population is zero, which is the correct
	// number, and estHeuristic is untrustworthy anyway so no plan turns on it. The
	// range estimator demotes that case instead, because its Δ/N denominator cannot
	// be zero; see [statsRangeEstimateInner].
	rows := (1.0 / d) * pop.n
	if rows < 0 {
		rows = 0
	}
	return estimate{rows: rows, source: estHeuristic}, statsFallbackNone
}

// statsRangeEstimate estimates the selectivity of n.<prop> <op> bound for a node
// of label, returning the absolute row count and the certified absolute error δ =
// 1/B + Δ/N on the selectivity (design §3).
//
// It is estStats when the statistic is fresh, and demotes to estFallback when the
// staleness term Δ/N has closed the firing region (Δ/N ≥ b − 1/B, design §3), when
// the accumulated deletes exceed the rebuild tolerance, or when no histogram
// exists for the bound's domain. Nothing consumes the verdict yet (#2099 will);
// this proves the estimate and its error are correct. The returned rows are
// clamped to [0, N] and (Ŝ + δ) is implicitly clamped to [0,1] by the histogram.
//
// It wraps the pure estimator with the observability surface (#2102): a lookup
// against a present collector is counted, and an estFallback verdict increments
// the fallback counter and the sibling that names its reason. A stats-free engine
// (no collector) emits nothing.
//
// N is resolved through [resolveLabelPopulation]. A caller that already holds the
// label's live count must use [statsRangeEstimateWith] instead, so the count is
// resolved once per component rather than once per consumer (rmp #2771).
func statsRangeEstimate(src statsSource, label, prop string, op stats.Op, bound expr.Value) (estimate, float64) {
	invB := 1.0 / float64(statsHistogramBuckets)
	// Guarded HERE, before the population is resolved: [resolveLabelPopulation] may
	// materialise a bitmap and always emits a counter on a decline, and neither may
	// happen on an engine that holds no statistics at all.
	if !statsCollectorPresent(src) {
		return estimate{rows: 0, source: estFallback}, invB
	}
	return statsRangeEstimateWith(src, label, prop, op, bound, resolveLabelPopulation(src, label))
}

// statsRangeEstimateWith is [statsRangeEstimate] against a caller-supplied N.
//
// It carries its OWN collector-present guard because it is a second door into the
// provider, reached from the reorder gate without passing through
// [statsRangeEstimate]. That guard is what keeps the observability surface
// invisible on an engine that never refreshed statistics, and a guard on one of two
// doors is not a guard. The delegating path therefore checks twice — two interface
// calls, on a path that is about to do a histogram lookup.
func statsRangeEstimateWith(src statsSource, label, prop string, op stats.Op, bound expr.Value, pop labelPopulation) (estimate, float64) {
	invB := 1.0 / float64(statsHistogramBuckets)
	if !statsCollectorPresent(src) {
		return estimate{rows: 0, source: estFallback}, invB
	}
	cmetrics.IncCounter(statsMetricLookup, 1) // observability (#2102)
	e, absErr, why := statsRangeEstimateInner(src, label, prop, op, bound, pop)
	return recordStatsFallback(e, why), absErr
}

// statsRangeEstimateInner is the pure range estimator; the wrappers above add
// observability. It is called only after the caller has confirmed a present
// collector.
//
// It returns the estimate, the certified absolute selectivity error, and — when
// the estimate is estFallback — the reason.
func statsRangeEstimateInner(src statsSource, label, prop string, op stats.Op, bound expr.Value, pop labelPopulation) (estimate, float64, statsFallbackReason) {
	invB := 1.0 / float64(statsHistogramBuckets)
	st, ok := lookupStats(src, label, prop)
	if !ok {
		return estimate{rows: 0, source: estFallback}, invB, statsFallbackNoStatistic
	}
	dom, ok := statsDomainOf(bound)
	if !ok || isNaNValue(bound) {
		// No histogram domain, or a NaN bound (every comparison yields null -> no
		// rows). Not a trustworthy range estimate.
		return estimate{rows: 0, source: estFallback}, invB, statsFallbackNoStatistic
	}
	h, ok := st.Histogram(dom)
	if !ok {
		return estimate{rows: 0, source: estFallback}, invB, statsFallbackNoStatistic
	}
	if !pop.known {
		// N IS NOT KNOWABLE, which is a different fact from the label being empty
		// and must not be conflated with it. This used to read
		// `n, _ := src.ResolveLabelCount(label)`, so a DECLINE — which the exact
		// count issues whenever any MVCC history is live (rmp #2290) — arrived as
		// the number zero and fell into the guard below. That made a plan decision
		// depend on unrelated concurrent activity, and made EXPLAIN, whose
		// present-time resolver does not decline, render a swap the engine would
		// not take (rmp #2771). See [labelPopulation].
		return estimate{rows: 0, source: estFallback}, invB, statsFallbackNoCount
	}
	n := pop.n
	if n <= 0 {
		// Guard the staleness denominator; with no live rows there is nothing to
		// seek. Reached only for a label KNOWN to hold zero live nodes.
		return estimate{rows: 0, source: estFallback}, invB, statsFallbackEmptyLabel
	}

	sHat := h.Selectivity(bound, op, statsCompare) // in [0,1], clamped
	staleErr := float64(st.Delta()) / n
	absErr := invB + staleErr

	// In-range absolute count = the selectivity over the in-domain rows the
	// histogram summarises, clamped to the label's live count.
	rows := sHat * float64(h.Total())
	if rows < 0 {
		rows = 0
	}
	if rows > n {
		rows = n
	}

	// Staleness -> veto (design section 3): the firing region closes as the
	// accumulated-write term grows; demote once it reaches b - 1/B, or once
	// deletes make the NDV and histogram untrustworthy. A demoted estimate keeps
	// its computed rows but is tagged estFallback so the trustworthiness veto
	// keeps the default plan.
	if staleErr >= statsRangeBreakEven-invB || st.NeedsRebuildForDeletes(statsDeleteRebuildTol) {
		return estimate{rows: rows, source: estFallback}, absErr, statsFallbackStale
	}
	return estimate{rows: rows, source: estStats}, absErr, statsFallbackNone
}

// statsCollectorPresent reports whether the source exposes a live statistics
// collector. It is the gate that keeps the observability surface zero-cost on a
// stats-free engine: when no collector exists the providers return estFallback
// without emitting any lookup metric, mirroring the count-store provider, which
// counts a lookup only when a store is present (count_estimate.go).
func statsCollectorPresent(src statsSource) bool {
	return src != nil && src.Statistics() != nil
}

// recordStatsFallback emits the lookup.fallback counter when e is an estFallback
// verdict, together with the sibling counter that names WHY (rmp #2771), and
// returns e unchanged so a provider can `return recordStatsFallback(e, why)`. It
// is called only after a lookup against a present collector has already been
// counted (#2102).
//
// Exactly one reason counter is emitted per estFallback verdict, so the reason
// counters partition the total. A caller that reports estFallback with
// [statsFallbackNone] would break that partition, so the reason is asserted here
// rather than silently dropped: an unnamed reason still counts towards the total
// and is left visible as the gap between the total and the sum of the reasons.
func recordStatsFallback(e estimate, why statsFallbackReason) estimate {
	if e.source != estFallback {
		return e
	}
	cmetrics.IncCounter(statsMetricLookupFallback, 1) // observability (#2102)
	if name := why.metric(); name != "" {
		cmetrics.IncCounter(name, 1) // observability (rmp #2771)
	}
	return e
}

// lookupStats resolves (label, prop) to its statistics bundle, or false when the
// source has no collector, the names are not interned, or no bundle was built.
func lookupStats(src statsSource, label, prop string) (*stats.Stats[expr.Value], bool) {
	if src == nil {
		return nil, false
	}
	c := src.Statistics()
	if c == nil {
		return nil, false
	}
	lid, okL := src.ResolveLabelID(label)
	pid, okP := src.ResolvePropertyID(prop)
	if !okL || !okP {
		return nil, false
	}
	return c.Lookup(lid, pid)
}

// recordStatsNodePropertyWrite is the write-path staleness hook: it bumps Δ (and,
// when a value was replaced or removed, the delete counter) for every tracked
// (label, property) the written node carries. It is called only when the engine
// holds statistics (Collector.Tracking), and is O(tracked-labels-for-prop) atomic
// bitmap checks with no allocation — the single write-path cost the design permits
// (design §2). Attribution over a possibly-replayed write only ever over-counts Δ,
// which closes the firing region sooner (the safe, conservative direction).
func recordStatsNodePropertyWrite(c *statsCollector, nodeIdx *labelidx.Index, id graph.NodeID, propID uint32, hadPrev bool) {
	if c == nil || nodeIdx == nil {
		return
	}
	for _, lid := range c.TrackedLabelsForProp(propID) {
		if !nodeIdx.Has(lid, id) {
			continue
		}
		c.RecordWrite(lid, propID)
		if hadPrev {
			// A replaced or removed value: HLL cannot lower a register, so this is
			// the direction that makes NDV over-estimate.
			c.RecordDelete(lid, propID)
		}
	}
}
