package sim

// mvcc_substrate.go — the standing MVCC-substrate telemetry oracle (rmp #2470).
//
// # What was unread, and why that mattered
//
// The MVCC substrate publishes a full account of what it is holding and why —
// [lpg.MVCCStats] (live records per store, the two published bounds, the
// reclamation watermark, in-flight commits, the retained chain-depth
// distribution, and the write-outcome counters) and [lpg.VacuumStats] (sweeps,
// records released, backlog). Until this oracle the simulator read exactly ONE
// field of it, [lpg.MVCCStats.Now], through [SimStore.ClockNow], and
// [lpg.VacuumStats] not at all.
//
// The MVCC scenarios therefore asserted isolation OUTCOMES — no lost update, no
// phantom apply, a stable snapshot — every one of which stays green while the
// vacuum quietly stops reclaiming. A substrate that never releases a version
// answers every query correctly and grows without bound; the DST could not see
// it, because nothing it read would change.
//
// # What the substrate actually exports, measured rather than assumed
//
// All four properties this oracle was asked for have a genuine public
// observable, but two of them are not readable in the shape one would guess,
// and both traps were found by measurement before a line of this file was
// written:
//
//   - **The vacuum is not always running.** It is woken by CHURN crossing
//     [lpg.MVCCStats.Bound] (4096 records as configured here), not by a timer,
//     and it EXITS once consecutive passes free nothing. A workload that stays
//     under the threshold never sweeps at all: measured, 815 live records after
//     800 committed writes with `passes=0, reclaimed=0`, and every bound
//     assertion passing because nothing had yet been asked of the substrate. A
//     run that does not put the substrate under this pressure proves nothing
//     about reclamation, which is why
//     [mvccSubstrateEvidence.reclamationPressured] gates the vacuum clauses and
//     why [RunMVCCSubstrateChurn] exists to guarantee it.
//
//   - **The chain-depth histogram describes the last COMPLETE SWEEP, not the
//     present.** The reclaimer resets a store's histogram when it starts that
//     store and fills it as it walks (see [mvcc.DepthHist]), so a graph whose
//     versions have all been released reads back as `chains=0, deepest=0` —
//     indistinguishable, to a naive bound check, from "every chain is short".
//     Measured: `chains=0` at every quiescent point, and `chains=1 deepest=1`
//     only while a sweep was in flight. A depth bound asserted at quiescence is
//     therefore satisfied by an EMPTY histogram, which is the vacuous guard this
//     sprint has already found six of. The oracle folds a running maximum over
//     samples taken DURING churn instead, and counts the samples that carried a
//     non-empty distribution so the population can be shown to be non-trivial.
//
// # The bound on retained depth is this oracle's, and says so
//
// The substrate publishes a bound for version MEMORY ([lpg.MVCCStats.Bound] and
// [lpg.MVCCStats.Ceiling], both adjudicated here) but publishes NO ceiling for
// per-object chain depth — only the distribution. So [maxRetainedChainDepth] is
// stated by this oracle rather than read from the substrate, and it is
// documented as such rather than dressed up as a published contract.
//
// # What a user ROLLBACK is, and why the abort arm drives conflicts instead
//
// [mvcc.WriteCounts.Aborts] counts transactions the substrate REFUSED
// publication, not transactions a client rolled back. GoGraph's explicit
// rollback is served by the statement undo log — `labelTx.abort` is
// deliberately "NOT yet wired to any statement path", because only one of the
// two mechanisms may own rollback — so a voluntary rollback publishes its
// inverses and is counted as a COMMIT. Measured: 50 rollbacks produced
// `commits +49, aborts 0`. The abort-heavy arm therefore forces SERIALIZATION
// CONFLICTS, which is what reaches the aborted-version path: measured, 29
// conflicts produced `aborts=29, conflicts=29, byStore[1]=29`.
//
// Withdrawal of those versions is SYNCHRONOUS, not a vacuum responsibility:
// [lpg.Graph.abortWake] calls `withdrawAbortedNow` before it returns, because a
// present-time read takes the stored value directly. So the abort arm asserts
// that live version memory does not carry the aborted transactions' versions
// once they have been refused — the property `mvcc_abort_reclaim` exists to
// provide.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// maxRetainedChainDepth is the bound this oracle holds retained version-chain
// depth to.
//
// It is the ORACLE's bound, not one the substrate publishes: [mvcc.Depths] is a
// distribution with no declared ceiling, so a number has to come from somewhere
// and this file is where it is stated. It is set at the top of
// [mvcc.DepthBuckets]' 32_63 bucket, so a chain that runs away lands in a bucket
// the histogram names (64_127 or 128_inf) rather than merely nudging a mean.
// Measured retained depth under the churn this package drives is 1 with no
// reader pinned, so the bound carries a wide margin over normal operation and
// still catches the growth it exists to catch.
const maxRetainedChainDepth = 64

// mvccSubstrateSample is ONE reading of the substrate telemetry.
//
// It is a flat copy rather than the two stats structs so that folding a run's
// worth of readings costs no allocation and the evidence never retains a handle
// on a graph the simulator may replace on the next crash.
type mvccSubstrateSample struct {
	// label names the point the reading was taken at, for a failure message.
	label string
	// quiescent records that the caller declared this point to have no
	// transaction in flight. Only a quiescent reading may be held to
	// [lpg.MVCCStats.InFlightCommits] returning to zero.
	quiescent bool
	// total, bound and ceiling are the live version-record count and the two
	// bounds the substrate publishes for it.
	total, bound, ceiling int64
	// watermark is the reclamation watermark and now the clock's published
	// instant, so now-watermark is how far behind the oldest reader is.
	watermark, now uint64
	// inFlight is how many commit timestamps are allocated but not published,
	// and waiting how many callers are blocked on the frontier.
	inFlight uint64
	waiting  int64
	// activeSnapshots / capacity / unregistered describe horizon occupancy.
	// While unregistered is non-zero the watermark is zero and NOTHING is
	// reclaimed — the one state in which version memory has no bound at all.
	activeSnapshots, capacity int
	unregistered              int64
	// wmRegressions and staleLeaves MUST both be zero; the substrate documents
	// each as impossible while it is sound.
	wmRegressions, staleLeaves int64
	// write is the write-outcome half: writers, commits, aborts, conflicts.
	write mvcc.WriteCounts
	// depth is the retained chain-depth distribution as of the last completed
	// sweep. See the file comment for why it reads empty at quiescence.
	depth mvcc.Depths
	// vacuum progress: sweeps run, records released, and debt not yet swept.
	passes    uint64
	reclaimed int64
	backlog   int64
}

// sampleMVCCSubstrate takes one reading of g's MVCC telemetry.
//
// Both stats calls are plain atomic loads, so sampling disturbs no reader and
// may be called as often as a scenario likes.
func sampleMVCCSubstrate(g *lpg.Graph[string, float64], label string, quiescent bool) mvccSubstrateSample {
	m := g.MVCCStats()
	v := g.VacuumStats()
	return mvccSubstrateSample{
		label: label, quiescent: quiescent,
		total: m.Total, bound: m.Bound, ceiling: m.Ceiling,
		watermark: m.Watermark, now: m.Now,
		inFlight: m.InFlightCommits, waiting: m.SessionsWaiting,
		activeSnapshots: m.ActiveSnapshots, capacity: m.SnapshotCapacity,
		unregistered:  m.UnregisteredSnapshots,
		wmRegressions: m.WatermarkRegressions, staleLeaves: m.HorizonStaleLeaves,
		write: m.Write, depth: m.ChainDepth,
		passes: v.Passes, reclaimed: v.Reclaimed, backlog: v.Backlog,
	}
}

// mvccSubstrateEvidence is the folded record of every reading a run took.
//
// It FOLDS rather than retains: a soak run samples continuously, and keeping
// every sample would make the oracle itself an unbounded allocation — the
// defect it is here to detect. Only the first and most recent readings are kept
// whole; everything else is a running maximum or a count, which is what the
// clauses in [checkMVCCSubstrate] adjudicate.
//
// It holds MEASUREMENTS and no verdict, so a test can log what a run observed
// and the adjudicator can work on numbers rather than on a claim.
type mvccSubstrateEvidence struct {
	// label names the run in a violation message.
	label string
	// n and quiesced count the readings folded in, and how many of those were
	// taken at a declared quiescent point.
	n, quiesced int
	// first and last are the earliest and most recent readings, which is what
	// makes "the watermark advanced over the run" a subtraction.
	first, last mvccSubstrateSample
	// maxTotal is the high-water mark of live version records.
	maxTotal int64
	// crossedBound records that a READING caught live records or backlog at or
	// above the churn threshold at which the vacuum is woken.
	//
	// It is a sampled witness and therefore aliases: the sweeper can clear the
	// debt between two samples, so a run that crossed the threshold many times
	// can be observed never above it. Measured exactly that — a 6000-round run
	// whose vacuum ran 76 passes and freed 5925 records was never sampled above
	// 1531 live. Use [mvccSubstrateEvidence.reclamationPressured], which adds the
	// unaliased witness, rather than this field alone.
	crossedBound bool
	// ceilingBreaches counts readings whose live-record total exceeded the
	// instantaneous ceiling, and worstCeilingExcess how far the worst went past.
	ceilingBreaches    int
	worstCeilingExcess int64
	// deepest is the greatest retained chain depth any sweep reported, maxChains
	// the most chains any single reading carried, and chainSamples how many
	// readings carried a NON-EMPTY distribution. The last is the non-vacuity
	// witness for the depth clause: without it a bound holds trivially on a
	// histogram that measured nothing.
	deepest      uint64
	maxChains    uint64
	chainSamples int
	// deepestUnpinned is the greatest retained depth observed while NO snapshot
	// was registered, and unpinnedChainSamples how many such readings carried a
	// distribution.
	//
	// This, and not deepest, is what the depth bound is adjudicated on. A deep
	// RETAINED chain is expected while a long-lived reader is pinning the
	// watermark — depth is measured after truncation below the watermark, so a
	// reader that holds the watermark back holds every version it can still
	// reach — and measured here at 750 with one reader open across 6000 writes.
	// That is the substrate working as designed, not a leak. With nothing
	// registered, every version below the watermark is reclaimable and a
	// retained chain must collapse; measured, it collapses to 1.
	deepestUnpinned      uint64
	unpinnedChainSamples int
	// maxActiveSnapshots is the most snapshots registered at any reading. It is
	// the witness that decides whether a stalled watermark or a deep chain has a
	// legitimate explanation, and it is folded over the WHOLE run rather than
	// read from the last sample: a reader that pinned the watermark and then
	// went away still explains the stall it caused.
	maxActiveSnapshots int
	// depth accumulates the buckets across readings. It is an accumulation of
	// samples and NOT a population snapshot — successive sweeps reset the
	// histogram, so the buckets double-count chains that outlived a sweep. It is
	// reported for shape, never adjudicated.
	depth mvcc.Depths
	// quiesceInFlightBreaches counts quiescent readings that still had a commit
	// allocated and unpublished, and worstQuiesceInFlight the worst such count.
	quiesceInFlightBreaches int
	worstQuiesceInFlight    uint64
	// wmRegressions, staleLeaves and unregistered are the worst readings of the
	// three counters that must never leave zero.
	wmRegressions, staleLeaves, unregistered int64
	// watermarkRegressed records that the watermark itself was observed to move
	// BACKWARDS between two readings — the same violation the substrate's own
	// counter reports, observed independently here so a defect in that counter
	// cannot hide it.
	watermarkRegressed bool
	// measured guards the zero value: every clause below would pass on it.
	measured bool
}

// newMVCCSubstrateEvidence starts an evidence record for a named run.
func newMVCCSubstrateEvidence(label string) *mvccSubstrateEvidence {
	return &mvccSubstrateEvidence{label: label}
}

// observe folds one reading of g into the evidence.
//
// quiescent declares that the caller knows no transaction to be in flight; only
// such a point may be held to in-flight commits returning to zero. Everything
// else is adjudicated at every point.
func (e *mvccSubstrateEvidence) observe(g *lpg.Graph[string, float64], label string, quiescent bool) mvccSubstrateSample {
	s := sampleMVCCSubstrate(g, label, quiescent)
	e.fold(&s)
	return s
}

// fold accumulates one already-taken reading. Split from [mvccSubstrateEvidence.observe]
// so a test can drive the folding with fabricated readings and prove each clause
// of [checkMVCCSubstrate] is reachable.
//
// Takes a pointer because the sample carries the two stats structs' worth of
// fields; it is not retained, only copied into first/last.
func (e *mvccSubstrateEvidence) fold(s *mvccSubstrateSample) {
	if !e.measured {
		e.first = *s
		e.measured = true
	} else if s.watermark < e.last.watermark {
		// Observed independently of the substrate's own regression counter.
		e.watermarkRegressed = true
	}
	e.last = *s
	e.n++
	if s.quiescent {
		e.quiesced++
		if s.inFlight != 0 {
			e.quiesceInFlightBreaches++
			if s.inFlight > e.worstQuiesceInFlight {
				e.worstQuiesceInFlight = s.inFlight
			}
		}
	}
	if s.total > e.maxTotal {
		e.maxTotal = s.total
	}
	// The debt threshold is what wakes the vacuum; reaching it is what makes
	// every reclamation clause a real question rather than a formality.
	if s.bound > 0 && (s.total >= s.bound || s.backlog >= s.bound) {
		e.crossedBound = true
	}
	if s.ceiling > 0 && s.total > s.ceiling {
		e.ceilingBreaches++
		if excess := s.total - s.ceiling; excess > e.worstCeilingExcess {
			e.worstCeilingExcess = excess
		}
	}
	if s.activeSnapshots > e.maxActiveSnapshots {
		e.maxActiveSnapshots = s.activeSnapshots
	}
	chains := s.depth.Chains()
	if chains > 0 {
		e.chainSamples++
		if chains > e.maxChains {
			e.maxChains = chains
		}
		e.depth.Add(s.depth)
	}
	if s.depth.Deepest > e.deepest {
		e.deepest = s.depth.Deepest
	}
	// Only a reading taken with nothing registered can speak to the bound: see
	// the field comment on deepestUnpinned.
	if s.activeSnapshots == 0 {
		if chains > 0 {
			e.unpinnedChainSamples++
		}
		if s.depth.Deepest > e.deepestUnpinned {
			e.deepestUnpinned = s.depth.Deepest
		}
	}
	if s.wmRegressions > e.wmRegressions {
		e.wmRegressions = s.wmRegressions
	}
	if s.staleLeaves > e.staleLeaves {
		e.staleLeaves = s.staleLeaves
	}
	if s.unregistered > e.unregistered {
		e.unregistered = s.unregistered
	}
}

// commits, aborts and conflicts are the write outcomes the substrate has
// counted as of the most recent reading.
//
// They are ABSOLUTE rather than a delta against the first reading, because the
// substrate documents them as cumulative and never reset, and because they are
// used here as EXISTENCE guards — "did anything commit", "was anything
// refused" — which an absolute count answers correctly even for evidence that
// starts with a single reading. A window delta would read zero on a
// one-reading record and silently disarm the clause it guards, which is how a
// guard comes to prove nothing.
//
// The quantities that genuinely describe a WINDOW — the watermark's travel and
// the vacuum's progress — are deltas, below.
func (e *mvccSubstrateEvidence) commits() uint64 { return e.last.write.Commits }

func (e *mvccSubstrateEvidence) aborts() uint64 { return e.last.write.Aborts }

func (e *mvccSubstrateEvidence) conflicts() uint64 { return e.last.write.Conflicts }

// reclamationPressured reports whether the run actually put the substrate under
// reclamation pressure — the precondition that makes every vacuum clause a real
// question rather than a formality.
//
// Two witnesses, because the obvious one aliases. A reading at or above the
// wake threshold is direct but samples can miss the peak entirely; a sweep
// HAVING RUN is indirect but cannot be missed, since the vacuum starts only when
// debt crosses that threshold or a transaction aborts. Either is sufficient, and
// their union is what a run must show to be read as having tested reclamation.
func (e *mvccSubstrateEvidence) reclamationPressured() bool {
	return e.crossedBound || e.sweeps() > 0
}

// watermarkAdvance is how far the reclamation watermark moved over the run.
func (e *mvccSubstrateEvidence) watermarkAdvance() uint64 {
	if e.last.watermark < e.first.watermark {
		return 0
	}
	return e.last.watermark - e.first.watermark
}

// sweeps and reclaimedRecords are the vacuum's progress over the run.
func (e *mvccSubstrateEvidence) sweeps() uint64 {
	if e.last.passes < e.first.passes {
		return 0
	}
	return e.last.passes - e.first.passes
}

func (e *mvccSubstrateEvidence) reclaimedRecords() int64 {
	if e.last.reclaimed < e.first.reclaimed {
		return 0
	}
	return e.last.reclaimed - e.first.reclaimed
}

// summary renders the folded measurements for a test log or a failure message.
func (e *mvccSubstrateEvidence) summary() string {
	return fmt.Sprintf("%s: %d readings (%d quiescent), commits +%d aborts +%d conflicts +%d, "+
		"watermark %d->%d (+%d) vs clock %d, version records max=%d last=%d (bound=%d ceiling=%d, crossed=%t), "+
		"vacuum +%d passes releasing +%d records (backlog=%d), retained depth deepest=%d (unpinned %d over %d "+
		"readings) across %d non-empty readings (max chains=%d), horizon peak %d of %d (unregistered=%d), "+
		"integrity wmRegress=%d staleLeaves=%d",
		e.label, e.n, e.quiesced, e.commits(), e.aborts(), e.conflicts(),
		e.first.watermark, e.last.watermark, e.watermarkAdvance(), e.last.now,
		e.maxTotal, e.last.total, e.last.bound, e.last.ceiling, e.crossedBound,
		e.sweeps(), e.reclaimedRecords(), e.last.backlog,
		e.deepest, e.deepestUnpinned, e.unpinnedChainSamples, e.chainSamples, e.maxChains,
		e.maxActiveSnapshots, e.last.capacity, e.unregistered,
		e.wmRegressions, e.staleLeaves)
}

// checkMVCCSubstrate adjudicates the folded telemetry of a run.
//
// Every clause below is a way the substrate could look healthy and not be:
//
//   - nothing was ever read, so the zero value would satisfy every clause;
//   - no quiescent point was ever declared, so the in-flight clause — the only
//     one that can distinguish a drained commit path from a stalled one — was
//     never asked;
//   - a commit allocated an instant and never published it, which holds the
//     frontier for EVERY reader however many later commits published behind it,
//     and is the permanent staleness [lpg.MVCCStats.InFlightCommits] exists to
//     report;
//   - the reclamation watermark did not move while transactions committed, so
//     nothing below it ever became reclaimable — a vacuum that has stopped
//     reclaiming, which every isolation OUTCOME check stays green through;
//   - the watermark moved BACKWARDS, so versions a live reader can still reach
//     became reclaimable: an Isolation violation rather than a leak;
//   - the substrate's own regression and stale-leaf detectors fired, each of
//     which it documents as impossible while it is sound;
//   - a reader or writer could not get a horizon slot, which suspends
//     reclamation entirely and is the one state in which version memory has no
//     bound;
//   - live version memory went past the instantaneous ceiling the substrate
//     publishes, which a committer is supposed to block rather than exceed;
//   - churn crossed the threshold that wakes the vacuum and the vacuum
//     nevertheless released nothing, which is unbounded version growth stated
//     exactly;
//   - a retained version chain grew past the depth bound, which is read cost
//     per object and the tail a mean would hide.
//
// It does NOT fail a run merely for staying under the vacuum's wake threshold:
// that is legitimate for a small workload, and it is the SCENARIO's job to
// guarantee the crossing. [checkMVCCSubstrateNonVacuity] is where that is
// required, kept separate so the two questions cannot be confused.
func checkMVCCSubstrate(tick int64, e *mvccSubstrateEvidence) []Violation {
	const op = "MVCC substrate telemetry"
	var out []Violation
	fail := func(kind ViolationKind, format string, args ...any) {
		out = append(out, Violation{
			Kind: kind, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s: ", e.label) + fmt.Sprintf(format, args...),
		})
	}

	if !e.measured {
		fail(ViolationACIDConsistency, "the substrate was never sampled: no version count, watermark or vacuum"+
			" reading was taken, so every clause below would pass on the zero value")
		return out
	}
	if e.quiesced == 0 {
		fail(ViolationACIDConsistency, "no reading was taken at a declared quiescent point, so in-flight commits"+
			" returning to zero — the only clause that separates a drained commit path from a stalled one —"+
			" was never asked (%d readings folded)", e.n)
	}

	// (c) In-flight commits must return to zero once nothing is in flight.
	if e.quiesceInFlightBreaches > 0 {
		fail(ViolationACIDIsolation, "%d quiescent reading(s) still had a commit timestamp allocated and"+
			" unpublished (worst %d): the commit frontier is CONTIGUOUS, so one such commit holds every"+
			" reader's view back however many later commits have already published",
			e.quiesceInFlightBreaches, e.worstQuiesceInFlight)
	}

	// (b) The watermark must advance while transactions commit, and must never regress.
	if e.watermarkRegressed {
		fail(ViolationACIDIsolation, "the reclamation watermark moved BACKWARDS between two readings"+
			" (last observed %d): versions a live reader can still reach became reclaimable", e.last.watermark)
	}
	// Guarded on having a WINDOW at all: with a single reading first == last, so
	// the advance is zero by construction and the clause would fire on evidence
	// that simply never observed two points.
	if c := e.commits(); e.n >= 2 && c > 0 && e.watermarkAdvance() == 0 &&
		e.unregistered == 0 && e.maxActiveSnapshots == 0 {
		fail(ViolationACIDConsistency, "the reclamation watermark stalled at %d while %d transaction(s) committed"+
			" (clock now %d), with NO snapshot registered at any reading to hold it and no unregistered reader"+
			" to suspend it: nothing below it ever became reclaimable", e.last.watermark, c, e.last.now)
	}

	// The substrate's own impossible-by-construction detectors.
	if e.wmRegressions != 0 {
		fail(ViolationACIDIsolation, "the substrate reported %d watermark regression(s), which it documents as"+
			" impossible while it is sound: a live reader stopped being represented in the watermark", e.wmRegressions)
	}
	if e.staleLeaves != 0 {
		fail(ViolationACIDIsolation, "the horizon reported %d stale leaf release(s): a slot was returned that"+
			" nobody held, whose next release lands on another reader's bit and removes that reader"+
			" from the watermark", e.staleLeaves)
	}
	if e.unregistered != 0 {
		fail(ViolationACIDConsistency, "%d reader(s) or writer(s) could not get a horizon slot (capacity %d):"+
			" while that is non-zero the watermark is zero and NOTHING is reclaimed, which is the one state"+
			" in which version memory genuinely has no bound", e.unregistered, e.last.capacity)
	}

	// (d) Version memory must stay under the ceiling the substrate publishes.
	if e.ceilingBreaches > 0 {
		fail(ViolationACIDConsistency, "live version records exceeded the published ceiling in %d reading(s)"+
			" (worst by %d records; high-water %d against ceiling %d): a committer is meant to WAIT for the"+
			" sweeper past this point rather than charge past it",
			e.ceilingBreaches, e.worstCeilingExcess, e.maxTotal, e.last.ceiling)
	}

	// The vacuum must actually reclaim once churn has woken it — UNLESS a
	// snapshot was pinning the watermark, in which case reclaiming nothing is the
	// substrate obeying isolation rather than failing to sweep. Measured: with
	// one reader held open across 6000 writes the vacuum ran 18 passes and freed
	// 16 records, and an unguarded clause here would have called that a defect.
	if e.reclamationPressured() && e.maxActiveSnapshots == 0 && e.unregistered == 0 && e.reclaimedRecords() == 0 {
		fail(ViolationACIDConsistency, "churn reached the vacuum's wake threshold (bound %d, high-water %d"+
			" records, backlog %d) and the vacuum released NOTHING across %d sweep(s), with no snapshot"+
			" registered at any reading to hold a version back: version memory is growing without bound",
			e.last.bound, e.maxTotal, e.last.backlog, e.sweeps())
	}

	// (a) Retained chain depth must return to the bound once nothing is pinning
	// the watermark. Adjudicated on the UNPINNED readings only: a deep retained
	// chain under a live reader is the substrate working as designed.
	if e.unpinnedChainSamples > 0 && e.deepestUnpinned > maxRetainedChainDepth {
		fail(ViolationACIDConsistency, "retained version-chain depth reached %d with NO snapshot registered,"+
			" past the bound of %d (observed over %d unpinned non-empty readings, buckets %v): with nothing"+
			" pinning the watermark every version below it is reclaimable, so a chain this deep is retained"+
			" cost no reader is holding — and chain depth IS read cost per object",
			e.deepestUnpinned, maxRetainedChainDepth, e.unpinnedChainSamples, e.depth.Buckets)
	}

	return out
}

// checkMVCCSubstrateNonVacuity requires that a run actually put the substrate
// under the pressure the clauses in [checkMVCCSubstrate] adjudicate.
//
// It is separate from the adjudication because the two ask different questions.
// A workload that never crosses the vacuum's wake threshold is not FAULTY, it is
// merely uninformative — and a run whose reclamation clauses were never asked is
// exactly the shape of guard this sprint has already found six of: green, and
// proving nothing. A scenario that means to certify the substrate calls this;
// one that merely watches it while doing something else does not.
func checkMVCCSubstrateNonVacuity(tick int64, e *mvccSubstrateEvidence) []Violation {
	const op = "MVCC substrate telemetry non-vacuity"
	var out []Violation
	fail := func(format string, args ...any) {
		out = append(out, Violation{
			Kind: ViolationVacuousRun, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s: ", e.label) + fmt.Sprintf(format, args...),
		})
	}

	if !e.measured {
		fail("the substrate was never sampled")
		return out
	}
	if e.n < 2 {
		fail("only %d reading was folded: no clause that compares two points in the run — the watermark's"+
			" advance, the vacuum's progress — could be evaluated at all", e.n)
	}
	if e.quiesced == 0 {
		fail("no quiescent reading was declared, so the in-flight-commit clause never ran")
	}
	if e.commits() == 0 {
		fail("no transaction committed over the run: the substrate was never given a version to hold")
	}
	if !e.reclamationPressured() {
		fail("the substrate was never put under reclamation pressure: no reading reached the vacuum's wake"+
			" threshold (bound %d, high-water %d live records) and no sweep ever ran, so the vacuum need"+
			" never have started and every reclamation clause passed by not being asked",
			e.last.bound, e.maxTotal)
	}
	if e.sweeps() == 0 {
		fail("the vacuum ran no sweep over the run (high-water %d records against bound %d): nothing was"+
			" reclaimed because nothing swept", e.maxTotal, e.last.bound)
	}
	if e.reclaimedRecords() == 0 {
		fail("the vacuum released no record over the run: the reclamation clauses adjudicated a substrate" +
			" that never reclaimed")
	}
	if e.chainSamples == 0 {
		fail("no reading carried a non-empty chain-depth distribution: the histogram describes each store's" +
			" last COMPLETE sweep and reads empty once everything is released, so the depth bound was" +
			" satisfied by a histogram that measured nothing")
	} else if e.unpinnedChainSamples == 0 {
		fail("every non-empty chain-depth reading (%d of them) was taken with a snapshot registered, and the"+
			" depth bound is adjudicated only on UNPINNED readings — so the bound was never actually applied"+
			" to anything", e.chainSamples)
	}
	return out
}
