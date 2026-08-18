package sim

import (
	"context"
	"fmt"
	"strings"
)

// Statistics-driven planning regime oracle (rmp #2456).
//
// db.stats.refresh() — Engine.RefreshStatistics behind the procedure surface —
// is the sole entry point that builds the planner's approximate statistics
// (graph/index/stats: HLL NDV sketches, exact top-k MCVs, equi-depth
// histograms). Before this checker the DST never invoked it, so the cost model
// only ever ran in its no-statistics regime and the whole statistics path was
// 0% covered by simulation. This checker drives the refresh under churn and
// crash/recovery and pins the contracts measured at HEAD (rmp #2456):
//
//   - Row shape: the procedure returns EXACTLY one row, `ok` (bool) and
//     `detail` (string). A completed rebuild reports ok=true with
//     [statsRefreshOKDetail]; a call inside the rate-limit window is REFUSED
//     in-band — ok=false with a [statsRefusedPrefix] reason — never an error,
//     because the caller did nothing wrong (procs.statsRefreshMinInterval,
//     30 s, exists so an O(nodes x properties) scan reachable by any Bolt
//     client cannot be driven as an amplification vector).
//   - Result identity: statistics are a planner INPUT. A refresh may change
//     which PLAN is chosen but must never change what a query RETURNS, so the
//     fixed probe battery must answer identically immediately before and
//     immediately after every refresh. A divergence is an ACID-consistency
//     violation. A PLAN change across the refresh is legal and is therefore
//     REPORTED (the [StatsRegime.PlanChanges] counter), never failed.
//   - Something was seen: a rebuild that reports ok=true must leave a
//     non-zero [cypher.Engine.StatsTrackedPairs] — the collector really holds
//     per-(label, property) statistics — otherwise the "refresh" proved
//     nothing. A refusal must leave the tracked-pairs count untouched.
//   - Post-crash regime: the statistics collector and the rate limiter are
//     per-Engine, in-memory, and deliberately NOT rebuilt by recovery
//     (cypher/stats_build.go: absence of a statistic is harmless — consumers
//     fall back to their exact-count plans). A recovered engine therefore
//     reports ZERO tracked pairs until the next explicit refresh
//     ([StatsRegime.CheckRecovered]), and its fresh limiter must allow that
//     refresh immediately ([ExpectRebuild] after recovery).

// statsSeedMix derives the statistics-regime checker's own draw stream from
// the master seed, so choosing the mid-run refresh ticks never perturbs the
// workload, crash, parity, or seek-result streams (the same isolation rule
// paritySeedMix and seekResultsSeedMix follow).
const statsSeedMix uint64 = 0x6a09e667f3bcc909

// statsRegimeOp is the op label every statistics-regime violation carries.
const statsRegimeOp = "stats refresh regime"

// statsRefreshQuery invokes the maintenance procedure and projects the row in
// the YIELD form a real client uses. ok is projected through toString so the
// checker's narrow [Result] view (string columns) can read it.
const statsRefreshQuery = "CALL db.stats.refresh() YIELD ok, detail RETURN toString(ok) AS ok, detail"

// statsRefreshOKDetail is the exact detail a completed rebuild reports,
// pinned from cypher/procs/builtin_db.go (dbStatsRefresh) and verified
// empirically for rmp #2456.
const statsRefreshOKDetail = "planner statistics rebuilt"

// statsRefusedPrefix is the prefix of the in-band refusal a call inside the
// rate-limit window reports (the remainder names the window and the wait, so
// only the stable prefix is pinned).
const statsRefusedPrefix = "refused: a statistics rebuild is rate-limited"

// RefreshExpectation states what a [StatsRegime.CheckRefresh] call may expect
// from the procedure's ok column, given what the caller knows about the
// engine's rate-limiter state.
type RefreshExpectation int

const (
	// ExpectRebuild asserts ok=true: the limiter is provably fresh (the first
	// call on a newly built or newly recovered engine), so a refusal means the
	// limiter leaked across an engine lifetime.
	ExpectRebuild RefreshExpectation = iota
	// ExpectRefusal asserts ok=false: the call is issued back-to-back with a
	// completed rebuild, inside the rate-limit window, so a rebuild means the
	// amplification-vector guard did not engage.
	ExpectRefusal
	// ExpectEither accepts both outcomes: a mid-run, seed-chosen probe whose
	// distance from the previous rebuild is wall-clock dependent. The row
	// shape, the detail/ok agreement, the tracked-pairs observable, and result
	// identity across the call are still asserted in full.
	ExpectEither
)

// StatsEngine is the engine surface the statistics-regime checker needs: the
// plan surface (results + Explain) plus the tracked-pairs observable that
// proves a rebuild actually published per-(label, property) statistics. The
// simulator's [EngineAdapter] satisfies it.
//
// # Concurrency contract
//
// Implementations need only be safe for single-goroutine use; the simulator
// never calls them concurrently.
type StatsEngine interface {
	PlanEngine
	// StatsTrackedPairs reports how many distinct (label, property) pairs the
	// engine currently holds planner statistics for; 0 on an engine that never
	// completed a refresh.
	StatsTrackedPairs() int
}

// StatsRegime is the statistics-driven planning regime checker: it drives
// CALL db.stats.refresh() at deterministic and seed-chosen points, pins the
// procedure's row/throttle contract, asserts result identity across every
// refresh, and accumulates the run's regime statistics. It is stateful so
// [StatsRegime.Finish] can assert non-vacuity over the whole run: at least
// one completed rebuild with a non-zero tracked-pairs observable and at least
// one exercised refusal, otherwise the statistics path never engaged and the
// run proved nothing about it.
//
// # Concurrency contract
//
// StatsRegime is NOT safe for concurrent use; the simulator drives it from
// the single simulation goroutine.
type StatsRegime struct {
	// probes is the fixed probe set whose results must be identical across
	// every refresh and whose Explain renderings are watched for (legal) plan
	// changes. The index-diversity scenario passes its parity probe set, so
	// the identity oracle runs over the same predicate shapes the parity and
	// plan-stability oracles pin.
	probes []ParityProbe
	// refreshes counts completed rebuilds (ok=true rows) observed.
	refreshes int
	// refusals counts in-window refusals (ok=false rows) observed.
	refusals int
	// planChanges counts probe arms whose Explain rendering changed across a
	// refresh — legal (statistics may change the PLAN, never the RESULT) and
	// therefore reported here rather than failed. At HEAD it stays 0: the
	// sim's Explain surface renders no statistics-derived annotation for the
	// probe shapes (measured for rmp #2456).
	planChanges int
	// maxTracked is the high-water StatsTrackedPairs observed after a
	// completed rebuild: the something-was-seen observable.
	maxTracked int
}

// NewStatsRegime builds the checker over the fixed probe set whose answers
// must survive every refresh unchanged.
func NewStatsRegime(probes ...ParityProbe) *StatsRegime {
	return &StatsRegime{probes: probes}
}

// Refreshes reports how many completed rebuilds (ok=true) the run observed.
func (k *StatsRegime) Refreshes() int { return k.refreshes }

// Refusals reports how many in-window refusals (ok=false) the run observed.
func (k *StatsRegime) Refusals() int { return k.refusals }

// PlanChanges reports how many probe-arm Explain renderings changed across a
// refresh over the whole run. A plan change is legal — statistics are a
// planner input — so this is the scenario's report channel, never a failure.
func (k *StatsRegime) PlanChanges() int { return k.planChanges }

// CheckRefresh drives one CALL db.stats.refresh() through engine and returns
// a violation for each contract breach found:
//
//   - the probe battery (both arms of every probe) answering differently
//     immediately before and immediately after the call is a
//     [ViolationACIDConsistency] — a statistics refresh changed a RESULT;
//   - a malformed row (not exactly one row, unreadable columns, a detail that
//     does not match its ok verdict) is a [ViolationOracleDeviation];
//   - an outcome contradicting expect ([ExpectRebuild] refused, or
//     [ExpectRefusal] rebuilt) is a [ViolationOracleDeviation];
//   - a rebuild that leaves [StatsEngine.StatsTrackedPairs] at zero, or a
//     refusal that CHANGES it, is a [ViolationOracleDeviation].
//
// A probe arm whose Explain rendering changed across the call increments
// [StatsRegime.PlanChanges] and is never a violation.
func (k *StatsRegime) CheckRefresh(tick int64, engine StatsEngine, expect RefreshExpectation) []Violation {
	c := &InvariantChecker{}

	beforeIDs, ok := k.captureProbeIDs(c, tick, engine, "before")
	if !ok {
		return c.violations
	}
	beforePlans, ok := k.capturePlans(c, tick, engine, "before")
	if !ok {
		return c.violations
	}
	trackedBefore := engine.StatsTrackedPairs()

	rebuilt, detail, ok := k.runRefresh(c, tick, engine)
	if !ok {
		return c.violations
	}
	k.checkOutcome(c, tick, engine, expect, rebuilt, detail, trackedBefore)

	afterIDs, ok := k.captureProbeIDs(c, tick, engine, "after")
	if ok {
		for i := range k.probes {
			k.checkArmIdentity(c, tick, &k.probes[i], "literal", beforeIDs[2*i], afterIDs[2*i])
			k.checkArmIdentity(c, tick, &k.probes[i], "param", beforeIDs[2*i+1], afterIDs[2*i+1])
		}
	}
	if afterPlans, ok := k.capturePlans(c, tick, engine, "after"); ok {
		for i := range afterPlans {
			if afterPlans[i] != beforePlans[i] {
				k.planChanges++
			}
		}
	}
	return c.violations
}

// checkOutcome validates the procedure's verdict against expect and the
// tracked-pairs observable.
func (k *StatsRegime) checkOutcome(c *InvariantChecker, tick int64, engine StatsEngine, expect RefreshExpectation, rebuilt bool, detail string, trackedBefore int) {
	if rebuilt {
		if expect == ExpectRefusal {
			c.add(ViolationOracleDeviation, tick, statsRegimeOp,
				"a refresh issued back-to-back with a completed rebuild was NOT refused: the "+
					"rate limit (one rebuild per procs.statsRefreshMinInterval) did not engage, so "+
					"the O(nodes x properties) scan is drivable as an amplification vector")
			return
		}
		if detail != statsRefreshOKDetail {
			c.add(ViolationOracleDeviation, tick, statsRegimeOp,
				fmt.Sprintf("a completed rebuild reported detail %q, want %q", detail, statsRefreshOKDetail))
		}
		tracked := engine.StatsTrackedPairs()
		if tracked <= 0 {
			c.add(ViolationOracleDeviation, tick, statsRegimeOp,
				"the rebuild reported ok=true but StatsTrackedPairs is 0: no (label, property) "+
					"statistics were published, so the refresh proved nothing")
			return
		}
		k.refreshes++
		if tracked > k.maxTracked {
			k.maxTracked = tracked
		}
		return
	}
	if expect == ExpectRebuild {
		c.add(ViolationOracleDeviation, tick, statsRegimeOp,
			fmt.Sprintf("the FIRST refresh on a fresh engine was refused (detail %q): the rate "+
				"limiter must start clean on every engine lifetime, so a refusal here means "+
				"limiter state leaked across construction or recovery", detail))
		return
	}
	if !strings.HasPrefix(detail, statsRefusedPrefix) {
		c.add(ViolationOracleDeviation, tick, statsRegimeOp,
			fmt.Sprintf("a refusal (ok=false) carried detail %q, want the rate-limit reason "+
				"(prefix %q) — an unexplained in-band refusal is indistinguishable from a "+
				"silent failure", detail, statsRefusedPrefix))
	}
	if tracked := engine.StatsTrackedPairs(); tracked != trackedBefore {
		c.add(ViolationOracleDeviation, tick, statsRegimeOp,
			fmt.Sprintf("a REFUSED refresh changed StatsTrackedPairs from %d to %d: a refusal "+
				"must be a pure no-op on the published snapshot", trackedBefore, tracked))
	}
	k.refusals++
}

// CheckRecovered pins the post-crash statistics regime: the collector is
// per-engine, in-memory, and never rebuilt by recovery, so a freshly
// recovered engine must report ZERO tracked pairs until the next explicit
// refresh. A non-zero count means recovery started rebuilding statistics (a
// contract change this oracle must be told about) or collector state leaked
// across the crash. Call it immediately after every recovery, before the
// post-recovery [ExpectRebuild] refresh.
func (k *StatsRegime) CheckRecovered(tick int64, engine StatsEngine) []Violation {
	if tracked := engine.StatsTrackedPairs(); tracked != 0 {
		return []Violation{{
			Kind: ViolationOracleDeviation,
			Tick: tick,
			Op:   statsRegimeOp,
			Message: fmt.Sprintf("a recovered engine reports %d tracked (label, property) statistics "+
				"pairs, want 0: statistics are in-memory and deliberately not rebuilt by recovery, "+
				"so the collector must start empty until the next explicit refresh", tracked),
		}}
	}
	return nil
}

// Finish asserts non-vacuity over the whole run: at least one completed
// rebuild left a non-zero tracked-pairs observable, and at least one refusal
// exercised the rate limit. A run that never engaged the statistics path — or
// never proved the throttle refuses — verified nothing about the
// statistics-driven planning regime and is reported as a
// [ViolationOracleDeviation] rather than passing silently. Call it once, at
// the end of the scenario.
func (k *StatsRegime) Finish(tick int64) []Violation {
	c := &InvariantChecker{}
	if k.refreshes == 0 || k.maxTracked <= 0 {
		c.add(ViolationOracleDeviation, tick, statsRegimeOp,
			fmt.Sprintf("vacuous run: no completed rebuild with a non-zero tracked-pairs "+
				"observable was ever seen (rebuilds=%d, max tracked pairs=%d), so the "+
				"statistics path never engaged", k.refreshes, k.maxTracked))
	}
	if k.refusals == 0 {
		c.add(ViolationOracleDeviation, tick, statsRegimeOp,
			"vacuous run: the rate-limit refusal was never observed, so the throttle "+
				"contract went unexercised")
	}
	return c.violations
}

// captureProbeIDs runs both arms of every probe and returns their sorted id
// multisets (literal at 2i, param at 2i+1). A probe failure is reported as a
// [ViolationOracleDeviation] and aborts the capture (ok=false): identity
// cannot be asserted against a battery that did not run.
func (k *StatsRegime) captureProbeIDs(c *InvariantChecker, tick int64, engine StatsEngine, phase string) ([][]int64, bool) {
	out := make([][]int64, 0, 2*len(k.probes))
	for i := range k.probes {
		p := &k.probes[i]
		lit, err := runProbeIDs(engine, p.Literal, nil)
		if err != nil {
			c.add(ViolationOracleDeviation, tick, statsRegimeOp,
				fmt.Sprintf("shape %q: %s-refresh literal arm %q failed: %v", p.Shape, phase, p.Literal, err))
			return nil, false
		}
		par, err := runProbeIDs(engine, p.Param, p.Params)
		if err != nil {
			c.add(ViolationOracleDeviation, tick, statsRegimeOp,
				fmt.Sprintf("shape %q: %s-refresh param arm %q failed: %v", p.Shape, phase, p.Param, err))
			return nil, false
		}
		out = append(out, lit, par)
	}
	return out, true
}

// capturePlans renders both arms of every probe through Explain (literal at
// 2i, param at 2i+1). A rendering failure is reported and aborts the capture.
func (k *StatsRegime) capturePlans(c *InvariantChecker, tick int64, engine StatsEngine, phase string) ([]string, bool) {
	out := make([]string, 0, 2*len(k.probes))
	for i := range k.probes {
		p := &k.probes[i]
		lit, err := engine.Explain(p.Literal, nil)
		if err != nil {
			c.add(ViolationOracleDeviation, tick, statsRegimeOp,
				fmt.Sprintf("shape %q: %s-refresh Explain literal %q failed: %v", p.Shape, phase, p.Literal, err))
			return nil, false
		}
		par, err := engine.Explain(p.Param, p.Params)
		if err != nil {
			c.add(ViolationOracleDeviation, tick, statsRegimeOp,
				fmt.Sprintf("shape %q: %s-refresh Explain param %q failed: %v", p.Shape, phase, p.Param, err))
			return nil, false
		}
		out = append(out, lit, par)
	}
	return out, true
}

// runRefresh invokes the procedure and returns its verdict. It asserts the
// pinned row shape — exactly one row with readable ok and detail columns —
// reporting any malformation as a [ViolationOracleDeviation] (ok=false).
func (k *StatsRegime) runRefresh(c *InvariantChecker, tick int64, engine StatsEngine) (rebuilt bool, detail string, ok bool) {
	res, err := engine.Run(context.Background(), statsRefreshQuery, nil)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, statsRegimeOp,
			fmt.Sprintf("%q failed: %v — a rebuild inside the window must be refused in-band, "+
				"never as an error", statsRefreshQuery, err))
		return false, "", false
	}
	defer func() { _ = res.Close() }()

	var okStr string
	rows := 0
	for res.Next() {
		rows++
		var okCol, detCol bool
		okStr, okCol = res.StringAt(0)
		detail, detCol = res.StringAt(1)
		if !okCol || !detCol {
			c.add(ViolationOracleDeviation, tick, statsRegimeOp,
				fmt.Sprintf("db.stats.refresh row %d: ok/detail columns unreadable as strings "+
					"(ok readable=%v, detail readable=%v)", rows, okCol, detCol))
			return false, "", false
		}
	}
	if err := res.Err(); err != nil {
		c.add(ViolationOracleDeviation, tick, statsRegimeOp,
			fmt.Sprintf("db.stats.refresh drain failed: %v", err))
		return false, "", false
	}
	if rows != 1 {
		c.add(ViolationOracleDeviation, tick, statsRegimeOp,
			fmt.Sprintf("db.stats.refresh returned %d rows, want exactly 1 so a caller can "+
				"distinguish a completed rebuild from a refusal", rows))
		return false, "", false
	}
	switch okStr {
	case "true":
		return true, detail, true
	case "false":
		return false, detail, true
	default:
		c.add(ViolationOracleDeviation, tick, statsRegimeOp,
			fmt.Sprintf("db.stats.refresh ok column rendered %q, want \"true\" or \"false\"", okStr))
		return false, "", false
	}
}

// checkArmIdentity appends an ACID-consistency violation when one probe arm's
// id-multiset differs across the refresh.
func (k *StatsRegime) checkArmIdentity(c *InvariantChecker, tick int64, p *ParityProbe, arm string, before, after []int64) {
	if equalIDMultisets(before, after) {
		return
	}
	query := p.Literal
	if arm == "param" {
		query = p.Param
	}
	c.add(ViolationACIDConsistency, tick, statsRegimeOp,
		fmt.Sprintf("shape %q (%s arm): a statistics refresh changed a query RESULT — statistics "+
			"are a planner input and may change the plan, never the answer\nquery: %s\nbefore: %s\nafter:  %s",
			p.Shape, arm, query, summariseIDs(before), summariseIDs(after)))
}
