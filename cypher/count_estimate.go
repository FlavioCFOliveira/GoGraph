package cypher

// count_estimate.go — the relationship count-store estExact provider (task
// #2083, design docs/count-store-design.md §7). It turns the exact E / D / T
// statistics the count-store maintains (#2082) into estimate-provenance values
// the planner's trustworthiness veto (planStaysDefault, estimate.go) consumes,
// exactly as labelCardinalityEstimate does for N(label).
//
// As of P2 this provider is INERT: nothing on the query path calls it yet (P3
// wires the count-store-gated join reorder that consumes these estimates), so it
// changes no plan — it is proven correct in isolation by count_estimate_test.go,
// the same way #2076 shipped its estExact label-count provider inert before the
// min-label peephole consumed it.
//
// # Promotion rule (design §7)
//
//   - E(relType) → estExact always. An unknown (never-interned) type has zero
//     live edges: an exact 0.
//   - N(label) → estExact always (unchanged; served by the label index via
//     labelCardinalityEstimate).
//   - D(label, relType, dir) / T(labelA, relType, labelB) → estExact iff the
//     relevant X-scoped family is NOT dirty; a dirty family yields estFallback,
//     an absolute veto to today's default plan (never a wrong or stale exact).
//     An unknown label or type resolves to an exact 0 (no such edge can exist).
//
// A read that feeds an estExact estimate MUST be issued under the query's View
// so the count is snapshot-consistent with the graph the query sees (§7.4); the
// resolver is consulted on the read-path build, which already holds visMu.RLock.

import (
	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// countSource is the capability the count-estimate provider needs from a
// resolver: stable name→id resolution against the shared label/relationship-type
// registry, and access to the engine's count store. The production
// *lpgLabelResolver implements it (ResolveLabelID + Counts).
type countSource interface {
	// ResolveLabelID resolves a label OR relationship-type name to its stable
	// interned id (both share g.reg); ok is false for a never-interned name.
	ResolveLabelID(name string) (uint32, bool)
	// Counts returns the relationship count-store, or nil when the resolver has
	// none (some tests) — in which case every estimate falls back.
	Counts() *count.Store
}

// relCardinalityEstimate returns E(relType) as an estimate. It is estExact
// whenever a count store is present (E is never dirty); an unknown type is an
// exact 0. A nil source or nil store yields estFallback so the trustworthiness
// veto keeps the default plan rather than acting on an unavailable statistic.
func relCardinalityEstimate(src countSource, relType string) estimate {
	if src == nil {
		return estimate{rows: 0, source: estFallback}
	}
	cs := src.Counts()
	if cs == nil {
		return estimate{rows: 0, source: estFallback}
	}
	cmetrics.IncCounter(countMetricLookup, 1) // observability (#2087)
	rt, ok := src.ResolveLabelID(relType)
	if !ok {
		return estimate{rows: 0, source: estExact}
	}
	return estimate{rows: float64(cs.CountE(rt)), source: estExact}
}

// degreeCardinalityEstimate returns D(label, relType, dir). It is estExact when
// the label's D cells in this direction are not dirty, and estFallback when they
// are (the IN side after any relabel, or an over-budget OUT relabel — §3.3.1). An
// unknown label or relationship type resolves to an exact 0 (no such degree can
// exist), which is sound even when the label is dirty because a never-interned
// type has provably zero edges.
func degreeCardinalityEstimate(src countSource, label, relType string, dir count.Direction) estimate {
	if src == nil {
		return estimate{rows: 0, source: estFallback}
	}
	cs := src.Counts()
	if cs == nil {
		return estimate{rows: 0, source: estFallback}
	}
	cmetrics.IncCounter(countMetricLookup, 1) // observability (#2087)
	lid, okL := src.ResolveLabelID(label)
	rt, okR := src.ResolveLabelID(relType)
	if !okL || !okR {
		return estimate{rows: 0, source: estExact}
	}
	if cs.DDirty(lid, dir) {
		cmetrics.IncCounter(countMetricLookupVeto, 1) // observability (#2087)
		return estimate{rows: 0, source: estFallback}
	}
	return estimate{rows: float64(cs.CountD(lid, rt, dir)), source: estExact}
}

// tripleCardinalityEstimate returns T(labelA, relType, labelB). It is estExact
// unless either endpoint label's T position is dirty (a-position labelA or
// b-position labelB), in which case it is estFallback. An unknown label or type
// resolves to an exact 0.
func tripleCardinalityEstimate(src countSource, labelA, relType, labelB string) estimate {
	if src == nil {
		return estimate{rows: 0, source: estFallback}
	}
	cs := src.Counts()
	if cs == nil {
		return estimate{rows: 0, source: estFallback}
	}
	cmetrics.IncCounter(countMetricLookup, 1) // observability (#2087)
	a, okA := src.ResolveLabelID(labelA)
	rt, okR := src.ResolveLabelID(relType)
	b, okB := src.ResolveLabelID(labelB)
	if !okA || !okR || !okB {
		return estimate{rows: 0, source: estExact}
	}
	if cs.TDirty(a, b) {
		cmetrics.IncCounter(countMetricLookupVeto, 1) // observability (#2087)
		return estimate{rows: 0, source: estFallback}
	}
	return estimate{rows: float64(cs.CountT(a, rt, b)), source: estExact}
}
