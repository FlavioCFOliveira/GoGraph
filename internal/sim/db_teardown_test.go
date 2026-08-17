package sim

// db_teardown_test.go — the wiring and the falsifiability proofs for the
// composed-teardown variant oracles (rmp #2475).
//
// None of these tests calls t.Parallel: [RunDBTeardown] installs the GLOBAL
// metrics sink (as the group-commit oracle does), so they must not run beside
// other metrics-emitting work.
//
// Every gate here is proved falsifiable as well as satisfied. Two of the proofs
// drive the real defect rather than a doctored value: the seam that withholds the
// checkpointer from the DB reproduces the goroutine leak the composed teardown
// exists to prevent, and the armed fsync fault produces the failing teardown
// whose published error the identity clause is there to pin.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// runDBTeardownArm drives one arm and reports the evidence, failing the test on a
// harness error and running the non-vacuity gate before any verdict is read.
func runDBTeardownArm(t *testing.T, cfg DBTeardownConfig) *DBTeardownEvidence {
	t.Helper()
	ev, err := RunDBTeardown(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunDBTeardown(%s): %v", cfg.Arm, err)
	}
	t.Log(ev.String())
	if v := checkDBTeardownNonVacuity(&ev); len(v) > 0 {
		for _, viol := range v {
			t.Errorf("non-vacuity: %s", viol)
		}
		t.Fatal("the run proved nothing about the teardown; the verdict below would be meaningless")
	}
	return &ev
}

// TestDBTeardown_CancelledContextStillJoinsAndCloses is the cancelled-context
// arm. store/db.go documents that ctx bounds ONLY the optional final checkpoint:
// the checkpoint goroutine must still be joined, the WAL must still be closed
// durably, and every commit acknowledged before the close must still be
// recoverable. A cancellation that skipped steps 2 and 3 would reintroduce the
// goroutine and file-handle leak the type exists to prevent.
//
// What the cancellation does to step 1 is LOGGED, not asserted: TriggerCtx
// submits into a select in which both the buffered submit and ctx.Done() are
// ready, so whether the final fold happens is Go's uniform choice and pinning
// either outcome would be flaky (see db_teardown.go).
func TestDBTeardown_CancelledContextStillJoinsAndCloses(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev := runDBTeardownArm(t, DBTeardownConfig{
		Seed:              0x2475_CA_11_ED,
		Arm:               ArmDBTeardownCancelledCtx,
		Closers:           1,
		CancelBeforeClose: true,
		FinalCheckpoint:   true,
	})
	t.Logf("witness: the cancelled final checkpoint folded=%t (checkpoints %d -> %d, TriggerCtx errors=%d)",
		ev.FinalCheckpointFolded(), ev.CheckpointsBefore, ev.CheckpointsAfter, ev.TriggerCtxErrors)

	for _, viol := range checkDBTeardown(ev) {
		t.Errorf("%s", viol)
	}
}

// TestDBTeardown_ConcurrentClosersObserveOneResult is the clean concurrent arm:
// 16 goroutines mixing Close and CloseCtx, plus the two serial follow-ups that
// pin the "Close after CloseCtx" and "CloseCtx after Close" orderings. The
// teardown body must run once, every caller must observe the same value, and no
// caller may be handed a spurious wal.ErrWriterClosed.
func TestDBTeardown_ConcurrentClosersObserveOneResult(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev := runDBTeardownArm(t, DBTeardownConfig{
		Seed:    0x2475_C10_5E12,
		Arm:     ArmDBTeardownConcurrentClosers,
		Closers: dbTeardownClosers,
	})
	if !ev.CloseErrNil {
		t.Errorf("a clean teardown published %q; want nil", ev.CloseErr)
	}
	for _, viol := range checkDBTeardown(ev) {
		t.Errorf("%s", viol)
	}
}

// TestDBTeardown_ConcurrentClosersShareTheFailedResult is the arm that makes the
// identity clause discriminating. A one-shot fsync fault on the WAL close's own
// fsync gives the teardown a NON-NIL result, and the quiesce callback allocates
// its wrapper freshly per invocation — so a body that ran per caller would hand
// out N distinct values (and a second wal.Close would additionally return
// wal.ErrWriterClosed). Every caller must observe the one published value.
//
// The durability half matters at least as much: the close FAILED, and every
// commit acknowledged before it must still be recoverable.
func TestDBTeardown_ConcurrentClosersShareTheFailedResult(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev := runDBTeardownArm(t, DBTeardownConfig{
		Seed:         0x2475_FA0_17ED,
		Arm:          ArmDBTeardownConcurrentClosers,
		Closers:      dbTeardownClosers,
		FaultOnClose: true,
	})
	if ev.CloseErrNil {
		t.Fatal("the faulted teardown published nil: this arm cannot show error-value publication")
	}
	if ev.DistinctCloseErrs != 1 {
		t.Errorf("%d distinct error values across %d callers; the sync.Once must publish exactly one",
			ev.DistinctCloseErrs, ev.Closers+ev.SerialClosers)
	}
	for _, viol := range checkDBTeardown(ev) {
		t.Errorf("%s", viol)
	}
}

// TestDBTeardown_CloseWaitsForACommitInsideItsFsync is the boundary arm: the
// closers run while one commit is parked INSIDE its WAL fsync (a SimDisk sync
// gate), which is the point at which a non-quiescing close would flush and close
// the file under the committer. The closers must block until it finishes, the
// commit must be acknowledged rather than failed, and it must be recoverable.
func TestDBTeardown_CloseWaitsForACommitInsideItsFsync(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev := runDBTeardownArm(t, DBTeardownConfig{
		Seed:           0x2475_1F_11_67,
		Arm:            ArmDBTeardownInFlightCommit,
		Closers:        dbTeardownMinClosers,
		InFlightCommit: true,
	})
	if !ev.CloseBlockedOnInFlight {
		t.Error("a closer returned while the commit was still parked inside its fsync")
	}
	for _, viol := range checkDBTeardown(ev) {
		t.Errorf("%s", viol)
	}
}

// TestDBTeardown_UnjoinedCheckpointLoopIsCaught is the SENSITIVITY proof for the
// join clause, and it drives the real defect: the DB is built WITHOUT the
// checkpointer, which is exactly the hand-wired teardown store.DB exists to
// replace. The loop then survives the close and reaches a CLOSED WAL, and the
// clause must fire.
//
// It doubles as the standing proof that goleak actually covers this path. The
// probe runs at the instant the teardown claims the goroutine is joined and
// requires goleak to SEE the checkpoint loop — so a future change that stopped
// the loop by accident, or a goleak configuration that stopped watching it, fails
// here loudly instead of leaving the join clause guarding nothing. The run stops
// the loop itself afterwards, which is why the deferred VerifyNone still passes.
func TestDBTeardown_UnjoinedCheckpointLoopIsCaught(t *testing.T) {
	defer goleak.VerifyNone(t)

	var probeErr error
	ev, err := RunDBTeardown(context.Background(), DBTeardownConfig{
		Seed:                    0x2475_5EA3_C0,
		Arm:                     ArmDBTeardownConcurrentClosers,
		Closers:                 dbTeardownMinClosers,
		SkipCheckpointerHandoff: true,
		Probe:                   func() { probeErr = goleak.Find() },
	})
	if err != nil {
		t.Fatalf("RunDBTeardown(seam): %v", err)
	}
	t.Log(ev.String())

	if probeErr == nil {
		t.Error("goleak found nothing running after a teardown that joined no checkpoint goroutine: " +
			"the leak this clause guards against is invisible to the package's leak detector")
	} else if !strings.Contains(probeErr.Error(), "checkpoint") {
		t.Errorf("goleak did not report the checkpoint loop, so it is not what the join clause is watching:\n%v", probeErr)
	}

	if ev.LoopStoppedAfterClose {
		t.Fatal("the checkpoint loop was reported joined although the DB was never given it: the join probe cannot fail")
	}
	v := checkDBTeardown(&ev)
	if len(v) == 0 {
		t.Fatal("a teardown that joined no checkpoint goroutine passed: the join clause cannot fail and proves nothing")
	}
	found := false
	for _, viol := range v {
		if strings.Contains(viol.Message, "still alive after the teardown") {
			found = true
		}
	}
	if !found {
		t.Errorf("no violation names the unjoined checkpoint loop:\n%v", v)
	}
}

// healthyDBTeardownEvidence is the shape of a clean concurrent run, taken from
// the arm's own contract, so each sensitivity case below perturbs exactly one
// fact.
func healthyDBTeardownEvidence() DBTeardownEvidence {
	return DBTeardownEvidence{
		Arm:                    ArmDBTeardownConcurrentClosers,
		Closers:                dbTeardownClosers,
		SerialClosers:          2,
		CloseCalls:             dbTeardownClosers + 2,
		TeardownBodyRuns:       1,
		DistinctCloseErrs:      1,
		CloseErrNil:            true,
		AckedCommits:           dbTeardownCommits + dbTeardownWALCommits,
		AckedAfterCheckpoint:   dbTeardownWALCommits,
		AckedKeys:              []string{"acked-0000"},
		WALBytes:               1024,
		RecoveredWALOps:        2 * dbTeardownWALCommits,
		SnapshotPublished:      true,
		LoopAliveBeforeClose:   true,
		LoopStoppedAfterClose:  true,
		PostCloseCommitRefused: true,
		CheckpointerOwned:      true,
		ReopenClean:            true,
	}
}

// TestDBTeardown_ClausesFire proves every verdict clause is falsifiable. The
// adjudicator is a pure function of the evidence, so each failure it exists to
// catch is injected directly rather than hoped for.
func TestDBTeardown_ClausesFire(t *testing.T) {
	healthy := healthyDBTeardownEvidence()
	if v := checkDBTeardown(&healthy); len(v) != 0 {
		t.Fatalf("the adjudicator rejected a healthy run, so its clauses do not describe the contract:\n%v", v)
	}

	cases := []struct {
		name   string
		mutate func(*DBTeardownEvidence)
		kind   ViolationKind
		want   string
	}{
		{
			name:   "the teardown body ran twice",
			mutate: func(e *DBTeardownEvidence) { e.TeardownBodyRuns = 2 },
			kind:   ViolationOracleDeviation,
			want:   "must run it exactly once",
		},
		{
			name:   "the DB counted two errors for one teardown",
			mutate: func(e *DBTeardownEvidence) { e.CloseErrorMetric = 2 },
			kind:   ViolationOracleDeviation,
			want:   "own error counter",
		},
		{
			name: "the callers observed different error values",
			mutate: func(e *DBTeardownEvidence) {
				e.DistinctCloseErrs = 2
				e.CloseErrNil = false
				e.FaultArmed = true
				e.CloseErrIsSimFault = true
			},
			kind: ViolationOracleDeviation,
			want: "distinct error VALUES",
		},
		{
			name: "a caller was handed ErrWriterClosed",
			mutate: func(e *DBTeardownEvidence) {
				e.WriterClosedClosers = 1
				e.DistinctCloseErrs = 2
				e.CloseErrNil = false
				e.FaultArmed = true
				e.CloseErrIsSimFault = true
			},
			kind: ViolationOracleDeviation,
			want: "second WAL close",
		},
		{
			name:   "the checkpoint goroutine was not joined",
			mutate: func(e *DBTeardownEvidence) { e.LoopStoppedAfterClose = false },
			kind:   ViolationOracleDeviation,
			want:   "still alive after the teardown",
		},
		{
			name:   "the WAL was left open",
			mutate: func(e *DBTeardownEvidence) { e.PostCloseCommitRefused = false },
			kind:   ViolationACIDDurability,
			want:   "did not close the WAL",
		},
		{
			name: "the cancellation became the close's error",
			mutate: func(e *DBTeardownEvidence) {
				e.CtxCancelled = true
				e.CloseErrIsCtx = true
				e.CloseErrNil = false
				e.CloseErr = "context canceled"
			},
			kind: ViolationOracleDeviation,
			want: "bounds ONLY the final checkpoint",
		},
		{
			name: "the cancellation was counted as a checkpoint failure",
			mutate: func(e *DBTeardownEvidence) {
				e.CtxCancelled = true
				e.FinalCheckpointErrorMetric = 1
			},
			kind: ViolationOracleDeviation,
			want: "counted as a genuine failure",
		},
		{
			name: "a fault-free teardown failed",
			mutate: func(e *DBTeardownEvidence) {
				e.CloseErrNil = false
				e.CloseErr = "some other failure"
			},
			kind: ViolationOracleDeviation,
			want: "fault-free teardown returned",
		},
		{
			name: "the failure was not the injected one",
			mutate: func(e *DBTeardownEvidence) {
				e.FaultArmed = true
				e.CloseErrNil = false
				e.CloseErr = "something else"
			},
			kind: ViolationOracleDeviation,
			want: "does not carry the injected fsync fault",
		},
		{
			name: "the close did not wait for the in-flight commit",
			mutate: func(e *DBTeardownEvidence) {
				e.InFlightGated = true
				e.GateFired = true
				e.CloseBlockedOnInFlight = false
			},
			kind: ViolationACIDDurability,
			want: "parked inside its WAL fsync",
		},
		{
			name: "the in-flight commit was failed by the teardown",
			mutate: func(e *DBTeardownEvidence) {
				e.InFlightGated = true
				e.GateFired = true
				e.CloseBlockedOnInFlight = true
				e.InFlightCommitErr = "wal: writer is closed"
			},
			kind: ViolationACIDDurability,
			want: "must let it finish",
		},
		{
			name:   "an acknowledged commit was lost",
			mutate: func(e *DBTeardownEvidence) { e.MissingAckedKeys = []string{"acked-0003"} },
			kind:   ViolationACIDDurability,
			want:   "did not survive the teardown",
		},
		{
			name:   "the refused commit was resurrected",
			mutate: func(e *DBTeardownEvidence) { e.PostCloseKeyRecovered = true },
			kind:   ViolationACIDAtomicity,
			want:   "nobody was told had committed",
		},
		{
			name:   "the reopen found corruption",
			mutate: func(e *DBTeardownEvidence) { e.ReopenClean = false },
			kind:   ViolationACIDDurability,
			want:   "genuine corruption",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broken := healthyDBTeardownEvidence()
			tc.mutate(&broken)
			v := checkDBTeardown(&broken)
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

// TestDBTeardownNonVacuity_ClausesFire proves the coverage preconditions are
// independent of the verdict (rmp #2470): a run that proved nothing must be
// reported by the NON-VACUITY gate and must not be dressed up as a broken
// teardown by the verdict gate.
//
// The first case is the trap #2472 and #2474 both found in this sprint — a clause
// satisfied by definition. A teardown that acknowledged nothing satisfies
// "every acknowledged commit was recovered" over the empty set, and a durable
// image that does not exist satisfies every assertion about its contents.
func TestDBTeardownNonVacuity_ClausesFire(t *testing.T) {
	healthy := healthyDBTeardownEvidence()
	if v := checkDBTeardownNonVacuity(&healthy); len(v) != 0 {
		t.Fatalf("the non-vacuity gate fired on an ample run:\n%v", v)
	}

	// Vacuous in every general respect at once, yet a perfectly clean teardown:
	// nothing acknowledged, nothing durable, nothing replayed from the WAL, no
	// loop to join, and fewer calls than the arm claims.
	vacuous := healthyDBTeardownEvidence()
	vacuous.AckedCommits = 0
	vacuous.AckedAfterCheckpoint = 0
	vacuous.AckedKeys = nil
	vacuous.WALBytes = 0
	vacuous.RecoveredWALOps = 0
	vacuous.SnapshotPublished = false
	vacuous.LoopAliveBeforeClose = false
	vacuous.CloseCalls = 1
	v := checkDBTeardownNonVacuity(&vacuous)
	if len(v) != 5 {
		t.Fatalf("non-vacuity reported %d violation(s); want 5 (acked, post-checkpoint acked, nothing replayed or folded, live loop, call count):\n%v", len(v), v)
	}
	for _, want := range []string{"empty set", "already in the snapshot", "unaccounted for", "no goroutine to join", "Close call"} {
		found := false
		for _, viol := range v {
			if strings.Contains(viol.Message, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no non-vacuity violation mentions %q:\n%v", want, v)
		}
	}
	// The separation that matters: a merely-uninformative run is NOT reported as
	// a faulty teardown. The verdict gate sees a clean close and says nothing.
	if cv := checkDBTeardown(&vacuous); len(cv) != 0 {
		t.Errorf("the verdict gate also fired on a merely-uninformative run, so an uninformative run reads as a faulty one:\n%v", cv)
	}

	// The converse: an ample population whose teardown is broken is NOT a
	// non-vacuity problem — it is exactly what the verdict gate owns.
	broken := healthyDBTeardownEvidence()
	broken.MissingAckedKeys = []string{"acked-0001"}
	if nv := checkDBTeardownNonVacuity(&broken); len(nv) != 0 {
		t.Errorf("non-vacuity fired on an ample run whose only problem is a lost commit:\n%v", nv)
	}
	if cv := checkDBTeardown(&broken); !hasViolationKind(cv, ViolationACIDDurability) {
		t.Errorf("a lost acknowledged commit was not reported as a DURABILITY violation:\n%v", cv)
	}

	// The arm-specific floors.
	thin := healthyDBTeardownEvidence()
	thin.Closers = 2
	thin.CloseCalls = 4
	if nv := checkDBTeardownNonVacuity(&thin); !violationsMention(nv, "concurrent closer") {
		t.Errorf("a 2-closer concurrent arm passed the concurrency floor:\n%v", nv)
	}

	faultless := healthyDBTeardownEvidence()
	faultless.FaultArmed = true
	if nv := checkDBTeardownNonVacuity(&faultless); !violationsMention(nv, "satisfied by the zero value") {
		t.Errorf("an armed fault that produced no error passed the identity-arm floor:\n%v", nv)
	}

	ungated := healthyDBTeardownEvidence()
	ungated.InFlightGated = true
	if nv := checkDBTeardownNonVacuity(&ungated); !violationsMention(nv, "rendezvous never fired") {
		t.Errorf("an in-flight arm whose gate never fired passed the floor:\n%v", nv)
	}
}

// TestDBTeardown_IdentityIsNotClassEquality pins the distinction rmp #2472
// established, on the helper the identity clause is built from: two errors that
// wrap the same sentinel with the same text are the same CLASS and different
// VALUES, and only the value comparison can tell one publication from N
// re-derivations.
func TestDBTeardown_IdentityIsNotClassEquality(t *testing.T) {
	a := fmt.Errorf("sim: db-teardown WAL close: %w", ErrSimFault)
	b := fmt.Errorf("sim: db-teardown WAL close: %w", ErrSimFault)
	if a.Error() != b.Error() {
		t.Fatalf("the two probes do not render identically (%q vs %q), so this test does not pin what it claims", a, b)
	}
	if !errors.Is(a, ErrSimFault) || !errors.Is(b, ErrSimFault) {
		t.Fatal("both probes must carry the same sentinel for the class/value distinction to be the one under test")
	}
	if identicalError(a, b) {
		t.Error("identicalError conflated two distinct values that merely share a class and a message")
	}
	if !identicalError(a, a) {
		t.Error("identicalError failed to recognise a value as itself")
	}
	if !identicalError(nil, nil) {
		t.Error("identicalError treats two nil results as different")
	}
	if identicalError(a, nil) {
		t.Error("identicalError treats a failure and a success as the same result")
	}
	if got := countDistinctErrors([]error{a, b, a, nil}); got != 3 {
		t.Errorf("countDistinctErrors = %d; want 3 (a, b, nil)", got)
	}
	if got := countDistinctErrors([]error{a, a, a}); got != 1 {
		t.Errorf("countDistinctErrors over one repeated value = %d; want 1", got)
	}
}

// violationsMention reports whether any violation's message contains want.
func violationsMention(vs []Violation, want string) bool {
	for _, v := range vs {
		if strings.Contains(v.Message, want) {
			return true
		}
	}
	return false
}
