package sim

// bolt_shutdown_drain_test.go — the tests for the Bolt graceful-teardown arms
// (rmp #2483).
//
// The shape follows this sprint's two completed tasks (bolt_auth_surface_test.go,
// bolt_tx_registry_test.go): every clause of BOTH adjudicators is falsified by
// perturbing ONE field of a hand-built healthy evidence value, and at least one
// control drives a REAL server rather than a doctored struct. goleak.VerifyNone
// runs in every test, because a teardown that leaked the checkpoint goroutine or a
// connection handler is exactly the defect these arms exist to catch.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// boltDrainTestSeeds are the seeds the arms are driven at. They are arbitrary but
// FIXED, so a failure is reproducible from the test log alone.
var boltDrainTestSeeds = []uint64{0x2483_D5A1, 0x2483_0002, 0x2483_0003}

// boltDrainArmTimeout bounds one arm. An arm takes well under a second in
// practice; the bound exists so a regression that deadlocks a drain fails the test
// instead of hanging the package.
const boltDrainArmTimeout = 60 * time.Second

// -----------------------------------------------------------------------------
// The arms, driven for real
// -----------------------------------------------------------------------------

// TestBoltShutdownDrain_Arms drives every deterministic arm at several seeds and
// requires each to satisfy BOTH its contract and its non-vacuity gate.
func TestBoltShutdownDrain_Arms(t *testing.T) {
	defer goleak.VerifyNone(t)
	for _, arm := range boltDrainArms {
		for _, seed := range boltDrainTestSeeds {
			cfg, ok := boltDrainArmConfig(arm, seed)
			if !ok {
				t.Fatalf("no configuration for arm %q", arm)
			}
			ctx, cancel := context.WithTimeout(context.Background(), boltDrainArmTimeout)
			ev, err := RunBoltShutdownDrain(ctx, cfg)
			cancel()
			if err != nil {
				t.Fatalf("arm %s seed %#x: %v", arm, seed, err)
			}
			if v := checkBoltShutdownDrain(ev); len(v) > 0 {
				t.Fatalf("arm %s seed %#x contract violations %v\n%s", arm, seed, v, ev)
			}
			if v := checkBoltShutdownDrainNonVacuity(ev); len(v) > 0 {
				t.Fatalf("arm %s seed %#x coverage shortfall %v\n%s", arm, seed, v, ev)
			}
		}
	}
}

// TestBoltShutdownDrain_OrderedArmMeasurements pins the numbers the ordered arm's
// oracle rests on, so a change that quietly stopped constructing the in-flight
// rendezvous — or stopped having anything to drain — fails here rather than
// passing an oracle with nothing to adjudicate.
func TestBoltShutdownDrain_OrderedArmMeasurements(t *testing.T) {
	defer goleak.VerifyNone(t)
	cfg, _ := boltDrainArmConfig(ArmBoltDrainOrdered, boltDrainTestSeeds[0])
	ctx, cancel := context.WithTimeout(context.Background(), boltDrainArmTimeout)
	defer cancel()
	ev, err := RunBoltShutdownDrain(ctx, cfg)
	if err != nil {
		t.Fatalf("RunBoltShutdownDrain: %v", err)
	}
	t.Logf("%s", ev)

	if !ev.GateFired {
		t.Error("the fsync rendezvous was never entered: no commit was in flight")
	}
	if !ev.ListenerClosedWhileParked {
		t.Error("the listener was not observed closed while the commit was parked")
	}
	if ev.CloseBodiesWhileParked != 0 {
		t.Errorf("the store's teardown ran %d time(s) while a commit was parked, want 0", ev.CloseBodiesWhileParked)
	}
	if len(ev.CloseBodies) != 1 {
		t.Fatalf("teardown bodies = %d, want 1", len(ev.CloseBodies))
	}
	if got := ev.CloseBodies[0].LiveAt; got != 0 {
		t.Errorf("the teardown began with %d connection(s) open, want 0", got)
	}
	// The in-flight commit must be ACKNOWLEDGED and must survive recovery. This is
	// the positive half of the drain contract: an absence-only oracle would be
	// satisfied by a drain that abandoned every in-flight write.
	var inflight *BoltDrainCommit
	for i := range ev.Commits {
		if ev.Commits[i].Phase == boltDrainPhaseInFlight {
			inflight = &ev.Commits[i]
		}
	}
	if inflight == nil {
		t.Fatal("no in-flight commit was recorded")
	}
	if !inflight.RunAcked {
		t.Errorf("the in-flight commit was not acknowledged (code %q, ignored %t)", inflight.RunCode, inflight.RunIgnored)
	}
	if !containsString(ev.RecoveredNames, inflight.Name) {
		t.Errorf("the acknowledged in-flight commit %q is absent after real recovery", inflight.Name)
	}
	// The PULL is a WITNESS, not a clause: a graceful Shutdown flushes the in-flight
	// statement's reply and then closes the connection, so the client's follow-up
	// PULL routinely never lands. Logged so the behaviour stays visible.
	t.Logf("in-flight commit: run-acked=%t pull-acked=%t transport-cut=%t (PULL is a witness, not a clause)",
		inflight.RunAcked, inflight.PullAcked, inflight.Transport)
	if !ev.PostCloseCommitRefused {
		t.Errorf("a commit after the teardown returned %q, want wal.ErrWriterClosed", ev.PostCloseCommitErr)
	}
	if !ev.LoopStoppedAfterTeardown {
		t.Error("the checkpoint goroutine outlived the teardown")
	}
}

// TestBoltShutdownDrain_ExpiryClosesViaServeExit is the measurement the task most
// wanted: on BOTH of Shutdown's failure branches, WHO closes the owned store and
// WHEN.
//
// Measured, and it refutes the obvious model twice over. Neither failure branch
// closes the store — the closer had run ZERO times at the instant Shutdown
// returned — and the store is closed afterwards by Serve's own deferred exit path,
// once the abandoned connections finish. Durability is untouched: the commit that
// was parked inside its fsync when Shutdown gave up is still acknowledged and
// still present after a reopen through real recovery.
func TestBoltShutdownDrain_ExpiryClosesViaServeExit(t *testing.T) {
	defer goleak.VerifyNone(t)
	for _, arm := range []string{ArmBoltDrainExpiryDrainTimeout, ArmBoltDrainExpiryCtxCancel} {
		cfg, _ := boltDrainArmConfig(arm, boltDrainTestSeeds[0])
		ctx, cancel := context.WithTimeout(context.Background(), boltDrainArmTimeout)
		ev, err := RunBoltShutdownDrain(ctx, cfg)
		cancel()
		if err != nil {
			t.Fatalf("arm %s: %v", arm, err)
		}
		t.Logf("%s", ev)

		if ev.ShutdownFirstNil {
			t.Fatalf("arm %s: the bounded Shutdown reported success; the expiry never happened", arm)
		}
		if ev.CloseBodiesAtShutdownReturn != 0 {
			t.Errorf("arm %s: %d teardown body/bodies had run when the expiring Shutdown returned, want 0",
				arm, ev.CloseBodiesAtShutdownReturn)
		}
		if len(ev.CloseBodies) != 1 {
			t.Fatalf("arm %s: teardown bodies = %d, want 1", arm, len(ev.CloseBodies))
		}
		body := &ev.CloseBodies[0]
		if body.Attribution != boltDrainClosedByServeExit {
			t.Errorf("arm %s: the store was closed by %q, want %q", arm, body.Attribution, boltDrainClosedByServeExit)
		}
		if !body.AfterShutdownReturned {
			t.Errorf("arm %s: the store was closed BEFORE the expiring Shutdown returned", arm)
		}
		if body.LiveAt != 0 {
			t.Errorf("arm %s: the store was closed with %d connection(s) still open", arm, body.LiveAt)
		}
		if len(ev.MissingAcked) != 0 {
			t.Errorf("arm %s: acknowledged commits lost across the expiry: %v", arm, ev.MissingAcked)
		}
		// The branch each bound reaches is pinned, because they are NOT the same and
		// covering one would leave the other untested (see the file comment in
		// bolt_shutdown_drain.go).
		switch arm {
		case ArmBoltDrainExpiryDrainTimeout:
			// EITHER branch is legal here: the clamped time.After and ctx.Done() come
			// due together and Go's select is uniform when both are ready, so pinning
			// one made this test intermittently red. The distribution is measured by
			// TestBoltShutdownDrain_DeadlineExpiryBranchIsARace; what matters to THIS
			// test is that the expiry happened and left the store for Serve's exit.
			if !ev.ShutdownErrIsDrainTimeout && !ev.ShutdownErrIsCtx {
				t.Errorf("a DEADLINE-bounded Shutdown returned %q, want either the drain-timeout error or a "+
					"context error", ev.ShutdownErrs[0])
			}
			t.Logf("arm %s: expiry branch was %q", arm, ev.ShutdownErrs[0])
		case ArmBoltDrainExpiryCtxCancel:
			if !ev.ShutdownErrIsCtx {
				t.Errorf("a CANCEL-bounded Shutdown returned %q, want a context error", ev.ShutdownErrs[0])
			}
		}
		t.Logf("arm %s: Shutdown returned %q; store closed by %s, after Shutdown returned = %t",
			arm, ev.ShutdownErrs[0], body.Attribution, body.AfterShutdownReturned)
	}
}

// TestBoltShutdownDrain_DeadlineExpiryBranchIsARace measures which branch a
// DEADLINE-bounded Shutdown reports, and asserts only that it is one of the two
// legal ones.
//
// The arm this replaces asserted the drain-timeout error, on the strength of 12
// consecutive observations of it. That was luck rather than a property. Shutdown
// clamps its drain timeout to time.Until(deadline) and then selects over BOTH that
// clamped time.After and ctx.Done() (bolt/server/serve.go), so the two come due at
// very nearly the same instant and, when both are ready, Go's select picks
// UNIFORMLY at random.
//
// The distribution is heavily SKEWED toward the drain timeout, which is precisely
// what made the pinned assertion dangerous: measured 8 of 8 drain-timeout in one
// -race run of this test, and 12 of 12 when the arm was first written. But it is not
// exclusive — a deadline-bounded Shutdown was observed returning "context deadline
// exceeded" under -race, and with that and the sibling assertions pinned, 5 of 6
// -race runs of this file were red. An assertion that holds 20 times and then fails
// is worse than one that never held, because it is trusted by the time it breaks.
//
// So the distribution is REPORTED and the verdict is confined to what the contract
// promises. The precedent is checkpoint_cadence.go's TriggerCtx fold, which
// likewise measures Go's uniform choice instead of pinning it.
//
// One consequence is worth keeping in view, because it is user-visible either way:
// the drain-timeout error is built with errors.New at the call site, so a caller
// who bounds Shutdown with a deadline may receive an opaque string with no sentinel
// to match, or context.DeadlineExceeded, and cannot predict which.
func TestBoltShutdownDrain_DeadlineExpiryBranchIsARace(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*boltDrainArmTimeout)
	defer cancel()

	const rounds = 8
	var drainTimeouts, ctxErrs int
	for i := 0; i < rounds; i++ {
		cfg, _ := boltDrainArmConfig(ArmBoltDrainExpiryDrainTimeout, boltDrainTestSeeds[1])
		ev, err := RunBoltShutdownDrain(ctx, cfg)
		if err != nil {
			t.Fatalf("deadline arm round %d: %v", i, err)
		}
		if ev.ShutdownFirstNil {
			t.Fatalf("deadline arm round %d reported success; the expiry never happened", i)
		}
		switch {
		case ev.ShutdownErrIsDrainTimeout:
			drainTimeouts++
		case ev.ShutdownErrIsCtx:
			ctxErrs++
		default:
			t.Errorf("round %d: a DEADLINE-bounded Shutdown reported %q, which is neither of its two branches",
				i, ev.ShutdownErrs[0])
		}
		// Whichever branch won, the contract is the same and is asserted every round.
		if ev.CloseBodiesAtShutdownReturn != 0 {
			t.Errorf("round %d: %d teardown body/bodies had run when the expiring Shutdown returned, want 0",
				i, ev.CloseBodiesAtShutdownReturn)
		}
		if len(ev.MissingAcked) != 0 {
			t.Errorf("round %d: acknowledged commits lost across the expiry: %v", i, ev.MissingAcked)
		}
	}
	t.Logf("deadline-bounded expiry over %d rounds: drain-timeout=%d context-error=%d (Go's select is uniform when both are ready)",
		rounds, drainTimeouts, ctxErrs)

	// The CANCEL arm is not a race: a deadline-free context has no competing timer,
	// so only ctx.Done() can fire. That one IS pinned.
	cancelCfg, _ := boltDrainArmConfig(ArmBoltDrainExpiryCtxCancel, boltDrainTestSeeds[1])
	byCancel, err := RunBoltShutdownDrain(ctx, cancelCfg)
	if err != nil {
		t.Fatalf("cancel arm: %v", err)
	}
	if !byCancel.ShutdownErrIsCtx {
		t.Errorf("a CANCEL-bounded Shutdown on a DEADLINE-FREE context returned %q, want a context error: with no "+
			"deadline there is no clamped timer to race", byCancel.ShutdownErrs[0])
	}
	if byCancel.ShutdownErrIsDrainTimeout {
		t.Errorf("a CANCEL-bounded Shutdown returned the drain-timeout error (%q): the drain bound is "+
			"shutdownDrainTimeout here and cannot have elapsed", byCancel.ShutdownErrs[0])
	}
}

// TestBoltShutdownDrain_PublicationIsOneValue drives the arm whose store close is
// made to FAIL and requires every teardown caller to observe the SAME value.
//
// The identity comparison is only discriminating over a non-nil value: with a clean
// close every caller observes nil and "they agreed" is satisfied by the zero value.
// This arm therefore arms a one-shot fsync fault on the WAL close and returns a
// FRESHLY allocated wrapper per invocation, so a teardown body that ran twice would
// yield two distinct values and fail the clause — the distinction rmp #2472
// established and db_teardown.go pins for store.DB's own sync.Once. What is new
// here is the SERVER's once-guard and error cache (bolt/server closeOwned), which
// no test in the module had exercised over a failing close.
func TestBoltShutdownDrain_PublicationIsOneValue(t *testing.T) {
	defer goleak.VerifyNone(t)
	cfg, _ := boltDrainArmConfig(ArmBoltDrainOnce, boltDrainTestSeeds[0])
	ctx, cancel := context.WithTimeout(context.Background(), boltDrainArmTimeout)
	defer cancel()
	ev, err := RunBoltShutdownDrain(ctx, cfg)
	if err != nil {
		t.Fatalf("RunBoltShutdownDrain: %v", err)
	}
	t.Logf("%s", ev)

	if !ev.CloseFaultArmed {
		t.Fatal("the arm did not arm the close failure, so the identity clause is a tautology over nil")
	}
	if ev.ShutdownFirstNil {
		t.Fatal("Shutdown reported success although the store's close was made to fail")
	}
	if ev.ShutdownCalls < 2 {
		t.Fatalf("the arm made %d Shutdown call(s), want at least 2", ev.ShutdownCalls)
	}
	if ev.DistinctShutdownErrs != 1 {
		t.Errorf("%d Shutdown call(s) observed %d distinct error VALUES, want 1: %q",
			ev.ShutdownCalls, ev.DistinctShutdownErrs, ev.ShutdownErrs)
	}
	if len(ev.CloseBodies) != 1 {
		t.Errorf("the store's teardown body ran %d time(s), want 1", len(ev.CloseBodies))
	}
	if !ev.ShutdownErrIsCloseFault {
		t.Errorf("Shutdown returned %q, want the store's own close failure surfaced", ev.ShutdownErrs[0])
	}
	if !ev.ServeExitErrIsCloseFault {
		t.Errorf("Serve returned %q, want the close failure joined into it", ev.ServeExitErr)
	}
	// A failed WAL CLOSE must not endanger commits already acknowledged: their own
	// fsyncs completed long before.
	if len(ev.MissingAcked) != 0 {
		t.Errorf("a failed store close lost acknowledged commits: %v", ev.MissingAcked)
	}
}

// TestBoltShutdownDrain_Determinism requires each deterministic arm to be
// bit-reproducible from its seed: two runs of one seed must render byte-identical
// evidence.
//
// This is what the evidence's String is shaped for. Three things are deliberately
// NOT in it, each because it is not a function of the seed: the sanitised failure
// MESSAGE (it embeds a crypto-random session id), the durable WAL image's BYTE size
// (a created node's hidden key is minted from a process-global counter, so its width
// depends on what every other test in the process created first), and the identity
// of whichever stop path won the race to close the store on a successful drain.
//
// A fourth is excluded for the same reason as the third, and it took a full-suite
// run to catch: the PEAK number of concurrently-open connections. Whether an
// earlier connection's handler has finished when a later one is accepted is
// scheduling, not seed — measured flipping between 3 and 4 across two runs of one
// seed, after eight consecutive clean runs of this test had suggested otherwise. It
// stays in the evidence, where the non-vacuity gate reads it as a coverage witness,
// and out of the rendering, whose stability is asserted here.
//
// The DEADLINE-bounded expiry arm is excluded for a fifth reason, and it is a
// property of the server rather than of the harness: which of Shutdown's two expiry
// branches reports is a genuine race (the clamped time.After and ctx.Done() come due
// together and Go's select is uniform when both are ready), so that arm's rendering
// legitimately differs between two runs of one seed. It is covered instead by
// TestBoltShutdownDrain_DeadlineExpiryBranchIsARace, which measures the distribution
// and asserts only the contract both branches share. Claiming determinism for it
// would be claiming the server does something it does not.
func TestBoltShutdownDrain_Determinism(t *testing.T) {
	defer goleak.VerifyNone(t)
	arms := append(append([]string{}, boltDrainArms...), ArmBoltDrainUnordered)
	for _, arm := range arms {
		if arm == ArmBoltDrainExpiryDrainTimeout {
			continue // see the godoc above: its expiry branch is a race, not a seed function
		}
		cfg, _ := boltDrainArmConfig(arm, boltDrainTestSeeds[0])
		var first string
		for run := range 2 {
			ctx, cancel := context.WithTimeout(context.Background(), boltDrainArmTimeout)
			ev, err := RunBoltShutdownDrain(ctx, cfg)
			cancel()
			if err != nil {
				t.Fatalf("arm %s run %d: %v", arm, run, err)
			}
			got := ev.String()
			if run == 0 {
				first = got
				continue
			}
			if got != first {
				t.Errorf("arm %s is NOT bit-reproducible from its seed:\n--- run 0 ---\n%s\n--- run 1 ---\n%s",
					arm, first, got)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// The live control
// -----------------------------------------------------------------------------

// TestBoltShutdownDrain_UnorderedControlFires is the sensitivity seam, and it
// drives a REAL server rather than a doctored value: the store is closed with a
// client live and mid-session and NO drain at all, which is precisely what the
// documented ordering exists to prevent.
//
// It must fail the contract on three independent clauses, and it must NOT fail the
// non-vacuity gate — the run is fully informative, it is the SERVER's behaviour
// under an out-of-order teardown that is wrong. That separation is the rmp #2470
// rule: a coverage shortfall and a defect must never read alike.
//
// It also establishes what a client is actually told, which is the only reason the
// wire clause can be written at all: wal.ErrWriterClosed never reaches a client
// (Session.sanitiseErr replaces its text), so the observable is the
// DatabaseError-class CODE and nothing finer.
func TestBoltShutdownDrain_UnorderedControlFires(t *testing.T) {
	defer goleak.VerifyNone(t)
	cfg, _ := boltDrainArmConfig(ArmBoltDrainUnordered, boltDrainTestSeeds[0])
	ctx, cancel := context.WithTimeout(context.Background(), boltDrainArmTimeout)
	defer cancel()
	ev, err := RunBoltShutdownDrain(ctx, cfg)
	if err != nil {
		t.Fatalf("RunBoltShutdownDrain: %v", err)
	}
	t.Logf("%s", ev)

	if v := checkBoltShutdownDrainNonVacuity(ev); len(v) > 0 {
		t.Fatalf("the control tripped the COVERAGE gate, which would make its failure uninformative: %v", v)
	}
	v := checkBoltShutdownDrain(ev)
	if len(v) == 0 {
		t.Fatal("the out-of-order control passed every contract clause: the ordering oracle cannot fail, " +
			"so its silence on the real arms proves nothing")
	}
	wantClauses := []string{"close-once", "drain-before-close", "wire-storage-failure"}
	for _, clause := range wantClauses {
		if !violationsMentionClause(v, clause) {
			t.Errorf("the out-of-order control did not fire the %q clause; it fired: %v", clause, v)
		}
	}
	// The wire signature is pinned: this is what a driver sees when a teardown races
	// its write, and it is a DatabaseError with no mention of a closed writer.
	var undrained *BoltDrainCommit
	for i := range ev.Commits {
		if ev.Commits[i].Phase == boltDrainPhaseUndrained {
			undrained = &ev.Commits[i]
		}
	}
	if undrained == nil {
		t.Fatal("the control recorded no write on the undrained connection")
	}
	if undrained.RunCode != boltDrainDatabaseErrorCode {
		t.Errorf("the undrained write was answered %q, want %q", undrained.RunCode, boltDrainDatabaseErrorCode)
	}
	// The store-level witness proves the WAL really was closed, and that the
	// writer-closed detector every other clause relies on can see one at all.
	if !ev.PostCloseCommitRefused {
		t.Errorf("a commit after the out-of-order close returned %q, want wal.ErrWriterClosed", ev.PostCloseCommitErr)
	}
	t.Logf("out-of-order teardown fired %d contract clause(s); client was told %q", len(v), undrained.RunCode)
}

// TestBoltShutdownDrain_IgnoredIsNotAnAcknowledgement pins the classification that
// this file's first oracle got wrong.
//
// A Bolt session that has been answered a FAILURE is in FAILED and answers every
// subsequent request-phase message with IGNORED. Reading the acknowledgement as
// "not a FAILURE" therefore counts a statement that was never dispatched as a
// durable commit — which is how the concurrent arm manufactured an ACID_DURABILITY
// violation in 8 of 30 runs, demanding from recovery a name that was neither in the
// live engine nor anywhere in the raw WAL image.
//
// The classifier is exercised directly, over the three replies it must separate.
func TestBoltShutdownDrain_IgnoredIsNotAnAcknowledgement(t *testing.T) {
	defer goleak.VerifyNone(t)
	cfg, _ := boltDrainArmConfig(ArmBoltDrainOrdered, boltDrainTestSeeds[0])
	r, err := openBoltDrainRunner(cfg)
	if err != nil {
		t.Fatalf("openBoltDrainRunner: %v", err)
	}
	defer r.release()

	ctx := context.Background()
	c, err := r.dialReady(ctx)
	if err != nil {
		t.Fatalf("dialReady: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Drive the session into FAILED with a statement that cannot parse, then attempt
	// a perfectly valid write on the same connection.
	bad, err := c.Run("THIS IS NOT CYPHER", nil)
	if err != nil {
		t.Fatalf("RUN (malformed): %v", err)
	}
	if isSuccess(bad) {
		t.Fatal("a malformed statement was answered SUCCESS; the session never entered FAILED")
	}
	row := r.writeOverWire(c, boltDrainPhasePre, "ignored-probe")
	if !row.RunIgnored {
		t.Fatalf("the write on a FAILED session was classified run-acked=%t code=%q, want IGNORED",
			row.RunAcked, row.RunCode)
	}
	if row.RunAcked {
		t.Error("an IGNORED reply was counted as an acknowledgement")
	}
	if containsString(r.ev.AckedNames, "ignored-probe") {
		t.Error("an IGNORED reply entered the acknowledged set, which the durability clause is answered against")
	}
	// And the node genuinely does not exist, which is what makes the mis-classification
	// a durability false positive rather than a cosmetic one.
	if nodes, _, nerr := boltDrainRecoveredNames(ctx, r.st); nerr != nil {
		t.Fatalf("read graph: %v", nerr)
	} else if containsString(nodes, "ignored-probe") {
		t.Error("the ignored statement was applied after all")
	}
}

// -----------------------------------------------------------------------------
// Falsifiability: every clause of BOTH adjudicators
// -----------------------------------------------------------------------------

// healthyBoltDrainEvidence returns an evidence value that satisfies every clause of
// both adjudicators, shaped like the ordered arm. The falsifiability tables below
// perturb ONE field of it at a time, so each test names exactly one clause.
//
// It is hand-built rather than captured from a run on purpose: a captured value
// would silently track a change in the arms, and a clause that stopped being
// reachable would keep passing.
func healthyBoltDrainEvidence() *BoltDrainEvidence {
	return &BoltDrainEvidence{
		Arm:           ArmBoltDrainOrdered,
		Seed:          1,
		CloserWired:   true,
		ConnDecorated: true,
		CloseBodies: []boltDrainCloseBody{{
			Attribution: boltDrainClosedByShutdown,
			AcceptedAt:  8, ClosedAt: 8, LiveAt: 0,
		}},
		ConnsAccepted:             8,
		ConnsClosed:               8,
		ConnsPeak:                 4,
		ConnsLiveAtEnd:            0,
		IdleConnsOpened:           boltDrainIdleConns,
		GateArmed:                 true,
		GateFired:                 true,
		ParkedLiveConns:           4,
		ListenerClosedWhileParked: true,
		DialRefusedAfterTeardown:  true,
		ShutdownErrs:              []string{"", ""},
		ShutdownFirstNil:          true,
		LastShutdownNil:           true,
		ShutdownCalls:             2,
		DistinctShutdownErrs:      1,
		ParkExpected:              true,
		AckedAfterCheckpoint:      1,
		Commits: []BoltDrainCommit{
			{Name: "pre-0", Phase: boltDrainPhasePre, RunAcked: true, PullAcked: true},
			{Name: "suffix-0", Phase: boltDrainPhaseWALSuffix, RunAcked: true, PullAcked: true},
			{Name: "inflight-0", Phase: boltDrainPhaseInFlight, RunAcked: true, Transport: true},
		},
		PostCloseCommitErr:       "wal: writer is closed",
		PostCloseCommitRefused:   true,
		LoopAliveBeforeTeardown:  true,
		LoopStoppedAfterTeardown: true,
		AckedNames:               []string{"inflight-0", "pre-0", "suffix-0"},
		IssuedNames:              []string{"inflight-0", "pre-0", "suffix-0"},
		RecoveredNames:           []string{"inflight-0", "pre-0", "suffix-0"},
		RecoveredWALOps:          8,
		WALBytes:                 499,
		SnapshotPublished:        true,
		ReopenClean:              true,
	}
}

// TestBoltShutdownDrain_HealthyEvidencePasses is the table's own precondition: the
// baseline must satisfy both adjudicators, or a perturbation proves nothing about
// the clause it aimed at.
func TestBoltShutdownDrain_HealthyEvidencePasses(t *testing.T) {
	defer goleak.VerifyNone(t)
	ev := healthyBoltDrainEvidence()
	if v := checkBoltShutdownDrain(ev); len(v) > 0 {
		t.Fatalf("the hand-built healthy evidence fails the contract: %v", v)
	}
	if v := checkBoltShutdownDrainNonVacuity(ev); len(v) > 0 {
		t.Fatalf("the hand-built healthy evidence fails the coverage gate: %v", v)
	}
}

// TestBoltShutdownDrain_ContractFalsifiability perturbs one field per sub-test and
// requires the named clause to fire.
func TestBoltShutdownDrain_ContractFalsifiability(t *testing.T) {
	defer goleak.VerifyNone(t)
	tests := []struct {
		name    string
		clause  string
		perturb func(*BoltDrainEvidence)
	}{
		{"teardown ran twice", "close-once", func(e *BoltDrainEvidence) {
			e.CloseBodies = append(e.CloseBodies, e.CloseBodies[0])
		}},
		{"teardown ran not at all", "close-once", func(e *BoltDrainEvidence) {
			e.CloseBodies = nil
		}},
		{"close began before the drain finished", "drain-before-close", func(e *BoltDrainEvidence) {
			e.CloseBodies[0].LiveAt = 2
		}},
		{"close reached from an unmodelled path", "close-attribution", func(e *BoltDrainEvidence) {
			e.CloseBodies[0].Attribution = boltDrainClosedByUnknown
		}},
		{"close ran while a commit was parked", "close-while-parked", func(e *BoltDrainEvidence) {
			e.CloseBodiesWhileParked = 1
		}},
		{"a connection outlived the teardown", "conn-residue", func(e *BoltDrainEvidence) {
			e.ConnsLiveAtEnd = 1
		}},
		{"a fresh connection was accepted after the teardown", "no-new-work", func(e *BoltDrainEvidence) {
			e.DialRefusedAfterTeardown = false
		}},
		{"an unbounded Shutdown reported failure", "drain-success", func(e *BoltDrainEvidence) {
			e.ShutdownFirstNil = false
			e.ShutdownErrs[0] = "boom"
		}},
		{"repeated callers observed two values", "published-identity", func(e *BoltDrainEvidence) {
			e.DistinctShutdownErrs = 2
		}},
		// The parked in-flight commit is adjudicated in two steps rather than one,
		// because it legitimately never reaches the acknowledged set: a graceful
		// Shutdown flushes its RUN reply and then closes the connection, so its
		// TERMINAL — the reply that carries the bookmark — never arrives. What the
		// drain owes it is that the statement it found executing nonetheless RAN and
		// is DURABLE, so there is a clause for each half.
		{"the in-flight statement was never dispatched", "inflight-dispatched", func(e *BoltDrainEvidence) {
			e.Commits[2].RunAcked = false
		}},
		{"the in-flight statement ran and its effect was discarded", "inflight-durable", func(e *BoltDrainEvidence) {
			e.RecoveredNames = slices.DeleteFunc(slices.Clone(e.RecoveredNames), func(n string) bool {
				return n == e.Commits[2].Name
			})
		}},
		{"a client on an undrained connection got a storage failure", "wire-storage-failure", func(e *BoltDrainEvidence) {
			e.Commits[0].RunCode = boltDrainDatabaseErrorCode
		}},
		{"a storage failure arrived on the PULL terminal", "wire-storage-failure", func(e *BoltDrainEvidence) {
			e.Commits[0].PullCode = boltDrainDatabaseErrorCode
		}},
		{"a deterministic arm was answered IGNORED", "wire-ignored", func(e *BoltDrainEvidence) {
			e.Commits[0].RunIgnored = true
		}},
		{"an unexpected failure code appeared", "wire-unexpected-code", func(e *BoltDrainEvidence) {
			e.Commits[0].RunCode = "Neo.ClientError.Statement.SyntaxError"
		}},
		{"an acknowledged commit is absent after recovery", "acked-lost", func(e *BoltDrainEvidence) {
			e.MissingAcked = []string{"pre-0"}
		}},
		{"recovery invented a node nobody issued", "phantom", func(e *BoltDrainEvidence) {
			e.PhantomNames = []string{"ghost"}
		}},
		{"a torn transaction was resurrected", "torn-create", func(e *BoltDrainEvidence) {
			e.PartialNames = []string{"pre-0"}
		}},
		{"recovery reported an unclean image", "reopen-unclean", func(e *BoltDrainEvidence) {
			e.ReopenClean = false
		}},
		{"the checkpoint goroutine outlived the teardown", "checkpointer-join", func(e *BoltDrainEvidence) {
			e.LoopStoppedAfterTeardown = false
		}},
		{"the WAL was not actually closed", "wal-closed", func(e *BoltDrainEvidence) {
			e.PostCloseCommitRefused = false
			e.PostCloseCommitErr = ""
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := healthyBoltDrainEvidence()
			tc.perturb(ev)
			v := checkBoltShutdownDrain(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbing %q did not fire the %q clause; violations: %v", tc.name, tc.clause, v)
			}
		})
	}
}

// TestBoltShutdownDrain_ExpiryContractFalsifiability covers the clauses that apply
// only to an expiry arm, over an expiry-shaped baseline.
func TestBoltShutdownDrain_ExpiryContractFalsifiability(t *testing.T) {
	defer goleak.VerifyNone(t)
	// Shape the baseline as the drain-timeout arm: bounded Shutdown, a non-nil first
	// error, the store closed by Serve's exit after Shutdown returned, and the last
	// (late) Shutdown observing the completed teardown.
	healthyExpiry := func() *BoltDrainEvidence {
		e := healthyBoltDrainEvidence()
		e.Arm = ArmBoltDrainExpiryDrainTimeout
		e.ExpiryExpected = true
		e.ShutdownExpiryBudget = boltDrainExpiryBudget
		e.ShutdownFirstNil = false
		e.ShutdownErrIsDrainTimeout = true
		e.ShutdownErrs = []string{"bolt: shutdown: drain timeout exceeded", ""}
		e.LastShutdownNil = true
		e.DistinctShutdownErrs = 2
		e.CloseBodies[0].Attribution = boltDrainClosedByServeExit
		e.CloseBodies[0].AfterShutdownReturned = true
		e.CloseBodiesAtShutdownReturn = 0
		return e
	}
	if v := checkBoltShutdownDrain(healthyExpiry()); len(v) > 0 {
		t.Fatalf("the expiry baseline fails the contract: %v", v)
	}
	if v := checkBoltShutdownDrainNonVacuity(healthyExpiry()); len(v) > 0 {
		t.Fatalf("the expiry baseline fails the coverage gate: %v", v)
	}

	tests := []struct {
		name    string
		clause  string
		perturb func(*BoltDrainEvidence)
	}{
		{"a failure branch closed the store", "expiry-closed-early", func(e *BoltDrainEvidence) {
			e.CloseBodiesAtShutdownReturn = 1
		}},
		{"the store was closed by Shutdown after all", "expiry-close-attribution", func(e *BoltDrainEvidence) {
			e.CloseBodies[0].Attribution = boltDrainClosedByShutdown
		}},
		{"the store was closed before Shutdown returned", "expiry-close-order", func(e *BoltDrainEvidence) {
			e.CloseBodies[0].AfterShutdownReturned = false
		}},
		// Clearing ONE branch flag no longer fires: either branch is legal on a
		// deadline-bounded expiry (the clause accepts both, because Go's select is
		// uniform when both timers are ready). What must still fire is an expiry that
		// reported NEITHER — an error from outside Shutdown's two exits.
		{"the expiry reported neither of Shutdown's two branches", "expiry-branch", func(e *BoltDrainEvidence) {
			e.ShutdownErrIsDrainTimeout = false
			e.ShutdownErrIsCtx = false
		}},
		{"the late Shutdown never succeeded", "expiry-eventual-success", func(e *BoltDrainEvidence) {
			e.LastShutdownNil = false
			e.ShutdownErrs[1] = "still broken"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := healthyExpiry()
			tc.perturb(ev)
			v := checkBoltShutdownDrain(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbing %q did not fire the %q clause; violations: %v", tc.name, tc.clause, v)
			}
		})
	}

	// The ctx-cancel arm's branch clause, over its own baseline.
	t.Run("the ctx branch was not the one taken", func(t *testing.T) {
		ev := healthyExpiry()
		ev.Arm = ArmBoltDrainExpiryCtxCancel
		ev.ShutdownExpiryByCancel = true
		ev.ShutdownErrIsDrainTimeout = false
		ev.ShutdownErrIsCtx = false
		ev.ShutdownErrs = []string{"bolt: shutdown: drain timeout exceeded", ""}
		v := checkBoltShutdownDrain(ev)
		if !violationsMentionClause(v, "expiry-branch") {
			t.Fatalf("a cancel-bounded arm that took the drain-timeout branch did not fire the clause: %v", v)
		}
	})
}

// TestBoltShutdownDrain_CloseFaultFalsifiability covers the two clauses that apply
// only when the store's close was made to fail.
func TestBoltShutdownDrain_CloseFaultFalsifiability(t *testing.T) {
	defer goleak.VerifyNone(t)
	healthyFault := func() *BoltDrainEvidence {
		e := healthyBoltDrainEvidence()
		e.Arm = ArmBoltDrainOnce
		e.ParkExpected = false
		e.GateArmed = false
		e.GateFired = false
		e.ParkedLiveConns = 0
		e.ListenerClosedWhileParked = false
		e.CloseFaultArmed = true
		e.ShutdownFirstNil = false
		e.ShutdownErrIsCloseFault = true
		e.ServeExitErrIsCloseFault = true
		e.ShutdownErrs = []string{"bolt: close owned store: injected", "bolt: close owned store: injected"}
		e.ServeExitErr = "bolt: close owned store: injected"
		// No parked commit on this arm, so there is no in-flight row.
		e.Commits = e.Commits[:2]
		return e
	}
	if v := checkBoltShutdownDrain(healthyFault()); len(v) > 0 {
		t.Fatalf("the close-fault baseline fails the contract: %v", v)
	}
	if v := checkBoltShutdownDrainNonVacuity(healthyFault()); len(v) > 0 {
		t.Fatalf("the close-fault baseline fails the coverage gate: %v", v)
	}

	tests := []struct {
		name    string
		clause  string
		perturb func(*BoltDrainEvidence)
	}{
		{"Shutdown swallowed the close failure", "close-failure-surfaced", func(e *BoltDrainEvidence) {
			e.ShutdownErrIsCloseFault = false
		}},
		{"Serve swallowed the close failure", "close-failure-joined", func(e *BoltDrainEvidence) {
			e.ServeExitErrIsCloseFault = false
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := healthyFault()
			tc.perturb(ev)
			v := checkBoltShutdownDrain(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbing %q did not fire the %q clause; violations: %v", tc.name, tc.clause, v)
			}
		})
	}
}

// TestBoltShutdownDrain_NonVacuityFalsifiability perturbs one field per sub-test
// and requires the named COVERAGE clause to fire. It is a separate table from the
// contract's because the two must never be confused: a shortfall here means the run
// proved nothing, not that the server is broken (rmp #2470).
func TestBoltShutdownDrain_NonVacuityFalsifiability(t *testing.T) {
	defer goleak.VerifyNone(t)
	tests := []struct {
		name    string
		clause  string
		perturb func(*BoltDrainEvidence)
	}{
		{"the server owned no closer", "nonvacuity-closer", func(e *BoltDrainEvidence) {
			e.CloserWired = false
		}},
		{"the ordering observable was absent", "nonvacuity-observable", func(e *BoltDrainEvidence) {
			e.ConnDecorated = false
		}},
		{"only one connection ever existed", "nonvacuity-conns", func(e *BoltDrainEvidence) {
			e.ConnsPeak = 1
		}},
		{"the idle connections were not opened", "nonvacuity-idle", func(e *BoltDrainEvidence) {
			e.IdleConnsOpened = 0
		}},
		{"nothing was acknowledged", "nonvacuity-acked", func(e *BoltDrainEvidence) {
			e.AckedNames = nil
		}},
		{"nothing was acknowledged after the checkpoint", "nonvacuity-wal-suffix", func(e *BoltDrainEvidence) {
			e.AckedAfterCheckpoint = 0
		}},
		{"recovery replayed no WAL op", "nonvacuity-wal-replay", func(e *BoltDrainEvidence) {
			e.RecoveredWALOps = 0
		}},
		{"no snapshot was published", "nonvacuity-snapshot", func(e *BoltDrainEvidence) {
			e.SnapshotPublished = false
		}},
		{"the checkpoint loop was never running", "nonvacuity-loop", func(e *BoltDrainEvidence) {
			e.LoopAliveBeforeTeardown = false
		}},
		{"no write was issued at all", "nonvacuity-commits", func(e *BoltDrainEvidence) {
			e.Commits = nil
		}},
		{"the rendezvous was never armed", "nonvacuity-gate", func(e *BoltDrainEvidence) {
			e.GateArmed = false
		}},
		{"the rendezvous was never entered", "nonvacuity-gate", func(e *BoltDrainEvidence) {
			e.GateFired = false
		}},
		{"Shutdown was never shown to have started draining", "nonvacuity-shutdown-progress", func(e *BoltDrainEvidence) {
			e.ListenerClosedWhileParked = false
		}},
		{"the drain had only the parked connection to wait for", "nonvacuity-parked-conns", func(e *BoltDrainEvidence) {
			e.ParkedLiveConns = 1
		}},
		{"no in-flight write was recorded", "nonvacuity-inflight-row", func(e *BoltDrainEvidence) {
			e.Commits = e.Commits[:2]
		}},
		{"only one Shutdown call was made", "nonvacuity-shutdown-calls", func(e *BoltDrainEvidence) {
			e.ShutdownCalls = 1
		}},
		{"an expiry arm gave Shutdown no bound", "nonvacuity-expiry", func(e *BoltDrainEvidence) {
			e.ExpiryExpected = true
			e.ShutdownExpiryBudget = 0
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := healthyBoltDrainEvidence()
			tc.perturb(ev)
			v := checkBoltShutdownDrainNonVacuity(ev)
			if !violationsMentionClause(v, tc.clause) {
				t.Fatalf("perturbing %q did not fire the %q coverage clause; violations: %v", tc.name, tc.clause, v)
			}
		})
	}

	t.Run("the fleet arm held too few connections", func(t *testing.T) {
		ev := healthyBoltDrainEvidence()
		ev.Arm = ArmBoltDrainFleet
		ev.FleetArm = true
		ev.ParkExpected = false
		ev.GateArmed = false
		ev.GateFired = false
		ev.IdleConnsOpened = 0
		ev.ConnsPeak = 2
		v := checkBoltShutdownDrainNonVacuity(ev)
		if !violationsMentionClause(v, "nonvacuity-fleet") {
			t.Fatalf("a fleet arm below its connection floor did not fire the clause: %v", v)
		}
	})
}

// -----------------------------------------------------------------------------
// The concurrent arm and the catalogue
// -----------------------------------------------------------------------------

// TestBoltShutdownFleet_Scenario drives the concurrent arm through its registered
// scenario. It is NOT bit-reproducible — which commit is mid-fsync when the drain
// starts depends on the scheduler — so it is guarded on leak-freedom, absence of
// panic, and the invariants that hold under ANY interleaving.
func TestBoltShutdownFleet_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := boltShutdownFleetScenario()
	for _, seed := range boltDrainTestSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), boltDrainArmTimeout)
		report, err := sc.Run(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("fleet seed %#x: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("fleet seed %#x violation:\n%s", seed, report)
		}
	}
}

// TestBoltShutdownDrain_Scenario drives the deterministic scenario through the
// catalogue entry the CLI resolves, so the registration itself is covered.
func TestBoltShutdownDrain_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioBoltShutdownDrain)
	if !ok {
		t.Fatalf("scenario %q missing from the catalogue", ScenarioBoltShutdownDrain)
	}
	if sc.Mode != ModeDeterministic {
		t.Errorf("scenario mode = %q, want %q", sc.Mode, ModeDeterministic)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*boltDrainArmTimeout)
	defer cancel()
	report, err := sc.Run(ctx, sc.DefaultSeed)
	if err != nil {
		t.Fatalf("scenario run: %v", err)
	}
	if report != nil {
		t.Fatalf("scenario violation:\n%s", report)
	}
}

// TestBoltShutdownDrain_SoakSweep repeats both scenarios across many seeds. It is
// the soak-layer arm: the short layer covers each arm at three seeds, and a
// teardown-only leak or a rare interleaving needs volume to surface.
func TestBoltShutdownDrain_SoakSweep(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)
	for i := range 40 {
		seed := boltShutdownDrainDefaultSeed + uint64(i)
		ctx, cancel := context.WithTimeout(context.Background(), 4*boltDrainArmTimeout)
		report, err := runBoltShutdownDrainScenario(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("deterministic sweep seed %#x: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("deterministic sweep seed %#x violation:\n%s", seed, report)
		}

		ctx, cancel = context.WithTimeout(context.Background(), boltDrainArmTimeout)
		report, err = runBoltShutdownFleetScenario(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("fleet sweep seed %#x: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("fleet sweep seed %#x violation:\n%s", seed, report)
		}
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// violationsMentionClause reports whether any violation's Op names the clause.
func violationsMentionClause(v []Violation, clause string) bool {
	for i := range v {
		if strings.Contains(v[i].Op, ":"+clause+">") {
			return true
		}
	}
	return false
}

// containsString reports whether xs contains s.
func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
