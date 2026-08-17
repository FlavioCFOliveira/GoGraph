package sim

// checkpoint_cadence_test.go — the wiring and the falsifiability proofs for the
// background checkpointer's cadence oracles (rmp #2476).
//
// Every gate here is proved falsifiable as well as satisfied, and two of the
// proofs drive real behaviour rather than a doctored value: the interval-only
// arm shows ticks arriving at a live loop that folds nothing (so the other arms'
// fires are attributable to the MaxAge gate and not to the ticker), and the
// transient-fault arm produces a genuine failed periodic fire whose recovery is
// what the retry clauses adjudicate.
//
// None of these tests calls t.Parallel: each owns a checkpoint goroutine driven
// by a fake clock and asserts on real-time observation windows, so running them
// beside each other would only make those windows noisier for no gain.

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// runCheckpointCadenceArm drives one arm and reports the evidence, failing the
// test on a harness error and running the non-vacuity gate before any verdict is
// read.
func runCheckpointCadenceArm(t *testing.T, cfg CheckpointCadenceConfig) *CheckpointCadenceEvidence {
	t.Helper()
	ev, err := RunCheckpointCadence(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunCheckpointCadence(%s): %v\n%s", cfg.Arm, err, ev.String())
	}
	t.Log(ev.String())
	if v := checkCheckpointCadenceNonVacuity(&ev); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("non-vacuity: %s", viol)
		}
		t.Fatal("the run proved nothing about the cadence; the verdict below would be meaningless")
	}
	return &ev
}

// TestCheckpointCadence_FakeClockDrivesThePeriodicFires is the clean arm: the
// ticker/MaxAge path driven entirely on the simulation's fake clock, with the
// explicit-trigger phase that settles whether a Trigger postpones the next
// periodic fire.
//
// The fires are attributed to the CADENCE three ways and none of them is an
// argument from absence: they are the checkpoints the run did not ask for, they
// land on exactly the tick ordinals an independent model of the rule predicts,
// and holding the fake clock still for a real-time window fires nothing at all.
func TestCheckpointCadence_FakeClockDrivesThePeriodicFires(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev := runCheckpointCadenceArm(t, CheckpointCadenceConfig{
		Seed: 0x2476_C1EA_11,
		Arm:  ArmCheckpointCadenceClean,
	})

	t.Logf("witness: %s of SIMULATED time advanced in %s of real time; the loop took %d ticker(s) from the injected clock, made %d Now / %d Since call(s) on it (%d of them while servicing a tick), and was offered %d of %d advances as ticks",
		time.Duration(ev.SimulatedElapsedNS), time.Duration(ev.RealElapsedNS),
		ev.TickersRegistered, ev.ClockNowCalls, ev.ClockSinceCalls, ev.GateReadsOnTicks,
		ev.TicksDelivered, ev.Ticks)
	t.Logf("witness: %d checkpoint(s), of which %d from the cadence and %d from the %d explicit trigger(s); fires at ticks %v",
		ev.Checkpoints, ev.CadenceFires, ev.TriggeredFires, ev.TriggerCalls, ev.FireTicks)
	t.Logf("witness: an explicit Trigger reset the age timer = %t (a trigger at tick %d, no fire at tick %d where the un-reset age would have fired, next cadence fire %d tick(s) later)",
		ev.TriggerResetTheAgeTimer(), ev.TriggerTick, ev.UnresetFireTick, ev.TicksFromTriggerToCadenceFire)

	if !equalInts(ev.FireTicks, ev.PredictedFireTicks) {
		t.Errorf("the cadence fired at %v; the MaxAge/Interval rule predicts %v", ev.FireTicks, ev.PredictedFireTicks)
	}
	if ev.FiredAtUnresetTick {
		t.Errorf("a periodic checkpoint fired at tick %d, one MaxAge after the last PERIODIC fire: the explicit Trigger did not reset the age timer",
			ev.UnresetFireTick)
	}
	if ev.TicksFromTriggerToCadenceFire != cadenceWindowTicks {
		t.Errorf("the cadence fire after the explicit trigger came %d tick(s) later; want a full MaxAge window (%d)",
			ev.TicksFromTriggerToCadenceFire, cadenceWindowTicks)
	}
	if ev.WALTruncAdvances == 0 {
		t.Error("the reclaimed-bytes counter never advanced across the whole run: no cadence fire reclaimed a WAL prefix")
	}
	for _, viol := range checkCheckpointCadence(ev) {
		t.Errorf("%s", viol)
	}
}

// TestCheckpointCadence_TransientFailureIsRecordedAndRetried is the arm the task
// exists for: a one-shot fsync fault fails ONE periodic fire inside its snapshot
// publish, and the run measures what the loop then does.
//
// The failed fire has no caller to return to, so [checkpoint.Stats.LastError] is
// the only place it can surface; the checkpoint counter and the reclaimed-bytes
// counter must both stand still across it, and the durable WAL image must not
// shrink — a publish that failed before wal.Writer.Sync cannot have reclaimed a
// prefix. The NEXT cadence fire must then succeed and clear the stale error.
//
// The retry DISTANCE is measured rather than assumed, and it is the behaviour the
// package documentation leaves unstated: the loop assigns lastFire after a
// periodic fire whether it succeeded or failed, so "retries on the next cadence"
// means a full MaxAge window and not the next Interval tick.
func TestCheckpointCadence_TransientFailureIsRecordedAndRetried(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev := runCheckpointCadenceArm(t, CheckpointCadenceConfig{
		Seed:               0x2476_FA01_7ED,
		Arm:                ArmCheckpointCadenceTransientFault,
		FaultOnCadenceFire: true,
	})

	t.Logf("witness: the faulted periodic fire at tick %d recorded LastError=%q; the retry ran at tick %d, %d tick(s) later (one full MaxAge window = %d ticks: %t)",
		ev.FailedFireTick, ev.FailedFireErr, ev.RetryFireTick, ev.TicksFromFailureToRetry,
		cadenceWindowTicks, ev.RetryWaitedAFullWindow())
	t.Logf("witness: across the failure checkpoints %d->%d, reclaimed bytes %d->%d, durable WAL %d->%d bytes; after the retry reclaimed bytes %d and LastError=%q",
		ev.CheckpointsBeforeFailure, ev.CheckpointsAfterFailure,
		ev.WALTruncBeforeFailure, ev.WALTruncAfterFailure,
		ev.WALBytesBeforeFailure, ev.WALBytesAfterFailure,
		ev.WALTruncAfterRetry, ev.LastErrorAfterRetry)
	t.Logf("witness: %d commit(s) were acknowledged while the checkpointer was in its failed state; %d acknowledged commit(s) in all, %d missing after the crash and reopen (recovery replayed %d WAL op(s))",
		ev.AckedDuringFailure, len(ev.AckedKeys), len(ev.MissingAckedKeys), ev.RecoveredWALOps)

	if !ev.FailedFireIsSimFault {
		t.Errorf("LastError after the faulted fire was %q; want the injected fsync fault", ev.FailedFireErr)
	}
	if !ev.RetryWaitedAFullWindow() {
		t.Errorf("the retry came %d tick(s) after the failure; the loop resets lastFire on a FAILED fire too, so a full MaxAge window (%d ticks) is what the code does",
			ev.TicksFromFailureToRetry, cadenceWindowTicks)
	}
	if ev.LastErrorAfterRetry != "" {
		t.Errorf("LastError still holds %q after the retry succeeded", ev.LastErrorAfterRetry)
	}
	if ev.WALTruncAfterRetry <= ev.WALTruncBeforeFailure {
		t.Errorf("the reclaimed-bytes counter did not advance across the failure and its retry (%d -> %d): the recovering checkpoint reclaimed nothing",
			ev.WALTruncBeforeFailure, ev.WALTruncAfterRetry)
	}
	for _, viol := range checkCheckpointCadence(ev) {
		t.Errorf("%s", viol)
	}
}

// TestCheckpointCadence_IntervalWithoutMaxAgeFiresNothing is the CONTROL, and it
// is a real behaviour rather than a doctored one: the loop creates its ticker
// whenever Interval > 0 but gates the body on MaxAge > 0, so an interval with no
// age ticks forever and folds nothing.
//
// It is what attributes the other arms' fires to the AGE GATE rather than to the
// arrival of a tick, so it asserts positively that the ticks DID arrive and the
// gate WAS evaluated — otherwise "nothing fired" would be satisfied by a loop
// that received nothing.
func TestCheckpointCadence_IntervalWithoutMaxAgeFiresNothing(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev := runCheckpointCadenceArm(t, CheckpointCadenceConfig{
		Seed:             0x2476_C7_11_0,
		Arm:              ArmCheckpointCadenceIntervalOnly,
		MaxAge:           -1, // MaxAge: 0, spelled so it is not the "unset" zero
		SkipTriggerPhase: true,
	})

	t.Logf("witness: %d of %d advance(s) delivered a tick over %s of simulated time (%d MaxAge windows' worth for the other arms); the loop read the clock %d time(s) while servicing them and folded %d cadence checkpoint(s), then served %d explicit trigger(s)",
		ev.TicksDelivered, ev.Ticks, time.Duration(ev.SimulatedElapsedNS),
		int(time.Duration(ev.SimulatedElapsedNS)/cadenceMaxAge), ev.GateReadsOnTicks, ev.CadenceFires, ev.TriggeredFires)

	if ev.CadenceFires != 0 {
		t.Errorf("%d checkpoint(s) this run did not ask for fired with MaxAge unset; the cadence body is gated on MaxAge > 0", ev.CadenceFires)
	}
	if ev.TicksDelivered != ev.Ticks {
		t.Errorf("only %d of %d advance(s) delivered a tick: 'nothing fired' would then say nothing about the gate",
			ev.TicksDelivered, ev.Ticks)
	}
	if ev.TriggeredFires != 1 {
		t.Errorf("the liveness trigger folded %d checkpoint(s); want 1 — without it a loop that had EXITED would satisfy this arm just as well",
			ev.TriggeredFires)
	}
	// The measured short-circuit: `MaxAge > 0 && Since(lastFire) >= MaxAge` never
	// reaches Since when MaxAge is unset, so a MaxAge-less loop reads no clock at
	// all on a tick. It is pinned here because the first version of this arm used a
	// clock observation as its tick barrier and hung on it.
	if ev.GateReadsOnTicks != 0 {
		t.Errorf("the loop made %d clock observation(s) while servicing a tick with MaxAge unset; the age gate short-circuits before reading the clock", ev.GateReadsOnTicks)
	}
	if ev.TickersRegistered != 1 {
		t.Errorf("the loop registered %d ticker(s) with MaxAge unset; Interval > 0 alone must still create one", ev.TickersRegistered)
	}
	for _, viol := range checkCheckpointCadence(ev) {
		t.Errorf("%s", viol)
	}
}

// TestCheckpointCadenceModel_PredictsTheDocumentedRule pins the harness's OWN
// model of the cadence rule, so a comparison against it is a real check. A model
// that predicted nothing, or predicted a fire on every tick, would make the
// correspondence clause in the verdict vacuous while looking identical from the
// outside.
func TestCheckpointCadenceModel_PredictsTheDocumentedRule(t *testing.T) {
	fires := func(m *cadenceModel, ticks int, triggerAt int) []int {
		var out []int
		for i := 1; i <= ticks; i++ {
			if m.tick() {
				out = append(out, i)
			}
			if i == triggerAt {
				m.trigger()
			}
		}
		return out
	}

	// The first tick always fires (lastFire is the zero time and time.Time.Sub
	// saturates), then one fire per MaxAge window.
	plain := fires(&cadenceModel{interval: cadenceInterval, maxAge: cadenceMaxAge}, 9, 0)
	if want := []int{1, 5, 9}; !equalInts(plain, want) {
		t.Errorf("the plain cadence model predicts %v; want %v", plain, want)
	}

	// MaxAge unset: the ticker still ticks and nothing ever fires.
	none := fires(&cadenceModel{interval: cadenceInterval, maxAge: 0}, 12, 0)
	if len(none) != 0 {
		t.Errorf("the MaxAge-less model predicts fires at %v; an interval alone must fold nothing", none)
	}

	// The two age-timer hypotheses must actually disagree over the plan's shape,
	// or the arm cannot settle the question.
	withReset := fires(&cadenceModel{interval: cadenceInterval, maxAge: cadenceMaxAge, triggerResets: true}, 8, 4)
	without := fires(&cadenceModel{interval: cadenceInterval, maxAge: cadenceMaxAge, triggerResets: false}, 8, 4)
	if equalInts(withReset, without) {
		t.Errorf("the two age-timer hypotheses predict the same schedule (%v): the model cannot discriminate them", withReset)
	}
	if want := []int{1, 8}; !equalInts(withReset, want) {
		t.Errorf("the trigger-resets model predicts %v; want %v (a trigger at tick 4 postpones the next fire by a full window)", withReset, want)
	}
	if want := []int{1, 5}; !equalInts(without, want) {
		t.Errorf("the trigger-does-not-reset model predicts %v; want %v", without, want)
	}
}

// healthyCheckpointCadenceEvidence is the shape of a clean transient-fault run,
// taken from the arm's own contract, so each sensitivity case below perturbs
// exactly one fact.
func healthyCheckpointCadenceEvidence() CheckpointCadenceEvidence {
	return CheckpointCadenceEvidence{
		Arm:                       ArmCheckpointCadenceTransientFault,
		MaxAgeNS:                  cadenceMaxAge.Nanoseconds(),
		IntervalNS:                cadenceInterval.Nanoseconds(),
		TickersRegistered:         1,
		ClockNowCalls:             12,
		ClockSinceCalls:           31,
		GateReadsOnTicks:          30,
		SimulatedElapsedNS:        (24 * cadenceInterval).Nanoseconds(),
		RealElapsedNS:             (2 * time.Second).Nanoseconds(),
		FrozenWindowNS:            cadenceFrozenWindow.Nanoseconds(),
		Ticks:                     24,
		TicksDelivered:            24,
		TriggerCalls:              1,
		TriggeredFires:            1,
		Checkpoints:               6,
		CadenceFires:              5,
		FireTicks:                 []int{1, 5, 9, 13, 17, 24},
		PredictedFireTicks:        []int{1, 5, 9, 13, 17, 24},
		PredictedFireTicksNoReset: []int{1, 5, 9, 13, 17, 21},
		TriggerTick:               20,
		UnresetFireTick:           21,

		TicksFromTriggerToCadenceFire: 4,
		FaultArmed:                    true,
		FailedFireTick:                13,
		FailedFireErr:                 "sim: injected disk fault",
		FailedFireIsSimFault:          true,
		TicksFromFailureToRetry:       4,
		RetryFireTick:                 17,
		CheckpointsBeforeFailure:      3,
		CheckpointsAfterFailure:       3,
		WALTruncBeforeFailure:         512,
		WALTruncAfterFailure:          512,
		WALTruncAfterRetry:            980,
		WALBytesBeforeFailure:         704,
		WALBytesAfterFailure:          704,
		WALTruncTotal:                 980,
		WALTruncAdvances:              3,
		AckedKeys:                     []string{"prefix-0000", "tail-0000"},
		AckedDuringFailure:            cadenceDuringFailureCommits,
		AckedAfterLastCheckpoint:      cadenceTailCommits,
		RecoveredWALOps:               2 * cadenceTailCommits,
		WALBytes:                      320,
		SnapshotPublished:             true,
		ReopenClean:                   true,
	}
}

// healthyIntervalOnlyEvidence is the shape of a clean interval-only control run:
// ticks delivered, the gate evaluated, nothing folded.
func healthyIntervalOnlyEvidence() CheckpointCadenceEvidence {
	return CheckpointCadenceEvidence{
		Arm:                      ArmCheckpointCadenceIntervalOnly,
		MaxAgeNS:                 0,
		IntervalNS:               cadenceInterval.Nanoseconds(),
		TickersRegistered:        1,
		ClockSinceCalls:          1,
		SimulatedElapsedNS:       (21 * cadenceInterval).Nanoseconds(),
		FrozenWindowNS:           cadenceFrozenWindow.Nanoseconds(),
		Ticks:                    21,
		TicksDelivered:           21,
		TriggerCalls:             1,
		TriggeredFires:           1,
		Checkpoints:              1,
		AckedKeys:                []string{"prefix-0000", "tail-0000"},
		AckedAfterLastCheckpoint: cadenceTailCommits,
		RecoveredWALOps:          2 * cadenceTailCommits,
		WALBytes:                 900,
		ReopenClean:              true,
	}
}

// TestCheckpointCadence_ClausesFire proves every verdict clause is falsifiable.
// The adjudicator is a pure function of the evidence, so each failure it exists
// to catch is injected directly rather than hoped for.
func TestCheckpointCadence_ClausesFire(t *testing.T) {
	healthy := healthyCheckpointCadenceEvidence()
	if v := checkCheckpointCadence(&healthy); len(v) != 0 {
		t.Fatalf("the adjudicator rejected a healthy run, so its clauses do not describe the contract:\n%v", v)
	}
	control := healthyIntervalOnlyEvidence()
	if v := checkCheckpointCadence(&control); len(v) != 0 {
		t.Fatalf("the adjudicator rejected a healthy interval-only control:\n%v", v)
	}

	cases := []struct {
		name   string
		base   func() CheckpointCadenceEvidence
		mutate func(*CheckpointCadenceEvidence)
		kind   ViolationKind
		want   string
	}{
		{
			name:   "the loop took no ticker from the injected clock",
			mutate: func(e *CheckpointCadenceEvidence) { e.TickersRegistered = 0 },
			kind:   ViolationOracleDeviation,
			want:   "WithClock seam being used",
		},
		{
			name:   "a checkpoint fired with the fake clock frozen",
			mutate: func(e *CheckpointCadenceEvidence) { e.FrozenWindowFires = 1 },
			kind:   ViolationOracleDeviation,
			want:   "held still",
		},
		{
			name:   "the loop read a clock this run does not control",
			mutate: func(e *CheckpointCadenceEvidence) { e.FrozenWindowSinces = 3 },
			kind:   ViolationOracleDeviation,
			want:   "clock observation(s) with the fake clock frozen",
		},
		{
			name:   "the fires did not land where the age rule predicts",
			mutate: func(e *CheckpointCadenceEvidence) { e.FireTicks = []int{1, 2, 3, 4, 5, 6} },
			kind:   ViolationOracleDeviation,
			want:   "not firing on the age gate",
		},
		{
			name:   "an explicit trigger did not postpone the cadence",
			mutate: func(e *CheckpointCadenceEvidence) { e.FiredAtUnresetTick = true },
			kind:   ViolationOracleDeviation,
			want:   "did not postpone the cadence",
		},
		{
			name:   "the failed fire recorded no error",
			mutate: func(e *CheckpointCadenceEvidence) { e.FailedFireErr = "" },
			kind:   ViolationOracleDeviation,
			want:   "LastError is the only place",
		},
		{
			name: "the failure was not the injected one",
			mutate: func(e *CheckpointCadenceEvidence) {
				e.FailedFireIsSimFault = false
				e.FailedFireErr = "something else"
			},
			kind: ViolationOracleDeviation,
			want: "does not carry the injected fsync fault",
		},
		{
			name:   "a failed fire was counted as a checkpoint",
			mutate: func(e *CheckpointCadenceEvidence) { e.CheckpointsAfterFailure = 4 },
			kind:   ViolationOracleDeviation,
			want:   "across a fire that FAILED",
		},
		{
			name:   "a failed fire reclaimed WAL bytes",
			mutate: func(e *CheckpointCadenceEvidence) { e.WALTruncAfterFailure = 600 },
			kind:   ViolationACIDDurability,
			want:   "reclaimed-bytes counter moved",
		},
		{
			name:   "a failed fire truncated the WAL anyway",
			mutate: func(e *CheckpointCadenceEvidence) { e.WALBytesAfterFailure = 128 },
			kind:   ViolationACIDDurability,
			want:   "durable WAL image shrank",
		},
		{
			name:   "the loop stopped firing after the failure",
			mutate: func(e *CheckpointCadenceEvidence) { e.RetryFireTick = 0 },
			kind:   ViolationOracleDeviation,
			want:   "no cadence fire followed the failed one",
		},
		{
			name:   "a stale failure survived a later success",
			mutate: func(e *CheckpointCadenceEvidence) { e.LastErrorAfterRetry = "sim: injected disk fault" },
			kind:   ViolationOracleDeviation,
			want:   "after a later checkpoint succeeded",
		},
		{
			name:   "the retry did not succeed",
			mutate: func(e *CheckpointCadenceEvidence) { e.Checkpoints = 3 },
			kind:   ViolationOracleDeviation,
			want:   "did not advance past",
		},
		{
			name:   "the retry reclaimed nothing",
			mutate: func(e *CheckpointCadenceEvidence) { e.WALTruncAfterRetry = 400 },
			kind:   ViolationOracleDeviation,
			want:   "folded a WAL prefix and reclaimed nothing",
		},
		{
			name:   "the checkpoint counter went backwards",
			mutate: func(e *CheckpointCadenceEvidence) { e.CheckpointsNonMonotonic = true },
			kind:   ViolationOracleDeviation,
			want:   "checkpoint counter DECREASED",
		},
		{
			name:   "the reclaimed-bytes counter went backwards",
			mutate: func(e *CheckpointCadenceEvidence) { e.WALTruncNonMonotonic = true },
			kind:   ViolationOracleDeviation,
			want:   "reclaimed-bytes counter DECREASED",
		},
		{
			name:   "an acknowledged commit was lost",
			mutate: func(e *CheckpointCadenceEvidence) { e.MissingAckedKeys = []string{"during-failure-0001"} },
			kind:   ViolationACIDDurability,
			want:   "did not survive the crash and reopen",
		},
		{
			name:   "the reopen found corruption",
			mutate: func(e *CheckpointCadenceEvidence) { e.ReopenClean = false },
			kind:   ViolationACIDDurability,
			want:   "genuine corruption",
		},
		{
			name:   "an interval with no MaxAge folded a checkpoint nobody asked for",
			base:   healthyIntervalOnlyEvidence,
			mutate: func(e *CheckpointCadenceEvidence) { e.CadenceFires = 2 },
			kind:   ViolationOracleDeviation,
			want:   "gated on MaxAge > 0",
		},
		{
			name:   "an interval with no MaxAge ran a checkpoint at a tick",
			base:   healthyIntervalOnlyEvidence,
			mutate: func(e *CheckpointCadenceEvidence) { e.FireTicks = []int{7} },
			kind:   ViolationOracleDeviation,
			want:   "with MaxAge unset",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := healthyCheckpointCadenceEvidence
			if tc.base != nil {
				base = tc.base
			}
			broken := base()
			tc.mutate(&broken)
			v := checkCheckpointCadence(&broken)
			if len(v) == 0 {
				t.Fatalf("%q passed the adjudicator: the clause cannot fail and proves nothing", tc.name)
			}
			if !hasViolationKind(v, tc.kind) {
				t.Errorf("%q was not reported as %s:\n%v", tc.name, tc.kind, v)
			}
			found := false
			for _, viol := range v {
				if strings.Contains(viol.Message, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("no violation explains %q (want a message containing %q):\n%v", tc.name, tc.want, v)
			}
		})
	}
}

// TestCheckpointCadenceNonVacuity_ClausesFire proves the coverage preconditions
// are independent of the verdict (rmp #2470): a run that proved nothing must be
// reported by the NON-VACUITY gate and must not be dressed up as a broken
// checkpointer by the verdict gate.
//
// The cases are the traps this sprint keeps finding — a clause satisfied by
// definition. A clock that never advanced satisfies every statement about the
// cadence; a run whose fires all came from its own triggers satisfies "the
// cadence fired" without exercising the ticker at all; an armed fault that never
// bit satisfies every retry clause over a failure that did not happen; and a
// plan whose two age-timer hypotheses predict the same schedule cannot settle the
// question it claims to settle.
func TestCheckpointCadenceNonVacuity_ClausesFire(t *testing.T) {
	healthy := healthyCheckpointCadenceEvidence()
	if v := checkCheckpointCadenceNonVacuity(&healthy); len(v) != 0 {
		t.Fatalf("the non-vacuity gate fired on an ample run:\n%v", v)
	}
	control := healthyIntervalOnlyEvidence()
	if v := checkCheckpointCadenceNonVacuity(&control); len(v) != 0 {
		t.Fatalf("the non-vacuity gate fired on an ample interval-only control:\n%v", v)
	}

	cases := []struct {
		name   string
		mutate func(*CheckpointCadenceEvidence)
		want   string
	}{
		{
			name:   "the loop never took a ticker from the injected clock",
			mutate: func(e *CheckpointCadenceEvidence) { e.TickersRegistered = 0 },
			want:   "not on the seam",
		},
		{
			name:   "the fake clock never advanced",
			mutate: func(e *CheckpointCadenceEvidence) { e.SimulatedElapsedNS = 0 },
			want:   "clock that stood still",
		},
		{
			name:   "no tick ever reached the age gate",
			mutate: func(e *CheckpointCadenceEvidence) { e.GateReadsOnTicks = 0 },
			want:   "never evaluated the age gate on a tick",
		},
		{
			name: "the run made no advance",
			mutate: func(e *CheckpointCadenceEvidence) {
				e.Ticks = 0
				e.TicksDelivered = 0
			},
			want: "no advance at all",
		},
		{
			name:   "the advances delivered no tick",
			mutate: func(e *CheckpointCadenceEvidence) { e.TicksDelivered = 0 },
			want:   "never offered a cadence tick",
		},
		{
			name:   "the ticker and the advance drifted apart",
			mutate: func(e *CheckpointCadenceEvidence) { e.TicksDelivered = 19 },
			want:   "out of step",
		},
		{
			name: "the trigger did all the work",
			mutate: func(e *CheckpointCadenceEvidence) {
				e.CadenceFires = 0
				e.TriggeredFires = 6
			},
			want: "the trigger did all the work",
		},
		{
			name: "a single fire is not a correspondence",
			mutate: func(e *CheckpointCadenceEvidence) {
				e.FireTicks = []int{1}
				e.PredictedFireTicks = []int{1}
			},
			want: "cannot show a correspondence",
		},
		{
			name: "the plan cannot tell the age-timer hypotheses apart",
			mutate: func(e *CheckpointCadenceEvidence) {
				e.PredictedFireTicksNoReset = []int{1, 5, 9, 13, 17, 24}
			},
			want: "predict the same fire ordinals",
		},
		{
			name:   "the armed fault never bit",
			mutate: func(e *CheckpointCadenceEvidence) { e.FailedFireTick = 0 },
			want:   "a failure that did not happen",
		},
		{
			name:   "nothing was acknowledged while the checkpointer was failing",
			mutate: func(e *CheckpointCadenceEvidence) { e.AckedDuringFailure = 0 },
			want:   "satisfied by the empty set",
		},
		{
			name:   "every acked key was already in the snapshot",
			mutate: func(e *CheckpointCadenceEvidence) { e.AckedAfterLastCheckpoint = 0 },
			want:   "already folded into the snapshot",
		},
		{
			name:   "recovery replayed nothing",
			mutate: func(e *CheckpointCadenceEvidence) { e.RecoveredWALOps = 0 },
			want:   "replayed no WAL op",
		},
		{
			name:   "nothing was acknowledged at all",
			mutate: func(e *CheckpointCadenceEvidence) { e.AckedKeys = nil },
			want:   "acked-implies-recoverable is satisfied by the empty set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broken := healthyCheckpointCadenceEvidence()
			tc.mutate(&broken)
			v := checkCheckpointCadenceNonVacuity(&broken)
			if !violationsMention(v, tc.want) {
				t.Errorf("%q passed the non-vacuity gate (want a message containing %q):\n%v", tc.name, tc.want, v)
			}
		})
	}

	// The control arm's own floor: with MaxAge unset the loop never touches the
	// clock, so "it folded nothing" is equally true of a loop that had exited. The
	// liveness trigger is what tells them apart, and its absence must be reported.
	dead := healthyIntervalOnlyEvidence()
	dead.TriggeredFires = 0
	dead.Checkpoints = 0
	if !violationsMention(checkCheckpointCadenceNonVacuity(&dead), "never showed the loop was alive") {
		t.Errorf("an interval-only run that never proved its loop was alive passed the floor:\n%v",
			checkCheckpointCadenceNonVacuity(&dead))
	}

	// The separation that matters: a merely-uninformative run is NOT reported as
	// a faulty checkpointer. A clock that never moved, no fire at all, nothing
	// acknowledged — and a verdict gate that says nothing, because none of it is
	// a breach of the cadence contract.
	vacuous := CheckpointCadenceEvidence{
		Arm:               ArmCheckpointCadenceClean,
		MaxAgeNS:          cadenceMaxAge.Nanoseconds(),
		IntervalNS:        cadenceInterval.Nanoseconds(),
		TickersRegistered: 1,
		FrozenWindowNS:    cadenceFrozenWindow.Nanoseconds(),
		ReopenClean:       true,
	}
	nv := checkCheckpointCadenceNonVacuity(&vacuous)
	if len(nv) == 0 {
		t.Fatal("the non-vacuity gate passed a run that advanced nothing, fired nothing and acknowledged nothing")
	}
	for _, want := range []string{"clock that stood still", "never evaluated the age gate on a tick", "no advance at all",
		"never offered a cadence tick", "the trigger did all the work", "cannot show a correspondence",
		"already folded into the snapshot", "replayed no WAL op",
		"acked-implies-recoverable is satisfied by the empty set"} {
		if !violationsMention(nv, want) {
			t.Errorf("no non-vacuity violation mentions %q:\n%v", want, nv)
		}
	}
	if cv := checkCheckpointCadence(&vacuous); len(cv) != 0 {
		t.Errorf("the verdict gate also fired on a merely-uninformative run, so an uninformative run reads as a faulty one:\n%v", cv)
	}

	// The converse: an ample population whose cadence is broken is NOT a
	// non-vacuity problem — it is exactly what the verdict gate owns.
	broken := healthyCheckpointCadenceEvidence()
	broken.MissingAckedKeys = []string{"during-failure-0000"}
	if nvb := checkCheckpointCadenceNonVacuity(&broken); len(nvb) != 0 {
		t.Errorf("non-vacuity fired on an ample run whose only problem is a lost commit:\n%v", nvb)
	}
	if cv := checkCheckpointCadence(&broken); !hasViolationKind(cv, ViolationACIDDurability) {
		t.Errorf("a lost acknowledged commit was not reported as a DURABILITY violation:\n%v", cv)
	}
}
