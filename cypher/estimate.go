package cypher

// estimate.go — estimate-provenance infrastructure (#2076, the estimate{rows,
// source} veto model of docs/optimizer-activation-design.md §2.1).
//
// A physical-plan substitution is legal only when it is provably result-
// identical to the default plan AND never slower. The mechanism that enforces
// "never slower" for free during rollout is a trustworthiness VETO on the
// estimates that drive the decision: every estimate carries its provenance
// (estSource), and a decision path that rests on any untrustworthy estimate (a
// fallback constant, or an unvalidated heuristic) is vetoed back to today's
// default plan. Under an estimator that only ever produces fallbacks the
// planner is therefore provably inert; it earns the right to deviate one
// decision at a time exactly as a real, trustworthy statistic comes online.
//
// These types are internal to the cypher package (unexported), like the rest of
// the planner machinery (hashJoinEnabled, tryBuildHashJoin, …). The estimate
// value is a small, copy-by-value struct (two words); no interface{} and no
// interface dispatch sit on any hot path that reads it.

// estSource is the provenance of a cardinality estimate — how trustworthy the
// row count is. Values are ordered exactly as docs/optimizer-activation-design.md
// §2.1 defines them (estExact first). Trust is a classification, not the numeric
// order: estExact and estStats are trustworthy enough to drive a non-default
// plan; estHeuristic and estFallback are not (see [estimate.trustworthy]).
type estSource uint8

const (
	// estExact is a maintained, exact count — e.g. a label's live-node count read
	// from the label index. It is ground truth for the query's pinned snapshot.
	estExact estSource = iota
	// estStats is a histogram- or sample-derived count. Trustworthy enough to
	// drive a plan decision, though not exact. No such source exists yet.
	estStats
	// estHeuristic is a principled formula over real inputs (e.g. total/distinct).
	// It is NOT trustworthy on its own: an unvalidated heuristic vetoes to the
	// default plan until it is promoted to estStats by validation.
	estHeuristic
	// estFallback is a constant standing in for missing data. It is never
	// trustworthy and always vetoes to the default plan.
	estFallback
)

// String renders the source for diagnostics.
func (s estSource) String() string {
	switch s {
	case estExact:
		return "exact"
	case estStats:
		return "stats"
	case estHeuristic:
		return "heuristic"
	case estFallback:
		return "fallback"
	default:
		return "unknown"
	}
}

// estimate is a row-count estimate tagged with its provenance. It is a small
// value type carried by copy. It is always constructed with an explicit source
// (via [labelCardinalityEstimate] or a literal); the planner never puts a
// zero-value estimate on a decision path.
type estimate struct {
	rows   float64
	source estSource
}

// trustworthy reports whether this estimate may drive a non-default physical
// plan choice. Only estExact and estStats qualify; estHeuristic (unvalidated)
// and estFallback do not — they force the planner back to today's default plan
// (docs/optimizer-activation-design.md §2.1).
func (e estimate) trustworthy() bool {
	return e.source == estExact || e.source == estStats
}

// planStaysDefault is the trustworthiness veto over a candidate decision path.
// It reports true — meaning the planner MUST keep today's default plan — when
// ANY estimate on the path is untrustworthy (estFallback, or unvalidated
// estHeuristic). An empty path is trivially trustworthy (there is nothing to
// veto), so a decision that reads no estimates is never blocked by this helper.
func planStaysDefault(path ...estimate) bool {
	for _, e := range path {
		if !e.trustworthy() {
			return true
		}
	}
	return false
}

// labelCounter is the zero-allocation exact-count capability a label resolver
// may provide: the live-node count for a label read straight from the label
// index cardinality, without materialising a bitmap. The production
// *lpgLabelResolver implements it; a resolver that does not is handled by the
// bitmap fallback in [labelCardinalityEstimate].
type labelCounter interface {
	ResolveLabelCount(name string) (int64, bool)
}

// labelCardinalityEstimate returns the EXACT live-node count for label as an
// estExact estimate. It reads the count via [labelCounter.ResolveLabelCount]
// when the resolver provides it (zero-allocation — no bitmap is built), and
// otherwise falls back to the cardinality of the label bitmap the resolver
// returns. Both are exact live-node counts for the query's pinned snapshot, so
// both are estExact.
//
// A nil resolver yields an estFallback estimate: with no source the count is
// not knowable, and the fallback tag makes the trustworthiness veto
// ([planStaysDefault]) fall back to the default plan rather than act on a
// fabricated number.
func labelCardinalityEstimate(src labelResolverIface, label string) estimate {
	if src == nil {
		return estimate{rows: 0, source: estFallback}
	}
	if lc, ok := src.(labelCounter); ok {
		if n, ok := lc.ResolveLabelCount(label); ok {
			return estimate{rows: float64(n), source: estExact}
		}
	}
	bm := src.ResolveLabelBitmap(label)
	if bm == nil {
		// An unknown label resolves to the empty bitmap (zero live nodes); a nil
		// bitmap is treated the same. The count is still exact.
		return estimate{rows: 0, source: estExact}
	}
	return estimate{rows: float64(bm.GetCardinality()), source: estExact}
}
