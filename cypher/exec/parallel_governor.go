package exec

// parallel_governor.go — ParallelGovernor (#1705).
//
// Adaptive intra-query parallelism for the morsel-parallel leaves
// (ParallelCountScan #1672, ParallelScanProject #1682). Each leaf, before it
// launches workers, asks the engine-shared governor how many workers it may use
// given how many parallel leaves are already running across all concurrent
// queries on the same Engine. This bounds the TOTAL worker goroutines near
// GOMAXPROCS instead of letting N concurrent queries each spawn GOMAXPROCS
// workers (N×GOMAXPROCS), which oversubscribes the cores and made large-graph
// parallel scans REGRESS past 4 cores under concurrent load (2026-06-24 audit).

import (
	"runtime"
	"sync/atomic"
)

// ParallelGovernor bounds intra-query parallelism across the concurrently
// executing queries that share one Engine. A morsel-parallel leaf registers on
// Init via [ParallelGovernor.Enter] (which returns its worker budget) and
// deregisters on Close via [ParallelGovernor.Leave].
//
// The budget is GOMAXPROCS divided by the number of parallel leaves currently
// in flight (including the caller), clamped to [1, morsels]:
//
//   - a single parallel query in flight gets the full GOMAXPROCS workers — no
//     change from the prior unconditional behaviour, so single-query latency is
//     unaffected;
//   - N concurrent parallel queries each get ~GOMAXPROCS/N workers, so the
//     aggregate worker count stays near GOMAXPROCS and the scheduler/cache
//     thrash of N×GOMAXPROCS goroutines is avoided.
//
// The inflight count is sampled once per Enter and races at the edges (an early
// query may briefly see a low count and grab a large budget); this is a
// deliberate approximation — it need only prevent the sustained N×GOMAXPROCS
// explosion, not divide the machine exactly. Worker count never affects results,
// only timing, so the governor has no ACID or openCypher-TCK impact.
//
// A nil *ParallelGovernor is valid and means "unbounded" — every leaf gets the
// full GOMAXPROCS budget and no count is tracked. This preserves the prior
// behaviour for any caller that constructs an operator without a governor (the
// public BuildPlan path and the operator unit tests).
//
// ParallelGovernor is safe for concurrent use by any number of goroutines.
type ParallelGovernor struct {
	inflight atomic.Int64
}

// Enter registers one parallel leaf and returns its worker budget, an integer
// in [1, morsels]. It MUST be paired with exactly one [ParallelGovernor.Leave]
// (deferred in the operator's Close). On a nil governor it returns the full
// GOMAXPROCS budget (clamped to morsels) without tracking anything.
//
// morsels is the number of work units the leaf has to distribute; the budget is
// never larger than that (more workers than morsels would leave workers idle).
func (g *ParallelGovernor) Enter(morsels int) int {
	gomax := runtime.GOMAXPROCS(0)
	inflight := int64(1)
	if g != nil {
		inflight = g.inflight.Add(1) // register self; the result includes self
	}
	budget := gomax
	if inflight > 1 {
		budget = gomax / int(inflight)
	}
	if budget < 1 {
		budget = 1
	}
	if morsels > 0 && budget > morsels {
		budget = morsels
	}
	return budget
}

// Leave deregisters a parallel leaf previously registered via
// [ParallelGovernor.Enter]. It is nil-safe and idempotency is the caller's
// responsibility (call it exactly once per successful Enter).
func (g *ParallelGovernor) Leave() {
	if g != nil {
		g.inflight.Add(-1)
	}
}
