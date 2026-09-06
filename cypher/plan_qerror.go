package cypher

// plan_qerror.go — observing how wrong the planner's estimates turn out to be
// (rmp #2767).
//
// # Why this exists
//
// Before rmp #2765 the estimate and the measurement lived on two different plans,
// so nothing could compare them. #2765 put them on the same [exec.PlanNode]. This
// file is what that made possible: for every operator carrying BOTH a trustworthy
// estimate and a comparable measurement, the ratio between the two is computed and
// emitted — at no extra measurement cost, because both numbers are already there.
//
// The gap it closes is named in the task: [Engine.RefreshStatistics] is
// caller-driven BY DESIGN (stats_build.go — deliberately no background worker),
// and until now the caller had nothing to drive it FROM. cypher/stats_metrics.go
// counts refreshes, lookups and fallbacks; none of them says whether a lookup
// produced a GOOD number. [Engine.StatsMisestimatedPairs] now does.
//
// # What is emitted, and what is NOT
//
// This file OBSERVES. It changes no plan, refreshes no statistic and re-plans
// nothing: acting on the observation is a separate decision that belongs to the
// operator of the database, not to the engine. The one state it keeps —
// [Engine.misestimated] — is read only by an exported accessor.
//
// # PROFILE only, structurally
//
// [Engine.observeEstimateQuality] has exactly ONE call site, in
// [Engine.profileMaterialised], which is the single funnel of all three PROFILE
// surfaces ([Engine.Profile], [Engine.ProfileTable] and the PROFILE statement
// prefix). It is not reachable from [Engine.Run], [Engine.RunInTx] or either
// EXPLAIN path, and that is a property of the CALL GRAPH rather than of a runtime
// test: there is no `if profiling` on any hot path to skip, because the code is
// not on the hot path at all.
//
// The property is gated three ways, since a call site is exactly the kind of thing
// a later change adds without noticing:
//
//   - TestQError_ObservationIsReachableOnlyFromTheProfilePath parses this package
//     and fails if the function acquires a second caller, or if its caller stops
//     being profileMaterialised;
//   - TestQError_RunAndExplainEmitNothing drives Run and both EXPLAIN surfaces
//     against a recording backend and asserts silence;
//   - BenchmarkEngWriteAutocommit and the read benchmarks stay flat.
//
// The two facts the comparison needs beyond #2765's estimate — how many times an
// operator was initialised, and whether it reached end-of-stream — live on the
// profiling wrapper, which only a PROFILE build allocates. See [exec.PlanRows].

import (
	"math"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// statsQErrorUnit is the duration ONE unit of q-error is carried as.
//
// The shared [cmetrics.Backend] has three primitives — counter, latency
// observation, gauge — and no float distribution, so a distribution of a
// dimensionless ratio has to travel as a duration. The choice of unit is not
// arbitrary: the Prometheus backend's bucket ladder runs from 100 µs to 5 s
// (internal/metrics/prometheus/prometheus.go, latencyBuckets), so one millisecond
// per unit places the observable range at q ∈ [0.1, 5000] with ten buckets across
// it. A q-error is ≥ 1 by construction, so the ladder brackets everything from a
// perfect estimate to one wrong by three and a half orders of magnitude, and only
// a worse-than-5000× estimate falls into +Inf.
//
// A scrape reads the mean q-error as 1000 × (`_sum` / `_count`), because `_sum` is
// serialised in seconds. That conversion is stated in docs/metrics.md beside the
// metric, and it is the price of not adding a fourth primitive to a Backend
// interface every consumer implements.
const statsQErrorUnit = time.Millisecond

// statsQErrorCeiling clamps the emitted sample. It exists only so that an
// arithmetically extreme ratio cannot overflow the int64 nanoseconds a
// [time.Duration] holds; it is nine orders of magnitude above any threshold a
// reader acts on, so a clamped sample and an unclamped one lead to the same
// conclusion.
const statsQErrorCeiling = 1e9

// statsQErrorHighFactor is the factor past which a misestimate is COUNTED as one
// rather than merely observed: it gates [statsMetricQErrorHigh] and admission to
// [Engine.misestimated].
//
// It is [joinReorderStatsMargin], and deliberately the same constant rather than a
// second number with the same job. That margin is the only factor this codebase
// already declares to be "large enough to matter for a statistics-driven plan
// decision": the disjoint-component reorder refuses to deviate from the default
// plan unless the candidate is 3× cheaper when any estimate on the path is
// histogram-derived (join_reorder_plan.go, which grounds the 3 in Ioannidis &
// Christodoulakis, SIGMOD 1991, via docs/optimizer-activation-design.md §4).
//
// It is a REPORTING threshold and nothing more. Crossing it does not prove a plan
// was harmed, and staying under it does not prove one was not — a misestimate below
// the margin can still flip a decision that sat near the boundary. What it does
// give is one number instead of two, tied to the one place the statistics actually
// reach a plan.
const statsQErrorHighFactor = joinReorderStatsMargin

// statsPair is a tracked (label, property) statistic, as the key of the
// misestimated set. It mirrors what [Engine.StatsTrackedPairs] counts, so the two
// accessors are read against the same denominator.
type statsPair struct {
	label string
	prop  string
}

// statsMisestimatedPairsMax bounds [Engine.misestimated].
//
// The set is already bounded in practice by the statistics collector's own size —
// only a tracked (label, property) can produce a trustworthy estimate to be wrong
// about — and that is bounded by observed schema cardinality rather than by |V|.
// The explicit ceiling is the module's bounded-resources mandate applied anyway:
// an unbounded set fed from a diagnostic that a client can invoke is not something
// to leave to an argument about what can reach it. Once full, further pairs are
// dropped and the count saturates, which is the honest degradation — the caller's
// conclusion ("refresh the statistics") does not change at the 4097th pair.
const statsMisestimatedPairsMax = 4096

// misestimatedPairs is the bounded set of (label, property) statistics a PROFILE
// has caught predicting badly.
//
// # Concurrency contract
//
// Safe for concurrent use. PROFILE is a diagnostic and writes here at most once per
// qualifying operator per profiled query, so a plain mutex is the right shape: the
// alternative (a sync.Map plus an atomic size) buys nothing at this rate and makes
// the ceiling racy.
type misestimatedPairs struct {
	mu   sync.Mutex
	pair map[statsPair]struct{}
}

// add records p, unless the set is already at its ceiling.
func (m *misestimatedPairs) add(p statsPair) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pair == nil {
		m.pair = make(map[statsPair]struct{}, 8)
	}
	// A pair already held is re-added freely: the ceiling bounds how many DISTINCT
	// pairs are kept, and refusing one that is already inside would change nothing
	// except to make the two conditions look independent when they are not.
	if _, seen := m.pair[p]; !seen && len(m.pair) >= statsMisestimatedPairsMax {
		return
	}
	m.pair[p] = struct{}{}
}

// size reports how many distinct pairs are held.
func (m *misestimatedPairs) size() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pair)
}

// reset empties the set. It is called when a fresh statistics snapshot is
// published, because every observation in it was made against the snapshot that
// was just replaced.
func (m *misestimatedPairs) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pair = nil
}

// StatsMisestimatedPairs reports how many distinct tracked (label, property) pairs
// a PROFILE has observed the planner estimating wrong by a factor of at least
// [statsQErrorHighFactor] (rmp #2767).
//
// It is the actionable half of the q-error surface, and the companion to
// [Engine.StatsTrackedPairs]: that one says how many statistics the engine holds,
// this one how many of them have been caught being wrong. A non-zero value is the
// signal [Engine.RefreshStatistics] never had a trigger for — statistics are
// maintained off the write path and only ever rebuilt when a caller asks, and until
// now nothing told a caller when to ask.
//
// It counts only what a PROFILE has actually looked at. A pair no profiled query
// has exercised is not in it, so a zero means "nothing observed to be wrong", never
// "everything is right". Only estimates the planner is permitted to ACT on are
// eligible; see [qErrorQualifies] for which those are and why.
//
// A successful [Engine.RefreshStatistics] CLEARS it, since every observation it
// held was made against the snapshot that has just been replaced. Reading a
// non-zero value again after a refresh therefore means the fresh statistics are
// still predicting badly — which is a different and more interesting fact than the
// first reading.
//
// Safe for concurrent use.
func (e *Engine) StatsMisestimatedPairs() int { return e.misestimated.size() }

// observeEstimateQuality walks the profiled operator tree and emits the q-error of
// every operator whose estimate and measurement are comparable.
//
// It is called from [Engine.profileMaterialised] and NOWHERE else — see the file
// header for why that is the structural form of "PROFILE only", and which gates
// hold it there.
//
// It runs after the drain and before any Close, on the same operator tree
// [exec.PlanTreeWithEstimates] captures, so every counter it reads is final. It
// walks the operators rather than the captured [exec.PlanNode] tree because the
// (label, property) attribution is keyed on operator IDENTITY, which the captured
// tree deliberately does not carry.
func (e *Engine) observeEstimateQuality(root exec.Operator, sink *planEstimateSink) {
	var walk func(exec.Operator)
	walk = func(op exec.Operator) {
		if op == nil {
			return
		}
		// The estimate is keyed on the operator BEFORE its profiling wrapper — the
		// identity buildOperator recorded — while the measurements live ON the
		// wrapper. Both are needed, so both forms are held here.
		inner := exec.UnwrapProfiled(op)
		e.observeOperatorQError(op, inner, sink)
		if kids, ok := inner.(exec.PlanChildren); ok {
			for _, c := range kids.PlanChildren() {
				walk(c)
			}
		}
	}
	walk(root)
}

// observeOperatorQError emits one operator's q-error, or emits nothing.
//
// Two conditions must both hold, and each rules out a different way of publishing
// a number that would not mean what it says:
//
//  1. The operator must carry a TRUSTWORTHY estimate ([qErrorQualifies]). Without
//     one there is no prediction to score, and scoring the zero value of
//     [exec.PlanEstimate] would report a perfect estimate for every operator that
//     emitted no rows — the exact inversion of the truth. An operator MISSING from
//     the map reads back as that same zero value, so the source test covers the
//     unclaimed case too and no separate presence check is needed; one would be a
//     condition that cannot fail.
//  2. Its row count must be a COMPLETE count of exactly one execution
//     ([exec.PlanRows]). That single test covers three things at once: an operator
//     nothing measured (an EXPLAIN carries estimates and no measurements, and
//     comparing an estimate with a structural zero would make every EXPLAIN look
//     like a catastrophic planner failure), an operator re-initialised by an
//     Apply-family parent, and an operator abandoned before end-of-stream. All
//     three would otherwise be reported as a planner error that never happened.
func (e *Engine) observeOperatorQError(op, inner exec.Operator, sink *planEstimateSink) {
	est := sink.est[inner]
	if !qErrorQualifies(est.Source) {
		return
	}
	rows, complete := exec.PlanRows(op)
	if !complete {
		return
	}
	q := qError(est.Rows, rows)
	cmetrics.ObserveLatency(statsMetricQError, qErrorDuration(q))
	if q < statsQErrorHighFactor {
		return
	}
	cmetrics.IncCounter(statsMetricQErrorHigh, 1)
	// Attribute the miss to the statistic it came from, when it came from one. An
	// estimate with no (label, property) behind it — a label scan's live count, a
	// count-store degree cell — still counts in the distribution and the counter,
	// because it was still a wrong prediction; it simply names no statistic for a
	// caller to refresh.
	if p, okPair := sink.pairs[inner]; okPair {
		e.misestimated.add(p)
	}
}

// qErrorQualifies reports whether an estimate of this provenance may be scored.
//
// # The boundary, and the argument for it
//
// It is exactly [estimate.trustworthy] — [exec.EstimateExact] and
// [exec.EstimateStats] in, [exec.EstimateHeuristic] and [exec.EstimateAbsent] out —
// re-expressed over the physical plan's mirror of the same classification. Three
// reasons, in decreasing order of force:
//
//   - ABSENT has no number at all. Its [exec.PlanEstimate] is the zero value, whose
//     Rows is 0, and 0 clamps to 1 in the q-error. An operator that emitted no rows
//     would therefore score a PERFECT 1 for an estimate that does not exist. That is
//     not a rounding problem, it is the metric saying the opposite of the truth, and
//     it is why the qualification is on the SOURCE and never on the number.
//   - HEURISTIC is a uniformity assumption, not a reading of the data. It is
//     1/NDV × N (stats_estimate.go, statsEqualityEstimateInner), which predicts the
//     AVERAGE value's frequency for whichever value was asked about. Under skew its
//     error is arbitrarily large BY CONSTRUCTION and a refresh does not reduce it:
//     re-scanning the graph yields the same average. Since the accessor exists to
//     tell a caller when refreshing would help, admitting an error a refresh cannot
//     fix would drown the signal in noise the caller can do nothing about. It is
//     also the provenance the planner already refuses to act on, so it cannot be
//     the cause of a plan the reader is holding.
//   - EXACT and STATS are the two the planner IS allowed to act on
//     ([estimate.trustworthy], docs/optimizer-activation-design.md §2.1). A
//     misestimate here is both meaningful — the number claims to be derived from
//     the data — and actionable, because both are read from state a refresh
//     rebuilds.
//
// Including EXACT is not a formality. Two very different things carry that tag:
// a LIVE label count, which cannot be stale and whose q-error is therefore a check
// on this instrument rather than on the planner; and a most-common-value hit, which
// is read from the statistics SNAPSHOT and has no staleness gate at all
// (statsEqualityEstimateInner returns estExact from the MCV list unconditionally,
// where statsRangeEstimateInner demotes a stale histogram to estFallback). An
// "exact" estimate from a stale MCV entry can therefore be arbitrarily wrong, and
// this metric is the first thing in the module able to see that.
func qErrorQualifies(s exec.EstimateSource) bool {
	return s == exec.EstimateExact || s == exec.EstimateStats
}

// qError returns max(est, act) / min(est, act) with both operands clamped at 1 —
// the standard, symmetric, multiplicative measure of cardinality-estimation error.
//
// It is 1 for a perfect estimate and grows with the FACTOR of the miss in either
// direction, which is what makes it the right shape here: over-estimating 10 rows
// as 1000 and under-estimating 1000 as 10 are the same size of mistake, and both
// are far worse than being 100 rows out on a million. The clamp at 1 is what keeps
// it defined when either side is zero; without it an estimate of 0 against a
// measurement of 0 is 0/0, and an estimate of 0 against any measurement is
// unbounded.
//
// Both arguments are non-negative by construction — [estRows] clamps a negative or
// NaN estimate to 0, and a measured row count cannot be negative — so no sign
// handling is needed. The result is a float64 because the ratio of two int64 counts
// is not an integer.
func qError(est, act int64) float64 {
	e, a := float64(est), float64(act)
	if e < 1 {
		e = 1
	}
	if a < 1 {
		a = 1
	}
	return math.Max(e, a) / math.Min(e, a)
}

// qErrorDuration encodes a q-error as the duration the metrics Backend carries it
// in: [statsQErrorUnit] per unit, clamped at [statsQErrorCeiling].
func qErrorDuration(q float64) time.Duration {
	if q > statsQErrorCeiling {
		q = statsQErrorCeiling
	}
	return time.Duration(q * float64(statsQErrorUnit))
}

// statsPairForSelection returns the (label, property) statistic a Selection's
// estimate is read from, and false when there is none.
//
// It reuses [selectionEstimateShape] — the SAME decomposition [selectionEstimate]
// performs to choose its provider — rather than repeating the extraction, so the
// pair a misestimate is attributed to is by construction the pair the estimate came
// from. An unlabelled scan leaf (an [ir.AllNodesScan]) has no (label, property)
// statistic: lookupStats resolves no label id for it and the estimate falls to
// estFallback, so it is excluded here rather than recorded under an empty label.
func statsPairForSelection(sel *ir.Selection, params map[string]expr.Value) (statsPair, bool) {
	sh, ok := selectionEstimateShape(sel, params)
	if !ok || sh.label == "" || sh.prop == "" {
		return statsPair{}, false
	}
	return statsPair{label: sh.label, prop: sh.prop}, true
}
