package sim

// checkpoint_cadence.go — the background checkpointer's CADENCE, driven on the
// simulation's fake clock (rmp #2476): what the ticker/MaxAge path of
// [checkpoint.Checkpointer] does, what it records in [checkpoint.Stats] when a
// periodic fire FAILS, and when it tries again.
//
// # What this harness drove, and what it never drove
//
// Every checkpoint in this package until now went through one of two SYNCHRONOUS
// entry points: [SimStore.Checkpoint] (which calls
// [checkpoint.Checkpointer.RunCheckpoint] inline, with no loop at all) or an
// explicit [checkpoint.Checkpointer.Trigger] against a loop deliberately
// configured with NO age and NO interval (durable_scenarios.go and
// db_teardown.go both say so in as many words). The cadence arm of
// [checkpoint.Checkpointer.loop] — the ticker, the MaxAge gate, and the
// [checkpoint.WithClock] seam that exists so the DST can drive them — was
// therefore never entered, and the documented behaviour that a periodic fire
// "record[s] the error in Stats.LastError ... so observability surfaces" had
// never been observed once.
//
// # The loop, as VERIFIED in store/checkpoint/checkpoint.go before this was built
//
// The cadence is not the ticker. It is the ticker AND the age gate, in this
// shape:
//
//	case done := <-c.triggerCh:
//	        err := c.runCheckpoint()
//	        lastFire = c.clk.Now()
//	        done <- err
//	case <-tickCh:
//	        if c.cfg.MaxAge > 0 && c.clk.Since(lastFire) >= c.cfg.MaxAge {
//	                _ = c.runCheckpoint()
//	                lastFire = c.clk.Now()
//	        }
//
// Four consequences follow that the [checkpoint.Config] documentation does not
// state, and that this file MEASURES rather than assumes:
//
//   - Interval alone never checkpoints. The ticker is created whenever Interval
//     > 0, but the body is gated on MaxAge > 0, so a checkpointer configured with
//     an interval and no age ticks forever and folds nothing. Config.MaxAge says
//     "Zero disables age-based triggering"; what it disables is the whole cadence.
//     [ArmCheckpointCadenceIntervalOnly] drives exactly that, and it doubles as
//     the control that attributes the other arm's fires to the AGE GATE rather
//     than to the mere arrival of a tick.
//   - With MaxAge unset the loop does not read the clock AT ALL on a tick. The
//     gate is `MaxAge > 0 && Since(lastFire) >= MaxAge` and Go's && short-circuits,
//     so Since is never reached. This was MEASURED here rather than reasoned about:
//     the first version of the control arm waited for a clock observation as its
//     proof that a tick had been consumed, and waited out its whole timeout. The
//     control therefore proves delivery with a probe ticker of its own on the same
//     fake clock, and proves the loop is alive with an explicit trigger.
//   - The first tick always fires. lastFire is the zero [time.Time] until
//     something fires, and [time.Time.Sub] saturates rather than wrapping, so the
//     first tick's elapsed-since is the maximum Duration and clears any MaxAge.
//   - An explicit Trigger RESETS the age timer: the trigger arm assigns lastFire
//     just as the tick arm does. A caller who triggers therefore postpones the
//     next periodic fire by a full MaxAge.
//   - A FAILED periodic fire also resets it — the assignment is unconditional,
//     after a runCheckpoint whose error is discarded. So the retry the package
//     documentation promises "on the next cadence" is a full MaxAge away, NOT the
//     next Interval tick. That is the interval this file pins by measurement.
//
// # How a cadence fire is told from a triggered one
//
// [checkpoint.Stats] counts checkpoints and cannot say which arm produced one, so
// attribution here is threefold and none of the three is an argument from absence:
//
//  1. ARITHMETIC. The arm owns every Trigger call it makes, so the cadence fires
//     are the checkpoints minus the successful triggers.
//  2. CORRESPONDENCE. The clock is advanced one Interval at a time and the fires
//     are recorded as TICK ORDINALS, then compared against the ordinals an
//     independent model of the rule above predicts ([cadenceModel], which knows
//     nothing about the implementation). A run in which triggers did the work
//     cannot reproduce that sequence.
//  3. CONTROL. Before any advance, the run holds the fake clock STILL for a real
//     wall-clock window and requires zero checkpoints and zero clock observations
//     — so no wall time leaks into the cadence — and the interval-only arm shows
//     ticks being delivered — proved with a probe ticker of the harness's own on
//     the same fake clock — and an explicit trigger still being served, with zero
//     cadence fires.
//
// The clock seam itself is proved rather than presumed: the injected clock is a
// counting decorator ([cadenceClock]) over a [clock.Fake], and the run requires
// the loop to have registered EXACTLY ONE ticker on it. A loop that had kept
// reading wall time would register none.
//
// # Durability is the invariant that outranks all of it
//
// A checkpoint that fails must leave the WAL alone: the publish fails before
// [wal.Writer.Sync] and before the phase-3 prefix truncate, so nothing
// acknowledged can be reclaimed by a checkpoint that never published a snapshot.
// The run acknowledges commits BEFORE the failed fire and again BETWEEN the
// failure and its retry, measures the durable WAL image across the failure (it
// must not shrink), and ends by CRASHING the store and reopening through real
// recovery: every acknowledged commit must come back. That clause is
// unconditional and classified [ViolationACIDDurability].

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
)

// The arm names, carried into the evidence so the non-vacuity gate can apply
// each arm's own floor.
const (
	// ArmCheckpointCadenceClean drives the cadence with no fault injected: the
	// ticker/MaxAge path, the explicit-trigger age reset, and the reclaimed-bytes
	// counter.
	ArmCheckpointCadenceClean = "cadence-clean"
	// ArmCheckpointCadenceTransientFault is the same plan with a one-shot fsync
	// fault armed on one periodic fire, so the failure lands in
	// [checkpoint.Stats.LastError] and the NEXT cadence fire has to recover from
	// it.
	ArmCheckpointCadenceTransientFault = "cadence-transient-fault"
	// ArmCheckpointCadenceIntervalOnly is the CONTROL: an interval with no
	// MaxAge. Ticks are delivered and the loop stays alive and responsive, and
	// nothing is ever folded from the cadence.
	ArmCheckpointCadenceIntervalOnly = "cadence-interval-only"
)

// cadenceDiskSeedMix decorrelates this scenario's SimDisk sub-stream from the run
// seed's other consumers, so arming a fault here never perturbs another
// scenario's reproducible fault stream.
const cadenceDiskSeedMix uint64 = 0x2476_CADE_0FF1_CE55

// cadenceLabel is the node label every acknowledged key carries, so the
// recovered graph can be interrogated per key rather than by count.
const cadenceLabel = "CadenceCommit"

// The cadence geometry. These are FAKE durations: no wall time passes for them,
// and the whole point of the [checkpoint.WithClock] seam is that none can.
const (
	// cadenceInterval is the ticker period and the size of one advance.
	cadenceInterval = 10 * time.Millisecond
	// cadenceMaxAge is the age gate. It is a whole multiple of the interval, so
	// one cadence window is exactly cadenceWindowTicks advances and the predicted
	// fire ordinals are exact rather than approximate.
	cadenceMaxAge = 40 * time.Millisecond
	// cadenceWindowTicks is that multiple: how many advances one MaxAge window
	// takes. It is derived here so a change to either duration above cannot leave
	// the plan's arithmetic behind.
	cadenceWindowTicks = int(cadenceMaxAge / cadenceInterval)
)

// The plan's commit budgets. Each commit is two ops (AddNode + SetNodeLabel), so
// every one of them is a durable WAL round the checkpointer can later reclaim.
const (
	// cadencePrefixCommits are acknowledged before the loop is started, so the
	// first cadence fire has a real WAL prefix to fold and reclaim.
	cadencePrefixCommits = 6
	// cadenceMidCommits are acknowledged between the last clean fire and the
	// faulted one, so the failed fire has something it MUST NOT reclaim.
	cadenceMidCommits = 4
	// cadenceDuringFailureCommits are acknowledged while the checkpointer is in
	// its failed state (after the faulted fire, before its retry). Without them
	// "durability held across the failure" would say nothing about the window the
	// failure actually opened.
	cadenceDuringFailureCommits = 3
	// cadenceTailCommits are acknowledged after the LAST checkpoint, so they live
	// only in the WAL suffix and recovery must replay them. Without them every
	// acknowledged key would come back from the snapshot and the durability
	// clause would be answered however the WAL was left.
	cadenceTailCommits = 4
)

// The real-time bounds. No fake time is governed by any of these: they bound
// only how long this run waits for the loop GOROUTINE to observe what the fake
// clock already delivered.
const (
	// cadenceObserveTimeout bounds every wait on the loop goroutine, so a
	// regression that stops consuming ticks fails the run instead of hanging the
	// package until the test binary's own timeout.
	cadenceObserveTimeout = 30 * time.Second
	// cadenceSettleWindow is how long a tick the plan predicts will NOT fire is
	// watched before it is recorded as quiet. A non-firing tick evaluates the gate
	// and returns to the select without touching Stats, so there is nothing to
	// wait for; the window is slack for the scheduler, not for a checkpoint.
	cadenceSettleWindow = 50 * time.Millisecond
	// cadenceFrozenWindow is how long the fake clock is held STILL while the loop
	// runs, to show that real time alone fires nothing.
	cadenceFrozenWindow = 100 * time.Millisecond
	// cadenceQuiesceWindow is how long the loop's clock use must stay UNCHANGED
	// after a fire before the next advance is allowed.
	//
	// It closes a real race rather than padding for comfort. The loop assigns
	// lastFire AFTER runCheckpoint returns, while the checkpoint counter this run
	// polls is incremented INSIDE it — so an advance issued the instant the
	// counter moves can be read by that assignment and push lastFire a whole
	// interval into the future, shifting every subsequent predicted fire by one
	// tick. Waiting for the loop to stop touching the clock puts the assignment
	// strictly before the next advance. The wait is keyed on clock USE rather than
	// on a call count so it does not encode how many Now/Since calls one fire
	// happens to make.
	cadenceQuiesceWindow = 25 * time.Millisecond
	// cadencePollInterval is the granularity of every bounded wait above.
	cadencePollInterval = 200 * time.Microsecond
)

// -----------------------------------------------------------------------------
// The injected clock
// -----------------------------------------------------------------------------

// cadenceClock is the [clock.Clock] handed to the checkpointer: a [clock.Fake]
// that counts how the cadence loop USES it. The counts are what make the clock
// seam an observation rather than an assumption — a loop that had kept reading
// wall time would register no ticker and make no Now/Since call — and the Since
// count in particular is how this file knows a delivered tick has actually been
// consumed by the loop, since the age gate calls Since exactly once per tick.
//
// # Concurrency contract
//
// cadenceClock is safe for concurrent use: [clock.Fake] is, and the counters are
// atomics. The run advances it from the controlling goroutine while the
// checkpoint loop reads it from its own.
type cadenceClock struct {
	fake    *clock.Fake
	nows    atomic.Int64
	sinces  atomic.Int64
	tickers atomic.Int64
	timers  atomic.Int64
}

// newCadenceClock returns a fake clock positioned at the Unix epoch, counting
// every call the cadence loop makes into it.
func newCadenceClock() *cadenceClock {
	return &cadenceClock{fake: clock.NewFake(time.Unix(0, 0).UTC())}
}

// Now reports the fake's current instant and counts the call.
func (c *cadenceClock) Now() time.Time {
	c.nows.Add(1)
	return c.fake.Now()
}

// Since reports the fake's elapsed time since t and counts the call. The cadence
// loop calls it exactly once per delivered tick to evaluate the age gate, which
// is what makes this counter a tick-consumption signal.
func (c *cadenceClock) Since(t time.Time) time.Duration {
	c.sinces.Add(1)
	return c.fake.Since(t)
}

// Until reports the fake's duration until t.
func (c *cadenceClock) Until(t time.Time) time.Duration { return c.fake.Until(t) }

// After returns a channel that fires once the fake advances at least d.
func (c *cadenceClock) After(d time.Duration) <-chan time.Time { return c.fake.After(d) }

// NewTimer registers a one-shot timer on the fake and counts the registration.
func (c *cadenceClock) NewTimer(d time.Duration) clock.Timer {
	t := c.fake.NewTimer(d)
	c.timers.Add(1)
	return t
}

// NewTicker registers a ticker on the fake and counts the registration. The
// counter is incremented AFTER the fake has registered the waiter, so a reader
// that observes a non-zero count knows the ticker will receive the next advance.
func (c *cadenceClock) NewTicker(d time.Duration) clock.Ticker {
	t := c.fake.NewTicker(d)
	c.tickers.Add(1)
	return t
}

// Advance moves the fake forward by d, delivering every tick the interval
// crosses. It is called only from the controlling goroutine.
func (c *cadenceClock) Advance(d time.Duration) { c.fake.Advance(d) }

// probeTicker registers a ticker on the underlying fake WITHOUT counting it, so
// the harness can observe that an advance really delivers at this period without
// perturbing the count that attributes the loop's own ticker to the seam. Two
// waiters of the same period are both due at the same instants, so a tick the
// probe receives is a tick the cadence loop's ticker received too.
func (c *cadenceClock) probeTicker(d time.Duration) clock.Ticker { return c.fake.NewTicker(d) }

// Nows, Sinces, Tickers report the loop's use of the seam.
func (c *cadenceClock) Nows() int64    { return c.nows.Load() }
func (c *cadenceClock) Sinces() int64  { return c.sinces.Load() }
func (c *cadenceClock) Tickers() int64 { return c.tickers.Load() }

// -----------------------------------------------------------------------------
// The independent model of the documented rule
// -----------------------------------------------------------------------------

// cadenceModel predicts, from the CONFIGURATION alone, which tick ordinals fire.
// It is an independent restatement of the loop's rule — a tick fires when MaxAge
// has elapsed since the last fire, an explicit trigger counts as a fire, and the
// age is unbounded until the first one — and it never consults the checkpointer,
// so comparing the observed fire ordinals against it is a real check and not a
// tautology.
//
// triggerResets selects which HYPOTHESIS the model embodies: with it set, an
// explicit trigger postpones the next periodic fire by a full MaxAge (what
// store/checkpoint actually does); cleared, the age keeps running from the last
// periodic fire. The run computes both and requires the two to DISAGREE, so the
// arm cannot claim to have settled the question with a plan that could not tell
// the answers apart.
type cadenceModel struct {
	interval, maxAge time.Duration
	elapsed          time.Duration
	lastFire         time.Duration
	fired            bool
	triggerResets    bool
}

// tick advances the model by one interval and reports whether the rule predicts
// a periodic fire.
func (m *cadenceModel) tick() bool {
	m.elapsed += m.interval
	if m.maxAge <= 0 {
		return false
	}
	// Until something fires, lastFire is the zero time and time.Time.Sub
	// saturates at the maximum Duration, so the age is effectively unbounded and
	// the first delivered tick always clears the gate.
	if m.fired && m.elapsed-m.lastFire < m.maxAge {
		return false
	}
	m.lastFire = m.elapsed
	m.fired = true
	return true
}

// trigger models an explicit Trigger at the model's current instant.
func (m *cadenceModel) trigger() {
	if m.triggerResets {
		m.lastFire = m.elapsed
		m.fired = true
	}
}

// -----------------------------------------------------------------------------
// Configuration and evidence
// -----------------------------------------------------------------------------

// CheckpointCadenceConfig parameterises one cadence run. The zero value is not
// meaningful: [RunCheckpointCadence] normalises Arm, MaxAge and Interval, but the
// arm-selecting flags are deliberately explicit.
type CheckpointCadenceConfig struct {
	// Seed is the master seed for the SimDisk sub-stream.
	Seed uint64
	// Arm names the variant under test. Empty defaults to
	// [ArmCheckpointCadenceClean].
	Arm string
	// MaxAge and Interval are the cadence geometry handed to
	// [checkpoint.Config]. Zero values take [cadenceMaxAge] and
	// [cadenceInterval]; a NEGATIVE MaxAge is how the interval-only control asks
	// for MaxAge: 0 without colliding with the "unset" meaning of zero.
	MaxAge   time.Duration
	Interval time.Duration
	// FaultOnCadenceFire arms a one-shot fsync fault immediately before the
	// faulted window, so exactly one periodic fire fails inside its snapshot
	// publish — before the WAL is synced and long before the prefix truncate.
	FaultOnCadenceFire bool
	// SkipTriggerPhase omits the explicit-trigger phase (and therefore the
	// age-timer question). The interval-only control uses it: with no MaxAge there
	// is no age timer to reset.
	SkipTriggerPhase bool
}

// CheckpointCadenceEvidence is what one cadence run OBSERVED. It carries
// measurements and no verdict, so the adjudicators below are pure functions of it
// and can be falsified by a doctored value rather than by hoping a real run
// misbehaves.
type CheckpointCadenceEvidence struct {
	// Arm is the variant that produced this evidence.
	Arm string

	// MaxAgeNS and IntervalNS are the geometry read back off the CONSTRUCTED
	// checkpointer's behaviour plan, not off the request: [checkpoint.New]
	// derives Interval from MaxAge when it is left at zero.
	MaxAgeNS   int64
	IntervalNS int64

	// TickersRegistered is how many tickers the loop took from the injected
	// clock. It must be 1: the cadence is on the seam, not on wall time.
	TickersRegistered int64
	// ClockNowCalls and ClockSinceCalls are the loop's total use of the seam, and
	// GateReadsOnTicks the part of it made while a TICK was being serviced. The
	// split matters: with MaxAge unset the age gate short-circuits before reading
	// the clock, so GateReadsOnTicks is 0 on that arm while the run's own explicit
	// trigger still moves the totals.
	ClockNowCalls    int64
	ClockSinceCalls  int64
	GateReadsOnTicks int64
	// SimulatedElapsedNS is how much FAKE time the run advanced, and
	// RealElapsedNS how long it took on the wall clock. The pair is the witness
	// that the cadence is virtual: the simulated total is a large multiple of
	// MaxAge whatever the real one happens to be.
	SimulatedElapsedNS int64
	RealElapsedNS      int64

	// The frozen-clock control, taken with the loop running and the ticker
	// registered: FrozenWindowNS of REAL time in which the fake clock did not
	// move.
	FrozenWindowNS     int64
	FrozenWindowFires  uint64
	FrozenWindowSinces int64

	// Ticks is how many one-interval advances the run made and TicksDelivered how
	// many of them the fake clock actually delivered to a waiter, measured with the
	// harness's own probe ticker on the same clock at the same period. The pair is
	// what makes "nothing fired" a statement about the AGE GATE rather than about a
	// tick that never arrived.
	Ticks          int
	TicksDelivered int
	// TriggerCalls is how many explicit [checkpoint.Checkpointer.Trigger] calls the
	// run issued, and TriggeredFires how many of those returned nil (and so
	// incremented the checkpoint counter).
	TriggerCalls   int
	TriggeredFires int
	// Checkpoints is the checkpointer's own lifetime counter at the end, and
	// CadenceFires the arithmetic attribution: the checkpoints this run did not
	// ask for.
	Checkpoints  uint64
	CadenceFires int

	// FireTicks are the tick ordinals at which the loop RAN a checkpoint —
	// successful or failed — observed as a change in the checkpoint counter or in
	// LastError. PredictedFireTicks are the ordinals the independent model
	// predicts, and PredictedFireTicksNoReset the ordinals the rival hypothesis
	// (an explicit trigger does NOT reset the age timer) predicts.
	FireTicks                 []int
	PredictedFireTicks        []int
	PredictedFireTicksNoReset []int

	// TriggerTick is the tick ordinal after which the explicit trigger was
	// issued (0 when the arm skipped that phase), and UnresetFireTick the ordinal
	// at which the rival hypothesis would have fired. FiredAtUnresetTick records
	// whether it did.
	TriggerTick                   int
	UnresetFireTick               int
	FiredAtUnresetTick            bool
	TicksFromTriggerToCadenceFire int

	// The transient failure. FailedFireTick is the ordinal whose fire failed (0
	// if none did), FailedFireErr what [checkpoint.Stats.LastError] then held, and
	// RetryFireTick the ordinal at which the next cadence fire ran.
	FaultArmed              bool
	FailedFireTick          int
	FailedFireErr           string
	FailedFireIsSimFault    bool
	TicksFromFailureToRetry int
	RetryFireTick           int
	// LastErrorAfterRetry is what LastError held once the retry had succeeded:
	// the answer to "does a success clear it".
	LastErrorAfterRetry string

	// The counters bracketing the failure. A failed fire must advance neither.
	CheckpointsBeforeFailure uint64
	CheckpointsAfterFailure  uint64
	WALTruncBeforeFailure    uint64
	WALTruncAfterFailure     uint64
	WALTruncAfterRetry       uint64
	// WALBytesBeforeFailure and WALBytesAfterFailure are the DURABLE WAL image on
	// the SimDisk across the failed fire. It must not shrink: a checkpoint that
	// did not publish must not have reclaimed anything.
	//
	// On a fault-free arm these three pairs bracket the same window with no failure
	// in it — the fire inside it succeeds and legitimately reclaims the prefix — so
	// they are informational there and the clauses that read them are gated on
	// FailedFireTick.
	WALBytesBeforeFailure int
	WALBytesAfterFailure  int

	// CheckpointsNonMonotonic and WALTruncNonMonotonic record whether either
	// lifetime counter was ever observed to DECREASE across the run's samples.
	CheckpointsNonMonotonic bool
	WALTruncNonMonotonic    bool
	// WALTruncTotal is the reclaimed-bytes counter at the end, and
	// WALTruncAdvances how many samples it strictly increased at.
	WALTruncTotal    uint64
	WALTruncAdvances int

	// AckedKeys are every key acknowledged during the run and MissingAckedKeys
	// those the reopen could not find. MissingAckedKeys non-empty is a durability
	// defect.
	AckedKeys        []string
	MissingAckedKeys []string
	// AckedDuringFailure is how many commits were acknowledged while the
	// checkpointer was in its failed state, and AckedAfterLastCheckpoint how many
	// landed after the final fire and so live only in the WAL suffix.
	AckedDuringFailure       int
	AckedAfterLastCheckpoint int
	// RecoveredWALOps is how many WAL ops the reopen's recovery replayed, WALBytes
	// the durable image it read, and SnapshotPublished whether a snapshot manifest
	// sits beside it.
	RecoveredWALOps   int
	WALBytes          int
	SnapshotPublished bool
	// ReopenClean is whether the reopen's recovery found no genuine corruption.
	ReopenClean bool
}

// String renders the evidence for a failure message or a test log.
func (e *CheckpointCadenceEvidence) String() string {
	return fmt.Sprintf("checkpoint-cadence[%s]: maxAge=%s interval=%s tickers=%d ticks=%d simElapsed=%s realElapsed=%s "+
		"frozen(%s)=fires:%d sinces:%d clock(now=%d since=%d gateReads=%d) delivered=%d checkpoints=%d cadenceFires=%d triggers=%d/%d "+
		"fireTicks=%v predicted=%v predictedNoReset=%v triggerTick=%d unresetTick=%d firedAtUnreset=%t trigger->fire=%d "+
		"fault=%t failedTick=%d failedErr=%q simFault=%t retryTick=%d failure->retry=%d lastErrAfterRetry=%q "+
		"cp(%d->%d) walTrunc(%d->%d->%d total=%d advances=%d) walBytes(%d->%d) monotonic(cp=%t trunc=%t) "+
		"acked=%d duringFailure=%d afterLastCp=%d missing=%d walBytes=%d walOpsReplayed=%d snapshot=%t clean=%t",
		e.Arm, time.Duration(e.MaxAgeNS), time.Duration(e.IntervalNS), e.TickersRegistered, e.Ticks,
		time.Duration(e.SimulatedElapsedNS), time.Duration(e.RealElapsedNS),
		time.Duration(e.FrozenWindowNS), e.FrozenWindowFires, e.FrozenWindowSinces,
		e.ClockNowCalls, e.ClockSinceCalls, e.GateReadsOnTicks, e.TicksDelivered, e.Checkpoints, e.CadenceFires, e.TriggeredFires, e.TriggerCalls,
		e.FireTicks, e.PredictedFireTicks, e.PredictedFireTicksNoReset,
		e.TriggerTick, e.UnresetFireTick, e.FiredAtUnresetTick, e.TicksFromTriggerToCadenceFire,
		e.FaultArmed, e.FailedFireTick, e.FailedFireErr, e.FailedFireIsSimFault, e.RetryFireTick,
		e.TicksFromFailureToRetry, e.LastErrorAfterRetry,
		e.CheckpointsBeforeFailure, e.CheckpointsAfterFailure,
		e.WALTruncBeforeFailure, e.WALTruncAfterFailure, e.WALTruncAfterRetry, e.WALTruncTotal, e.WALTruncAdvances,
		e.WALBytesBeforeFailure, e.WALBytesAfterFailure,
		!e.CheckpointsNonMonotonic, !e.WALTruncNonMonotonic,
		len(e.AckedKeys), e.AckedDuringFailure, e.AckedAfterLastCheckpoint, len(e.MissingAckedKeys),
		e.WALBytes, e.RecoveredWALOps, e.SnapshotPublished, e.ReopenClean)
}

// RetryWaitedAFullWindow reports whether the fire that followed a FAILED one was
// a whole MaxAge window away rather than the next tick. It is the measurement of
// the retry cadence the package documentation leaves unstated, and it is a
// witness in the log as well as a clause.
func (e *CheckpointCadenceEvidence) RetryWaitedAFullWindow() bool {
	return e.TicksFromFailureToRetry == cadenceWindowTicks
}

// TriggerResetTheAgeTimer reports whether the observed fire sequence matched the
// hypothesis that an explicit trigger postpones the next periodic fire, and not
// the rival one.
func (e *CheckpointCadenceEvidence) TriggerResetTheAgeTimer() bool {
	return e.TriggerTick > 0 && !e.FiredAtUnresetTick &&
		equalInts(e.FireTicks, e.PredictedFireTicks) &&
		!equalInts(e.PredictedFireTicks, e.PredictedFireTicksNoReset)
}

// -----------------------------------------------------------------------------
// The environment
// -----------------------------------------------------------------------------

// cpCadenceEnv is one durable stack under test: a SimDisk, a full-stack store on
// it, an injected fake clock, and the [checkpoint.Checkpointer] whose cadence
// loop is the subject.
type cpCadenceEnv struct {
	disk     *SimDisk
	st       *SimStore
	clk      *cadenceClock
	cp       *checkpoint.Checkpointer[string, float64]
	cpCancel context.CancelFunc
	cfg      simStoreConfig
	interval time.Duration
	// gateObservable records whether the age gate reads the clock at all. It does
	// not when MaxAge is unset (the && short-circuits), so the run cannot use a
	// clock observation as its tick-consumption barrier on that arm.
	gateObservable bool
	// probe is the harness's own ticker on the same fake clock, at the same
	// period: a tick it receives is proof the advance delivered one.
	probe clock.Ticker
	// ticksDelivered counts the probe ticks received across the run.
	ticksDelivered int
	// gateReads counts the clock observations the loop made WHILE an advance was
	// being serviced, so they are attributable to a tick and not to an explicit
	// trigger the run issued afterwards.
	gateReads int64
	// simulated is the total FAKE time advanced so far.
	simulated time.Duration
	// tick is the ordinal of the last advance.
	tick int
	// truncSamples records every WALTruncBytes reading in order, so monotonicity
	// is adjudicated over the series rather than over its endpoints.
	truncSamples []uint64
	cpSamples    []uint64
}

// openCadenceEnv stands up the durable stack: a store on a fresh SimDisk and a
// checkpointer over it wired with the SAME seams [SimStore.Checkpoint] uses,
// plus the counting fake clock. The loop is NOT started here — the run
// acknowledges a durable prefix first, so the first cadence fire has a real WAL
// prefix to fold.
func openCadenceEnv(cfg CheckpointCadenceConfig) (*cpCadenceEnv, error) {
	scfg := fullStackStoreConfig()
	disk := NewSimDisk(NewSeed(cfg.Seed^cadenceDiskSeedMix), 0) // faultRate 0: only the armed one-shot fault fires
	st, err := OpenSimStore(disk, scfg)
	if err != nil {
		return nil, fmt.Errorf("sim: checkpoint-cadence open store: %w", err)
	}

	maxAge := cfg.MaxAge
	if maxAge == 0 {
		maxAge = cadenceMaxAge
	}
	if maxAge < 0 {
		maxAge = 0 // the interval-only control
	}
	interval := cfg.Interval
	if interval == 0 {
		interval = cadenceInterval
	}

	clk := newCadenceClock()
	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](
		checkpoint.Config{Dir: scfg.dir, MaxAge: maxAge, Interval: interval}, st.graph, st.wlog, &unusedMu,
		checkpoint.WithClock[string, float64](clk),
		checkpoint.WithCommitSerialiser[string, float64](st.store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](st.store.Codec()),
		checkpoint.WithWeightCodec[string, float64](st.store.WeightCodec()),
		checkpoint.WithSnapshotFS[string, float64](simCheckpointBackend[string, float64]{disk: disk}),
		checkpoint.WithConstraintSpecs[string, float64](st.engine.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](st.engine.IndexSpecsForSnapshot),
	)
	return &cpCadenceEnv{disk: disk, st: st, clk: clk, cp: cp, cfg: scfg, interval: interval, gateObservable: maxAge > 0}, nil
}

// start launches the cadence loop and waits until it has registered its ticker on
// the injected clock. Advancing before that would deliver a tick to nobody, so
// the wait is a correctness precondition of every advance below — and the
// registration count is itself the evidence that the loop reads the seam.
func (e *cpCadenceEnv) start() error {
	ctx, cancel := context.WithCancel(context.Background())
	e.cpCancel = cancel
	e.cp.Start(ctx)
	if err := waitForCadence(func() bool { return e.clk.Tickers() >= 1 },
		"the cadence loop never registered a ticker on the injected clock"); err != nil {
		return err
	}
	// The delivery probe is registered AFTER the loop's ticker and BEFORE any
	// advance, so both waiters share the same deadlines from here on.
	e.probe = e.clk.probeTicker(e.interval)
	return nil
}

// stop stops the cadence loop and releases its context. It is idempotent
// ([checkpoint.Checkpointer.Stop] is), so it is safe on every exit path.
func (e *cpCadenceEnv) stop() {
	if e.probe != nil {
		e.probe.Stop()
		e.probe = nil
	}
	if e.cp != nil {
		e.cp.Stop()
	}
	if e.cpCancel != nil {
		e.cpCancel()
	}
}

// sample records the checkpointer's lifetime counters, so monotonicity is
// adjudicated over an ordered series.
func (e *cpCadenceEnv) sample() checkpoint.Stats {
	st := e.cp.Stats()
	e.cpSamples = append(e.cpSamples, st.Checkpoints)
	e.truncSamples = append(e.truncSamples, st.WALTruncBytes)
	return st
}

// commit acknowledges one two-op transaction (AddNode + SetNodeLabel). It goes
// through the transaction layer rather than the Cypher engine so the arm controls
// exactly what a commit is: one transaction, two ops, one durable WAL round.
func (e *cpCadenceEnv) commit(ctx context.Context, key string) error {
	tx, err := e.st.store.BeginCtx(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", key, err)
	}
	if aerr := tx.AddNode(key); aerr != nil {
		_ = tx.Rollback()
		return fmt.Errorf("add node %s: %w", key, aerr)
	}
	if lerr := tx.SetNodeLabel(key, cadenceLabel); lerr != nil {
		_ = tx.Rollback()
		return fmt.Errorf("label node %s: %w", key, lerr)
	}
	return tx.Commit()
}

// walBytes reads the size of the durable WAL image on the SimDisk. An absent file
// is reported as zero.
func (e *cpCadenceEnv) walBytes() (int, error) {
	path := walPathFor(e.cfg.dir)
	if !e.disk.Exists(path) {
		return 0, nil
	}
	image, err := e.disk.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("sim: checkpoint-cadence read WAL image: %w", err)
	}
	return len(image), nil
}

// advance moves the fake clock forward by exactly one interval and reports
// whether the cadence loop ran a checkpoint in response.
//
// expectFire selects how long the run watches for the outcome, and it comes from
// the PLAN's own arithmetic ([cadenceModel]) rather than from the implementation:
// a tick predicted to fire is waited on generously, because a real checkpoint runs
// on the loop goroutine and takes as long as it takes; a tick predicted to stay
// quiet is watched for [cadenceSettleWindow] only, because a closed gate returns
// to the select without touching anything. A prediction that is wrong in either
// direction therefore surfaces as a mismatch in the fire ordinals rather than as
// a silent pass.
func (e *cpCadenceEnv) advance(expectFire bool) (bool, error) {
	before := e.cp.Stats()
	sincesBefore := e.clk.Sinces()
	// Every clock observation made between here and this call's return happened
	// while a TICK was being serviced, so it is the age gate (and, on a fire, the
	// checkpoint's own duration measurement) rather than anything the run asked for
	// out of band.
	defer func() { e.gateReads += e.clk.Sinces() - sincesBefore }()

	e.tick++
	e.simulated += e.interval
	e.clk.Advance(e.interval)

	// The fake delivers to every due waiter inside Advance, so the probe's tick is
	// already buffered here: receiving it is direct evidence that this advance
	// delivered a tick at this period, and therefore that the loop's own ticker
	// received one too.
	select {
	case <-e.probe.C():
		e.ticksDelivered++
	default:
	}

	// The age gate calls Since exactly once per delivered tick, so this is the loop
	// telling us it has consumed the tick rather than a guess about timing. It is
	// available only when MaxAge is set: the gate is `MaxAge > 0 && Since(...)` and
	// && short-circuits, so a MaxAge-less loop wakes on the tick and returns to the
	// select without touching the clock (MEASURED — see the file comment).
	if e.gateObservable {
		if err := waitForCadence(func() bool { return e.clk.Sinces() > sincesBefore },
			fmt.Sprintf("the cadence loop never observed tick %d", e.tick)); err != nil {
			return false, err
		}
	}

	changed := func() bool {
		st := e.cp.Stats()
		return st.Checkpoints != before.Checkpoints || st.LastError != before.LastError
	}
	if expectFire {
		if err := waitForCadence(changed,
			fmt.Sprintf("tick %d was predicted to fire a checkpoint and none ran", e.tick)); err != nil {
			e.sample()
			return false, err
		}
		e.quiesce()
		e.sample()
		return true, nil
	}
	deadline := time.Now().Add(cadenceSettleWindow)
	for time.Now().Before(deadline) {
		if changed() {
			e.quiesce()
			e.sample()
			return true, nil
		}
		time.Sleep(cadencePollInterval)
	}
	e.sample()
	return false, nil
}

// quiesce blocks until the cadence loop has left the clock alone for
// [cadenceQuiesceWindow], which is how the run knows the loop has finished
// recording the fire it just observed — including the lastFire assignment that
// happens after runCheckpoint returns. See cadenceQuiesceWindow for the race it
// closes.
func (e *cpCadenceEnv) quiesce() {
	last := e.clk.Nows() + e.clk.Sinces()
	stable := time.Now()
	for time.Since(stable) < cadenceQuiesceWindow {
		time.Sleep(cadencePollInterval)
		if now := e.clk.Nows() + e.clk.Sinces(); now != last {
			last = now
			stable = time.Now()
		}
	}
}

// -----------------------------------------------------------------------------
// The run
// -----------------------------------------------------------------------------

// RunCheckpointCadence drives one cadence variant end to end: acknowledge a
// durable prefix, start the loop, hold the fake clock still to show real time
// fires nothing, then advance it one interval at a time through a plan that
// crosses several MaxAge windows — optionally failing one periodic fire with a
// one-shot fsync fault and optionally interrupting the cadence with an explicit
// trigger — and finally CRASH the store and reopen it through real recovery.
//
// It installs no global state and may be run beside other work; the fake clock,
// the SimDisk and the store are all owned by this call.
func RunCheckpointCadence(ctx context.Context, cfg CheckpointCadenceConfig) (CheckpointCadenceEvidence, error) {
	if cfg.Arm == "" {
		cfg.Arm = ArmCheckpointCadenceClean
	}
	started := time.Now()

	env, err := openCadenceEnv(cfg)
	if err != nil {
		return CheckpointCadenceEvidence{Arm: cfg.Arm}, err
	}
	// The loop is stopped on every exit path, so no arm can leak the goroutine
	// into another test.
	defer env.stop()

	maxAge := cfg.MaxAge
	if maxAge == 0 {
		maxAge = cadenceMaxAge
	}
	if maxAge < 0 {
		maxAge = 0
	}
	ev := CheckpointCadenceEvidence{
		Arm:            cfg.Arm,
		MaxAgeNS:       maxAge.Nanoseconds(),
		IntervalNS:     env.interval.Nanoseconds(),
		FaultArmed:     cfg.FaultOnCadenceFire,
		FrozenWindowNS: cadenceFrozenWindow.Nanoseconds(),
	}

	// The two rival models of the age rule, stepped in lockstep with the real
	// loop below. Neither consults the checkpointer.
	model := &cadenceModel{interval: env.interval, maxAge: maxAge, triggerResets: true}
	rival := &cadenceModel{interval: env.interval, maxAge: maxAge, triggerResets: false}

	// A durable prefix, acknowledged BEFORE the loop starts: the first cadence
	// fire has a real WAL prefix to fold and reclaim.
	for i := range cadencePrefixCommits {
		key := fmt.Sprintf("prefix-%04d", i)
		if cerr := env.commit(ctx, key); cerr != nil {
			return ev, fmt.Errorf("sim: checkpoint-cadence prefix commit: %w", cerr)
		}
		ev.AckedKeys = append(ev.AckedKeys, key)
	}

	if serr := env.start(); serr != nil {
		return ev, serr
	}

	// The frozen-clock control: the loop is running and its ticker is registered,
	// but the fake clock does not move. Real time passing must fire nothing and
	// must not even be observed by the loop, which is what proves no wall clock
	// leaks into the cadence.
	frozenSinces := env.clk.Sinces()
	frozenChecks := env.cp.Stats().Checkpoints
	time.Sleep(cadenceFrozenWindow)
	ev.FrozenWindowFires = env.cp.Stats().Checkpoints - frozenChecks
	ev.FrozenWindowSinces = env.clk.Sinces() - frozenSinces

	// step advances one interval, keeps both models in lockstep, and records the
	// tick ordinal when the loop actually ran a checkpoint.
	step := func() error {
		predicted := model.tick()
		if rival.tick() {
			ev.PredictedFireTicksNoReset = append(ev.PredictedFireTicksNoReset, env.tick+1)
		}
		if predicted {
			ev.PredictedFireTicks = append(ev.PredictedFireTicks, env.tick+1)
		}
		fired, aerr := env.advance(predicted)
		if aerr != nil {
			return aerr
		}
		if fired {
			ev.FireTicks = append(ev.FireTicks, env.tick)
		}
		return nil
	}

	// Phase A — clean cadence. Two full MaxAge windows past the always-firing
	// first tick, so the correspondence between simulated time and fires is a
	// sequence rather than a single event.
	for range 2*cadenceWindowTicks + 1 {
		if serr := step(); serr != nil {
			return ev, serr
		}
	}

	if maxAge > 0 {
		if ferr := runCadenceFaultPhase(ctx, env, &ev, cfg, step); ferr != nil {
			return ev, ferr
		}
		if !cfg.SkipTriggerPhase {
			if terr := runCadenceTriggerPhase(env, &ev, model, rival, step); terr != nil {
				return ev, terr
			}
		}
	} else {
		// The interval-only control has no windows to cross: keep ticking so the
		// "ticks delivered, nothing folded" measurement is made over a span far
		// longer than any MaxAge the other arms use.
		for range 3 * cadenceWindowTicks {
			if serr := step(); serr != nil {
				return ev, serr
			}
		}
		// The LIVENESS proof this arm needs. A MaxAge-less loop never touches the
		// clock on a tick, so "nothing fired" would otherwise be equally satisfied
		// by a loop that had died: the explicit trigger shows it is alive and
		// servicing its select, and therefore that it declined every tick rather
		// than missing them.
		ev.TriggerCalls++
		if terr := env.cp.Trigger(); terr != nil {
			return ev, fmt.Errorf("sim: checkpoint-cadence liveness trigger: %w", terr)
		}
		ev.TriggeredFires++
		env.sample()
	}

	// A committed tail that exists ONLY in the WAL suffix. Without it the last
	// checkpoint has already folded every acknowledged key into the snapshot and
	// "every acknowledged commit was recovered" is answered by the snapshot
	// however the cadence behaved.
	for i := range cadenceTailCommits {
		key := fmt.Sprintf("tail-%04d", i)
		if cerr := env.commit(ctx, key); cerr != nil {
			return ev, fmt.Errorf("sim: checkpoint-cadence tail commit: %w", cerr)
		}
		ev.AckedKeys = append(ev.AckedKeys, key)
		ev.AckedAfterLastCheckpoint++
	}

	final := env.sample()
	ev.Ticks = env.tick
	ev.TicksDelivered = env.ticksDelivered
	ev.Checkpoints = final.Checkpoints
	ev.CadenceFires = int(final.Checkpoints) - ev.TriggeredFires
	ev.LastErrorAfterRetry = final.LastError
	ev.WALTruncTotal = final.WALTruncBytes
	ev.TickersRegistered = env.clk.Tickers()
	ev.ClockNowCalls = env.clk.Nows()
	ev.ClockSinceCalls = env.clk.Sinces()
	ev.GateReadsOnTicks = env.gateReads
	ev.SimulatedElapsedNS = env.simulated.Nanoseconds()
	ev.CheckpointsNonMonotonic, _ = seriesMonotonicity(env.cpSamples)
	ev.WALTruncNonMonotonic, ev.WALTruncAdvances = seriesMonotonicity(env.truncSamples)

	if bytes, berr := env.walBytes(); berr == nil {
		ev.WALBytes = bytes
	} else {
		return ev, berr
	}
	ev.SnapshotPublished = hasDurableSnapshot(env.disk, env.cfg.dir)

	// Stop the loop BEFORE the crash so no checkpoint can be running against a
	// store that is about to be dropped, then crash (drop the engine, keep the
	// byte image — never a graceful flush) and reopen through real recovery.
	env.stop()
	env.st.Crash()

	re, err := OpenSimStore(env.disk, env.cfg)
	if err != nil {
		return ev, fmt.Errorf("sim: checkpoint-cadence reopen: %w", err)
	}
	defer func() { _ = re.Close() }()
	ev.ReopenClean = re.Clean()
	ev.RecoveredWALOps = re.WALOps()
	for _, key := range ev.AckedKeys {
		if !re.graph.HasNodeLabel(key, cadenceLabel) {
			ev.MissingAckedKeys = append(ev.MissingAckedKeys, key)
		}
	}
	ev.RealElapsedNS = time.Since(started).Nanoseconds()
	return ev, nil
}

// runCadenceFaultPhase drives the window in which one periodic fire fails.
//
// The commits either side of the failure are what give it meaning: the first
// group is what the failed fire MUST NOT reclaim, and the second is acknowledged
// while the checkpointer is in its failed state, which is the window durability
// has to survive.
func runCadenceFaultPhase(
	ctx context.Context,
	env *cpCadenceEnv,
	ev *CheckpointCadenceEvidence,
	cfg CheckpointCadenceConfig,
	step func() error,
) error {
	for i := range cadenceMidCommits {
		key := fmt.Sprintf("mid-%04d", i)
		if cerr := env.commit(ctx, key); cerr != nil {
			return fmt.Errorf("sim: checkpoint-cadence mid commit: %w", cerr)
		}
		ev.AckedKeys = append(ev.AckedKeys, key)
	}

	before := env.cp.Stats()
	ev.CheckpointsBeforeFailure = before.Checkpoints
	ev.WALTruncBeforeFailure = before.WALTruncBytes
	bytesBefore, err := env.walBytes()
	if err != nil {
		return err
	}
	ev.WALBytesBeforeFailure = bytesBefore

	if cfg.FaultOnCadenceFire {
		// The next Sync on this disk is the faulted fire's FIRST snapshot component
		// fsync: no committer is running and this loop is the only actor, so the
		// ordinal is deterministic. The publish then fails before wal.Writer.Sync
		// and long before the phase-3 prefix truncate, which is what makes the
		// failure transient rather than destructive.
		env.disk.ArmSyncFaultAt(1)
	}

	// One full window: the fire lands on its last tick.
	firesBefore := len(ev.FireTicks)
	for range cadenceWindowTicks {
		if serr := step(); serr != nil {
			return serr
		}
	}
	if len(ev.FireTicks) > firesBefore {
		faultedTick := ev.FireTicks[len(ev.FireTicks)-1]
		after := env.cp.Stats()
		ev.CheckpointsAfterFailure = after.Checkpoints
		ev.WALTruncAfterFailure = after.WALTruncBytes
		if cfg.FaultOnCadenceFire {
			ev.FailedFireTick = faultedTick
			ev.FailedFireErr = after.LastError
			ev.FailedFireIsSimFault = strings.Contains(after.LastError, ErrSimFault.Error())
		}
		bytesAfter, berr := env.walBytes()
		if berr != nil {
			return berr
		}
		ev.WALBytesAfterFailure = bytesAfter
	}

	for i := range cadenceDuringFailureCommits {
		key := fmt.Sprintf("during-failure-%04d", i)
		if cerr := env.commit(ctx, key); cerr != nil {
			return fmt.Errorf("sim: checkpoint-cadence during-failure commit: %w", cerr)
		}
		ev.AckedKeys = append(ev.AckedKeys, key)
		ev.AckedDuringFailure++
	}

	// The retry window. The loop assigns lastFire after a periodic fire whether it
	// succeeded or failed, so this measures how far away "the next cadence" really
	// is — the interval the package documentation does not state.
	firesBefore = len(ev.FireTicks)
	for range cadenceWindowTicks {
		if serr := step(); serr != nil {
			return serr
		}
	}
	if len(ev.FireTicks) > firesBefore && ev.FailedFireTick > 0 {
		ev.RetryFireTick = ev.FireTicks[len(ev.FireTicks)-1]
		ev.TicksFromFailureToRetry = ev.RetryFireTick - ev.FailedFireTick
	}
	ev.WALTruncAfterRetry = env.cp.Stats().WALTruncBytes
	return nil
}

// runCadenceTriggerPhase settles the age-timer question by CONSTRUCTION: it
// advances to one tick short of the next periodic fire, issues an explicit
// trigger there, and then keeps ticking across the ordinal at which the rival
// hypothesis — a trigger that does NOT reset the age — would have fired.
func runCadenceTriggerPhase(
	env *cpCadenceEnv,
	ev *CheckpointCadenceEvidence,
	model, rival *cadenceModel,
	step func() error,
) error {
	// Walk to one tick before the window closes, so the trigger lands inside a
	// window the periodic path has not yet reached.
	for range cadenceWindowTicks - 1 {
		if serr := step(); serr != nil {
			return serr
		}
	}

	ev.TriggerCalls++
	ev.TriggerTick = env.tick
	if terr := env.cp.Trigger(); terr != nil {
		return fmt.Errorf("sim: checkpoint-cadence explicit trigger: %w", terr)
	}
	ev.TriggeredFires++
	model.trigger()
	rival.trigger()
	env.sample()

	// The discriminating tick: under the rival hypothesis the age since the last
	// PERIODIC fire has now reached MaxAge and a checkpoint fires here.
	ev.UnresetFireTick = env.tick + 1
	firesBefore := len(ev.FireTicks)
	for range cadenceWindowTicks {
		if serr := step(); serr != nil {
			return serr
		}
		if env.tick == ev.UnresetFireTick && len(ev.FireTicks) > firesBefore {
			ev.FiredAtUnresetTick = true
		}
	}
	if len(ev.FireTicks) > firesBefore {
		ev.TicksFromTriggerToCadenceFire = ev.FireTicks[len(ev.FireTicks)-1] - ev.TriggerTick
	}
	return nil
}

// waitForCadence polls cond until it holds or [cadenceObserveTimeout] elapses.
// The timeout is REAL time and bounds only how long this run waits for the loop
// GOROUTINE to observe what the fake clock has already delivered; no simulated
// time passes here, so the cadence itself is never governed by it.
func waitForCadence(cond func() bool, what string) error {
	deadline := time.Now().Add(cadenceObserveTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			return fmt.Errorf("sim: checkpoint-cadence: %s (waited %s)", what, cadenceObserveTimeout)
		}
		time.Sleep(cadencePollInterval)
	}
	return nil
}

// seriesMonotonicity reports whether a lifetime counter's ordered samples ever
// DECREASED, and how many samples it strictly increased at. A lifetime counter
// that goes backwards is a defect in its own right, independent of what the
// cadence did.
func seriesMonotonicity(samples []uint64) (decreased bool, advances int) {
	for i := 1; i < len(samples); i++ {
		switch {
		case samples[i] < samples[i-1]:
			decreased = true
		case samples[i] > samples[i-1]:
			advances++
		}
	}
	return decreased, advances
}

// equalInts reports whether two tick-ordinal sequences are identical.
//
// It delegates to [slices.Equal], which has exactly the same contract — equal
// lengths and elementwise equality, with a nil and an empty slice equal — and is
// kept as a named helper because the ordinal comparisons read better at the call
// sites. The hand-written index loop it replaces was semantically identical but
// indexed b under a range over a, which gosec's G602 flags as an out-of-range
// index the moment a call site gives it slices of known length: it cannot see that
// the length guard above already made the two equal. That is a false positive, but
// suppressing it would have left the rule off for a real one.
func equalInts(a, b []int) bool { return slices.Equal(a, b) }

// -----------------------------------------------------------------------------
// Adjudication
// -----------------------------------------------------------------------------

// checkCheckpointCadenceNonVacuity is the SEPARATE coverage precondition, kept
// apart from [checkCheckpointCadence] for the reason rmp #2470 established: an
// uninformative run must not read as a faulty one. It asserts the run had the
// SHAPE a cadence adjudication needs — a ticker actually taken from the injected
// clock, fake time actually advanced, fires that the trigger cannot account for,
// a plan able to tell the two age-timer hypotheses apart, an injected fault that
// genuinely bit, and commits that recovery has to fetch from the WAL rather than
// from the snapshot.
//
// It is shape-only by design and says nothing about what the cadence did. A
// violation here means the RUN proved nothing, not that the checkpointer is
// broken.
func checkCheckpointCadenceNonVacuity(e *CheckpointCadenceEvidence) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{Kind: ViolationVacuousRun, Op: "<checkpoint-cadence non-vacuity>", Message: msg + " — " + e.String()})
	}
	if e.TickersRegistered != 1 {
		add(fmt.Sprintf("the loop registered %d ticker(s) on the injected clock; want exactly 1: without one the cadence is not on the seam this arm drives", e.TickersRegistered))
	}
	if e.SimulatedElapsedNS == 0 {
		add("the fake clock never advanced: every clause about the cadence is satisfied by a clock that stood still")
	}

	if e.Ticks == 0 {
		add("the run made no advance at all")
	}
	if e.TicksDelivered == 0 {
		add("the fake clock delivered no tick to any waiter: whatever the loop did or did not fold, it was never offered a cadence tick")
	}
	if e.TicksDelivered != e.Ticks {
		add(fmt.Sprintf("only %d of %d advance(s) delivered a tick: the ticker and the advance are out of step, so the fire ordinals cannot be read as MaxAge windows", e.TicksDelivered, e.Ticks))
	}
	if e.MaxAgeNS <= 0 && e.TriggeredFires == 0 {
		add("the MaxAge-less control never showed the loop was alive: a loop that had exited folds nothing just as convincingly as one that declined every tick")
	}
	if e.MaxAgeNS > 0 {
		if e.GateReadsOnTicks == 0 {
			add("the loop never evaluated the age gate on a tick: no tick reached it, so nothing about the cadence was exercised")
		}
		if e.CadenceFires == 0 {
			add("no checkpoint is attributable to the cadence (checkpoints minus explicit triggers is zero): a run in which the trigger did all the work proves nothing about the ticker/MaxAge path")
		}
		if len(e.FireTicks) < 2 {
			add(fmt.Sprintf("only %d fire(s) observed: one event cannot show a correspondence between simulated time and the cadence", len(e.FireTicks)))
		}
		if e.TriggerTick > 0 && equalInts(e.PredictedFireTicks, e.PredictedFireTicksNoReset) {
			add("the plan's two age-timer hypotheses predict the same fire ordinals, so this run cannot tell whether an explicit trigger resets the age timer")
		}
		if e.FaultArmed && e.FailedFireTick == 0 {
			add("the armed fsync fault never reached a periodic fire: LastError was never set, so the retry clauses are satisfied by a failure that did not happen")
		}
		if e.AckedDuringFailure == 0 && e.FaultArmed {
			add("nothing was acknowledged while the checkpointer was in its failed state: durability across the failure is satisfied by the empty set")
		}
	}
	if e.AckedAfterLastCheckpoint == 0 {
		add("no commit was acknowledged after the last checkpoint: the whole acked set was already folded into the snapshot, so recovering it says nothing about what the WAL carried")
	}
	if e.RecoveredWALOps == 0 {
		add("recovery replayed no WAL op: every acknowledged key came back from the snapshot, so the durability clause is answered whatever the cadence did to the WAL")
	}
	if len(e.AckedKeys) == 0 {
		add("no commit was acknowledged: acked-implies-recoverable is satisfied by the empty set")
	}
	return v
}

// checkCheckpointCadence adjudicates the cadence contract over one run. It is a
// PURE function of the observed evidence, which is what lets the sensitivity test
// falsify every clause by handing it a doctored value.
//
// The clauses, in the order a failure is most usefully read:
//
//   - the cadence is on the injected clock: a ticker was taken from it, and real
//     time alone fired nothing;
//   - the observed fire ordinals are the ones the documented rule predicts, and
//     not the ones the rival age-timer hypothesis predicts;
//   - an interval with no MaxAge fires nothing at all, however many ticks arrive;
//   - a FAILED periodic fire lands in LastError, advances neither lifetime
//     counter, and reclaims no WAL byte;
//   - the next cadence fire recovers: LastError clears, the checkpoint counter
//     advances, and the reclaimed-bytes counter advances;
//   - both lifetime counters are monotonic across the whole run;
//   - and, above everything, every acknowledged commit survived the crash and the
//     reopen — including the ones acknowledged while the checkpointer was failing.
func checkCheckpointCadence(e *CheckpointCadenceEvidence) []Violation {
	var v []Violation
	add := func(kind ViolationKind, msg string) {
		v = append(v, Violation{Kind: kind, Op: "<checkpoint-cadence>", Message: msg + " — " + e.String()})
	}

	if e.TickersRegistered != 1 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the cadence loop registered %d ticker(s) on the injected clock; exactly 1 is the WithClock seam being used — a loop reading wall time registers none",
			e.TickersRegistered))
	}
	if e.FrozenWindowFires != 0 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"%d checkpoint(s) fired while the fake clock was held still for %s of real time: the cadence is reading a clock this run does not control, so no fire below is attributable to it",
			e.FrozenWindowFires, time.Duration(e.FrozenWindowNS)))
	}
	if e.FrozenWindowSinces != 0 {
		add(ViolationOracleDeviation, fmt.Sprintf(
			"the loop made %d clock observation(s) with the fake clock frozen: a tick was delivered by something other than the injected clock",
			e.FrozenWindowSinces))
	}

	if e.MaxAgeNS <= 0 {
		// The interval-only control: the ticker exists and ticks arrive, and the
		// loop must fold nothing it was not explicitly asked for.
		if e.CadenceFires != 0 {
			add(ViolationOracleDeviation, fmt.Sprintf(
				"%d checkpoint(s) not asked for by this run fired with MaxAge unset (%d checkpoint(s), %d explicit trigger(s)): the cadence body is gated on MaxAge > 0, so an interval alone must tick forever and fold nothing",
				e.CadenceFires, e.Checkpoints, e.TriggeredFires))
		}
		if len(e.FireTicks) != 0 {
			add(ViolationOracleDeviation, fmt.Sprintf(
				"the loop ran a checkpoint at tick(s) %v with MaxAge unset", e.FireTicks))
		}
	} else {
		if !equalInts(e.FireTicks, e.PredictedFireTicks) {
			add(ViolationOracleDeviation, fmt.Sprintf(
				"the cadence fired at ticks %v; the MaxAge=%s / Interval=%s rule predicts %v — the loop is not firing on the age gate this run modelled",
				e.FireTicks, time.Duration(e.MaxAgeNS), time.Duration(e.IntervalNS), e.PredictedFireTicks))
		}
		if e.TriggerTick > 0 && e.FiredAtUnresetTick {
			add(ViolationOracleDeviation, fmt.Sprintf(
				"a periodic checkpoint fired at tick %d, one MaxAge after the last PERIODIC fire: an explicit Trigger did not postpone the cadence, although the loop assigns lastFire on the trigger arm too",
				e.UnresetFireTick))
		}
	}

	if e.FailedFireTick > 0 {
		if e.FailedFireErr == "" {
			add(ViolationOracleDeviation,
				"a periodic fire failed and Stats.LastError was empty: the loop discards the error, so LastError is the only place a periodic failure can surface")
		}
		if !e.FailedFireIsSimFault {
			add(ViolationOracleDeviation, fmt.Sprintf(
				"Stats.LastError holds %q, which does not carry the injected fsync fault: the failure came from somewhere other than the publish this arm faulted",
				e.FailedFireErr))
		}
		if e.CheckpointsAfterFailure != e.CheckpointsBeforeFailure {
			add(ViolationOracleDeviation, fmt.Sprintf(
				"the checkpoint counter moved %d -> %d across a fire that FAILED: a checkpoint that published no snapshot must not be counted",
				e.CheckpointsBeforeFailure, e.CheckpointsAfterFailure))
		}
		if e.WALTruncAfterFailure != e.WALTruncBeforeFailure {
			add(ViolationACIDDurability, fmt.Sprintf(
				"the reclaimed-bytes counter moved %d -> %d across a fire that FAILED: the publish failed before wal.Writer.Sync, so no WAL prefix can have been reclaimed",
				e.WALTruncBeforeFailure, e.WALTruncAfterFailure))
		}
		if e.WALBytesAfterFailure < e.WALBytesBeforeFailure {
			add(ViolationACIDDurability, fmt.Sprintf(
				"the durable WAL image shrank from %d to %d bytes across a fire that FAILED: a checkpoint that never published a snapshot truncated the WAL that still holds those commits",
				e.WALBytesBeforeFailure, e.WALBytesAfterFailure))
		}
		if e.RetryFireTick == 0 {
			add(ViolationOracleDeviation,
				"no cadence fire followed the failed one: the loop must keep firing on its cadence after a failure, not stop at the first error")
		} else {
			if e.LastErrorAfterRetry != "" {
				add(ViolationOracleDeviation, fmt.Sprintf(
					"Stats.LastError still holds %q after a later checkpoint succeeded: a success records its own outcome, so the stale failure must be cleared",
					e.LastErrorAfterRetry))
			}
			if e.CheckpointsAfterFailure >= e.Checkpoints {
				add(ViolationOracleDeviation, fmt.Sprintf(
					"the checkpoint counter did not advance past %d after the retry: the fire that followed the failure did not succeed",
					e.CheckpointsAfterFailure))
			}
			if e.WALTruncAfterRetry <= e.WALTruncAfterFailure {
				add(ViolationOracleDeviation, fmt.Sprintf(
					"the reclaimed-bytes counter did not advance past %d after the retry (%d): the recovering checkpoint folded a WAL prefix and reclaimed nothing",
					e.WALTruncAfterFailure, e.WALTruncAfterRetry))
			}
		}
	}

	if e.CheckpointsNonMonotonic {
		add(ViolationOracleDeviation,
			"the checkpoint counter DECREASED between two samples: Stats is documented as a monotonic lifetime counter")
	}
	if e.WALTruncNonMonotonic {
		add(ViolationOracleDeviation,
			"the reclaimed-bytes counter DECREASED between two samples: Stats is documented as a monotonic lifetime counter")
	}

	if len(e.MissingAckedKeys) > 0 {
		add(ViolationACIDDurability, fmt.Sprintf(
			"%d acknowledged commit(s) did not survive the crash and reopen (%v): a commit acknowledged before a checkpoint — succeeding, failing, or never running — is durable whatever the cadence then did",
			len(e.MissingAckedKeys), e.MissingAckedKeys))
	}
	if !e.ReopenClean {
		add(ViolationACIDDurability,
			"the reopen's recovery reported genuine corruption: the cadence left an image recovery cannot trust")
	}
	return v
}
