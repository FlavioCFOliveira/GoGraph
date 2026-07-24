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
// As of #2097/#2098 these providers ship INERT: nothing on the query path calls
// them yet. #2099 is the sole intended consumer — it will widen the selective
// range-index seek to fire under the §3 upper-confidence-bound rule when a fresh
// estStats range estimate is available. Until then the providers change no plan
// and are proven correct in isolation by stats_estimate_test.go, the same way the
// count-estimate provider shipped inert before its peephole consumed it.

import (
	"math"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	labelidx "github.com/FlavioCFOliveira/GoGraph/graph/index/label"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/stats"
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
// registry, and the exact live label count (the denominator N of every
// selectivity). The production *lpgLabelResolver implements it.
type statsSource interface {
	Statistics() *statsCollector
	ResolveLabelID(name string) (uint32, bool)
	ResolvePropertyID(name string) (uint32, bool)
	ResolveLabelCount(name string) (int64, bool)
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

// statsEqualityEstimate estimates the selectivity of n.<prop> = literal for a
// node of label, as an absolute row count.
//
// An MCV hit yields the EXACT per-value count (estExact — effectively a maintained
// exact for that literal). Otherwise the estimate is the distribution-average
// 1/NDV × N, tagged estHeuristic: a NON-gating hint, because a specific literal's
// true frequency can be arbitrarily far from N/NDV under skew, so 1/NDV can never
// certify a no-regression equality decision (design §4). A missing statistic, or
// a NaN literal (=(NaN) matches nothing), yields the safe estimate.
func statsEqualityEstimate(src statsSource, label, prop string, literal expr.Value) estimate {
	st, ok := lookupStats(src, label, prop)
	if !ok {
		return estimate{rows: 0, source: estFallback}
	}
	if isNaNValue(literal) {
		// `= NaN` is false for every row (NaN = NaN → false); exactly zero rows.
		return estimate{rows: 0, source: estExact}
	}
	h := expr.EquivalentHash(literal)
	if cnt, hit := st.MCV.Lookup(h, literal, statsEquivalent); hit {
		return estimate{rows: float64(cnt), source: estExact}
	}
	// 1/NDV heuristic. Round D̂ to an integer ≥ 1 and guard 1/D̂ (design §5.3).
	d := math.Round(st.NDV.Estimate())
	if d < 1 {
		d = 1
	}
	n, _ := src.ResolveLabelCount(label)
	rows := (1.0 / d) * float64(n)
	if rows < 0 {
		rows = 0
	}
	return estimate{rows: rows, source: estHeuristic}
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
func statsRangeEstimate(src statsSource, label, prop string, op stats.Op, bound expr.Value) (estimate, float64) {
	invB := 1.0 / float64(statsHistogramBuckets)
	st, ok := lookupStats(src, label, prop)
	if !ok {
		return estimate{rows: 0, source: estFallback}, invB
	}
	dom, ok := statsDomainOf(bound)
	if !ok || isNaNValue(bound) {
		// No histogram domain, or a NaN bound (every comparison yields null → no
		// rows). Not a trustworthy range estimate.
		return estimate{rows: 0, source: estFallback}, invB
	}
	h, ok := st.Histogram(dom)
	if !ok {
		return estimate{rows: 0, source: estFallback}, invB
	}
	n, _ := src.ResolveLabelCount(label)
	if n <= 0 {
		// Guard the Δ/N denominator; with no live rows there is nothing to seek.
		return estimate{rows: 0, source: estFallback}, invB
	}

	sHat := h.Selectivity(bound, op, statsCompare) // ∈ [0,1], clamped
	staleErr := float64(st.Delta()) / float64(n)
	absErr := invB + staleErr

	// In-range absolute count = Ŝ over the in-domain rows the histogram summarises,
	// clamped to the label's live count.
	rows := sHat * float64(h.Total())
	if rows < 0 {
		rows = 0
	}
	if rows > float64(n) {
		rows = float64(n)
	}

	// Staleness → veto (design §3): the firing region Ŝ ≤ b − 1/B − Δ/N closes as
	// Δ grows; demote once Δ/N ≥ b − 1/B, or once deletes make NDV/histogram
	// untrustworthy. A demoted estimate keeps its computed rows but is tagged
	// estFallback so the trustworthiness veto keeps the default plan.
	source := estStats
	if staleErr >= statsRangeBreakEven-invB || st.NeedsRebuildForDeletes(statsDeleteRebuildTol) {
		source = estFallback
	}
	return estimate{rows: rows, source: source}, absErr
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
