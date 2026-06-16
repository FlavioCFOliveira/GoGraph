package sim

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// coverageDim names a coverage dimension: a family of buckets the swarm tracks
// and biases toward. Each dimension is a closed, BOUNDED set of bucket keys
// (scenario names, op kinds, outcome classes, violation classes), so the
// tracker's memory is bounded by construction — it never grows an unbounded
// key space.
type coverageDim string

// Coverage dimensions. Every bucket key within a dimension comes from a fixed,
// finite vocabulary the sim already defines (scenario constants, OpKind
// constants, ViolationKind constants), so the coverage map cannot grow without
// bound.
const (
	// dimScenario tracks which catalogue scenarios have been exercised.
	dimScenario coverageDim = "scenario"
	// dimOpKind tracks which workload operation kinds the failing op belonged to
	// (observable only for failing runs, whose report carries the failed op).
	dimOpKind coverageDim = "op-kind"
	// dimOutcome tracks the coarse outcome class of each run (pass / violation /
	// harness-error).
	dimOutcome coverageDim = "outcome"
	// dimViolation tracks which invariant-violation classes have been seen.
	dimViolation coverageDim = "violation"
)

// Outcome bucket keys for [dimOutcome].
const (
	outcomePass         = "pass"
	outcomeViolation    = "violation"
	outcomeHarnessError = "harness-error"
)

// CoverageTracker accumulates which coverage buckets a swarm has exercised and
// biases new-run scenario selection toward under-covered scenarios. It is fed
// one [SwarmRun] at a time (typically via the swarm's Observe hook) and is the
// completeness-critic that stops the swarm from re-testing the same happy path.
//
// The tracker derives every signal from already-observable sim-side data — the
// scenario name, the run outcome, and (for a failing run) the report's failed
// op kind and violation classes. It adds NO production hook. Signals that would
// require instrumenting production code (which Cypher exec operators a query
// used, which crashpoint sites a run hit) are NOT observable from the test side
// and are reported by [CoverageTracker.UnobservableSignals] rather than faked.
//
// # Concurrency contract
//
// CoverageTracker is safe for concurrent use: every method takes the internal
// mutex. Swarm workers feed and query it from many goroutines.
type CoverageTracker struct {
	mu sync.Mutex
	// counts maps dimension -> bucket key -> hit count. Both levels are bounded
	// by the fixed vocabularies above.
	counts map[coverageDim]map[string]int
	// scenarios is the closed set of scenario names the tracker biases over,
	// captured at construction so Select can weight toward the least-covered one.
	scenarios []string
	// selectCalls counts Select invocations, used only to break ties
	// deterministically (round-robin among equally-least-covered scenarios) so
	// the bias does not starve a tied bucket.
	selectCalls int
}

// NewCoverageTracker builds a tracker that biases selection over the given
// scenario names (typically a registry's [Registry.Names]). The scenario set is
// the bounded universe Select chooses from; it is copied so the caller may
// mutate its slice afterwards. Every dimension starts at zero coverage.
func NewCoverageTracker(scenarios []string) *CoverageTracker {
	cp := make([]string, len(scenarios))
	copy(cp, scenarios)
	sort.Strings(cp)
	ct := &CoverageTracker{
		counts:    make(map[coverageDim]map[string]int),
		scenarios: cp,
	}
	// Pre-seed the scenario dimension with zero counts so an unexplored scenario
	// shows up in the report (a missing key would be indistinguishable from a
	// zero-count one otherwise).
	sd := make(map[string]int, len(cp))
	for _, n := range cp {
		sd[n] = 0
	}
	ct.counts[dimScenario] = sd
	return ct
}

// inc records one hit on (dim, key) under the lock-held invariant. It lazily
// creates the dimension map on first use.
func (ct *CoverageTracker) incLocked(dim coverageDim, key string) {
	m := ct.counts[dim]
	if m == nil {
		m = make(map[string]int)
		ct.counts[dim] = m
	}
	m[key]++
}

// Record folds one completed run into the coverage tally. It tallies the
// scenario, the coarse outcome, and — for a failing run — the failed op kind and
// every violation class in the report. It is safe to call from many goroutines
// (the swarm's Observe hook runs under the aggregator lock, but Record takes its
// own lock so it is also safe to call directly).
func (ct *CoverageTracker) Record(run SwarmRun) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	ct.incLocked(dimScenario, run.Scenario)

	switch {
	case run.Err != nil:
		ct.incLocked(dimOutcome, outcomeHarnessError)
	case run.Report != nil:
		ct.incLocked(dimOutcome, outcomeViolation)
		// The failed op and its violation classes are observable from the report.
		if run.Report.FailedOp.Kind != "" {
			ct.incLocked(dimOpKind, string(run.Report.FailedOp.Kind))
		}
		for _, v := range run.Report.Violations {
			ct.incLocked(dimViolation, string(v.Kind))
		}
	default:
		ct.incLocked(dimOutcome, outcomePass)
	}
}

// Select implements [ScenarioSelector]: it returns the scenario name the next
// run should execute, biased toward the least-covered scenario in the tracked
// universe. Ties are broken round-robin (by the Select call counter) so no tied
// scenario is starved, keeping the bias fair and deterministic for a given call
// order. When the tracker has no scenario universe it falls back to the default.
func (ct *CoverageTracker) Select(_ int, defaultScenario string) string {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if len(ct.scenarios) == 0 {
		return defaultScenario
	}
	sd := ct.counts[dimScenario]

	// Find the minimum coverage count among the tracked scenarios.
	minCount := -1
	for _, n := range ct.scenarios {
		c := sd[n]
		if minCount < 0 || c < minCount {
			minCount = c
		}
	}
	// Collect every scenario at the minimum, then pick one round-robin so a tie
	// is distributed rather than always resolving to the first name.
	var least []string
	for _, n := range ct.scenarios {
		if sd[n] == minCount {
			least = append(least, n)
		}
	}
	choice := least[ct.selectCalls%len(least)]
	ct.selectCalls++
	return choice
}

// CoverageBucket is one reported bucket: its dimension, key, and hit count.
type CoverageBucket struct {
	Dimension string
	Key       string
	Count     int
}

// Exercised reports whether the bucket has been hit at least once.
func (b CoverageBucket) Exercised() bool { return b.Count > 0 }

// CoverageSummary is the tracker's report: every bucket across every dimension,
// in a deterministic order, plus the count of exercised vs unexplored buckets.
type CoverageSummary struct {
	// Buckets lists every tracked bucket (dimension-then-key sorted).
	Buckets []CoverageBucket
	// Exercised is the number of buckets with a non-zero count.
	Exercised int
	// Unexplored is the number of tracked buckets still at zero.
	Unexplored int
}

// Summary returns a snapshot of the current coverage across every dimension, in
// a deterministic (dimension, key) order. Zero-count scenario buckets are
// included so the report distinguishes "tracked but never hit" from "unknown".
func (ct *CoverageTracker) Summary() CoverageSummary {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	var out CoverageSummary
	dims := make([]string, 0, len(ct.counts))
	for d := range ct.counts {
		dims = append(dims, string(d))
	}
	sort.Strings(dims)
	for _, d := range dims {
		m := ct.counts[coverageDim(d)]
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c := m[k]
			out.Buckets = append(out.Buckets, CoverageBucket{Dimension: d, Key: k, Count: c})
			if c > 0 {
				out.Exercised++
			} else {
				out.Unexplored++
			}
		}
	}
	return out
}

// ScenarioCoverage returns the per-scenario hit counts (a copy), for tests that
// assert the bias steered runs toward under-covered scenarios.
func (ct *CoverageTracker) ScenarioCoverage() map[string]int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	out := make(map[string]int, len(ct.counts[dimScenario]))
	for k, v := range ct.counts[dimScenario] {
		out[k] = v
	}
	return out
}

// UnobservableSignals reports the coverage signals the task brief names that
// CANNOT be observed from the test side without adding a production hook, so
// they are deliberately NOT tracked (the constitution forbids adding a hook to
// production code from the DST harness). It documents the boundary rather than
// faking the signal.
//
// Returned, in order:
//
//   - "cypher-exec-operators": which physical operators (Expand, NodeByLabelScan,
//     hash join, index seek, …) a query's plan used. The engine does not export
//     a per-run operator-usage counter through any public API; observing it would
//     require instrumenting cypher/exec. The differential test (#1567) instead
//     exercises operator EQUIVALENCE via the DisableHashJoin / DisableRangeIndexSeek
//     toggles, which is the observable proxy.
//   - "crashpoint-sites": which internal/crashpoint sites a run armed/hit. Crash
//     points are a test-only injection seam with no public hit-counter; the
//     crash-storm scenario exercises them but does not expose which fired. The
//     metrics oracle (#1568) reads only already-exported metrics for the same
//     reason.
func (ct *CoverageTracker) UnobservableSignals() []string {
	return []string{"cypher-exec-operators", "crashpoint-sites"}
}

// String renders the coverage summary as a human-readable block (one line per
// dimension with its bucket counts), suitable for the CLI -coverage-report
// output. It ends without a trailing newline.
func (s CoverageSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Coverage: %d exercised, %d unexplored (%d buckets)",
		s.Exercised, s.Unexplored, len(s.Buckets))
	curDim := ""
	for _, bk := range s.Buckets {
		if bk.Dimension != curDim {
			fmt.Fprintf(&b, "\n  [%s]", bk.Dimension)
			curDim = bk.Dimension
		}
		mark := " "
		if !bk.Exercised() {
			mark = "·" // unexplored
		}
		fmt.Fprintf(&b, "\n    %s %-20s %d", mark, bk.Key, bk.Count)
	}
	return b.String()
}
