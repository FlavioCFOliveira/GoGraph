package sim

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// txTerminateDefaultSeed is the seed the clean run and the live control use.
const txTerminateDefaultSeed = 0x2482_7E12

// TestBoltTxTerminate_Clean drives the whole operator-termination arm against a
// real WAL-backed server on virtual time and asserts both the contract and the
// non-vacuity gate are satisfied.
//
// It also pins the arm's GEOMETRY, because every other clause depends on it:
// ZERO advances is what makes "every departure from the registry is the
// operator's doing" a proof rather than a hope, and the four BEGINs are what fix
// every predicted id suffix.
func TestBoltTxTerminate_Clean(t *testing.T) {
	defer goleak.VerifyNone(t)

	start := time.Now()
	ev, err := RunBoltTxTerminate(context.Background(), txTerminateDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltTxTerminate: %v", err)
	}
	elapsed := time.Since(start)

	if ev.Advances != 0 {
		t.Errorf("the arm advanced the fake clock %d time(s), want 0: with any advance a departure could be a "+
			"reaper's work and the whole attribution collapses", ev.Advances)
	}
	if want := len(txTerminateRoles) + 1; ev.Begins != want {
		t.Errorf("the arm made %d BEGIN(s), want %d (one per ledger role plus the successor)", ev.Begins, want)
	}
	for _, v := range checkBoltTxTerminate(ev) {
		t.Errorf("contract violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	for _, v := range checkBoltTxTerminateNonVacuity(ev) {
		t.Errorf("non-vacuity violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	t.Logf("arm wall time %s (no virtual time was advanced at all)", elapsed)
	if t.Failed() {
		t.Log("\n" + ev.String())
	}
}

// TestBoltTxTerminate_Deterministic asserts one seed produces one run.
//
// The decisive comparison is of the RENDERED evidence, because
// [BoltTxTerminateEvidence.String] deliberately renders only what is seed-pure:
// no raw transaction id (the prefix is crypto/rand), no Now count (it counts how
// often the harness polled the registry), and no honest-window byte total (the
// frame embeds a node key drawn from a process-global counter in cypher/exec, so
// its WIDTH depends on how many nodes the rest of the process created first).
// Those two are asserted to be PRESENT rather than equal, so a field that
// silently stopped being collected is still caught.
func TestBoltTxTerminate_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const seed = 0x5EED_7E12
	first, err := RunBoltTxTerminate(context.Background(), seed)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunBoltTxTerminate(context.Background(), seed)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("two runs of seed %d rendered differently:\n--- first ---\n%s\n--- second ---\n%s",
			seed, first.String(), second.String())
	}
	if first.TimersArmed != second.TimersArmed || first.Untils != second.Untils || first.Tickers != second.Tickers {
		t.Errorf("clock counters diverged: first(timers %d untils %d tickers %d) second(timers %d untils %d tickers %d)",
			first.TimersArmed, first.Untils, first.Tickers,
			second.TimersArmed, second.Untils, second.Tickers)
	}
	if len(first.Plan) != len(second.Plan) {
		t.Fatalf("ledger lengths differ: %d vs %d", len(first.Plan), len(second.Plan))
	}
	for i := range first.Plan {
		if a, b := &first.Plan[i], &second.Plan[i]; *a != *b {
			t.Errorf("ledger row %d diverged:\n first=%+v\nsecond=%+v", i, *a, *b)
		}
	}
	if len(first.Calls) != len(second.Calls) {
		t.Fatalf("call rosters differ: %d vs %d", len(first.Calls), len(second.Calls))
	}
	for i := range first.Calls {
		if a, b := &first.Calls[i], &second.Calls[i]; *a != *b {
			t.Errorf("terminate call %d diverged:\n first=%+v\nsecond=%+v", i, *a, *b)
		}
	}
	if len(first.Windows) != len(second.Windows) {
		t.Fatalf("window counts differ: %d vs %d", len(first.Windows), len(second.Windows))
	}
	for i := range first.Windows {
		a, b := &first.Windows[i], &second.Windows[i]
		if a.framesAppended() != b.framesAppended() {
			t.Errorf("window %q appended %d frame(s) then %d", a.Name, a.framesAppended(), b.framesAppended())
		}
		if a.bytesAppended() == 0 || b.bytesAppended() == 0 {
			t.Errorf("window %q appended %d then %d WAL byte(s); a window that writes nothing cannot witness the "+
				"termination bracket being zero", a.Name, a.bytesAppended(), b.bytesAppended())
		}
	}
	if first.Nows == 0 || second.Nows == 0 {
		t.Errorf("the server made %d then %d Now call(s) on the injected clock; the seam must be reached in both runs",
			first.Nows, second.Nows)
	}
}

// TestBoltTxTerminate_StaleIDsAreRefused asserts the two stale cases directly on
// a live run, because they are what this arm adds over
// bolt/server/tx_introspection_test.go.
//
// That file's TestTxIntrospection_TerminateUnknownID covers an id the server has
// NEVER minted: a hand-written string. Both cases here are strictly stronger,
// because the harness WATCHED each id be live and then be listed no longer —
// once after an operator termination, once after a client COMMIT. An
// implementation that remembered ended transactions, or that matched on the
// connection rather than on the id, would pass the never-seen case and fail
// these.
func TestBoltTxTerminate_StaleIDsAreRefused(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunBoltTxTerminate(context.Background(), txTerminateDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltTxTerminate: %v", err)
	}
	byName := make(map[string]*BoltTxTerminateCall, len(ev.Calls))
	for i := range ev.Calls {
		byName[ev.Calls[i].Name] = &ev.Calls[i]
	}
	for _, name := range []string{txTermCallStaleTerminated, txTermCallStaleCommitted, txTermCallStaleVsSuccessor} {
		c := byName[name]
		if c == nil {
			t.Errorf("the run never made the %q terminate call", name)
			continue
		}
		if c.Got != txTermOutcomeNoSuchTx {
			t.Errorf("terminating the %s id (%s) returned %s, want %s", name, c.Target, c.Got, txTermOutcomeNoSuchTx)
		}
	}
	if c := byName[txTermCallLive]; c == nil || c.Got != txTermOutcomeOK {
		t.Errorf("the LIVE terminate call is missing or did not return nil (%+v): without it the three refusals "+
			"above are equally true of a server that refuses every id", c)
	}
	// Successor immunity, asserted where a reader looks for it rather than only
	// through the adjudicator.
	if ev.SuccessorSuffix == ev.Plan[0].IDSuffix {
		t.Errorf("the successor reused the predecessor's id suffix %s: nextID's sequence must only ever increase",
			ev.SuccessorSuffix)
	}
	if !ev.SuccessorListed {
		t.Errorf("the successor transaction %s was gone after its predecessor's id was terminated", ev.SuccessorSuffix)
	}
}

// cleanTxTerminateEvidence returns a hand-built evidence value that both checkers
// pass, so a perturbation test can attribute any violation to the field it
// changed. Its shape mirrors the arm's nominal geometry, and
// TestBoltTxTerminate_Clean pins that geometry on a real run, so the two cannot
// drift apart silently.
func cleanTxTerminateEvidence() BoltTxTerminateEvidence {
	ev := BoltTxTerminateEvidence{
		Arm: boltTxArmTerminate, Seed: 1,
		IdleBound: txTerminateBound, TotalBound: txTerminateBound,
		Advances: 0, Begins: len(txTerminateRoles) + 1,
		TimersArmed: int64(len(txTerminateRoles) + 1), Untils: int64(len(txTerminateRoles) + 1),
		Tickers: 0, Nows: 137,
		RegistryPeak:  len(txTerminateRoles),
		VictimCode:    txTerminateFailureCode,
		VictimMessage: txTerminateFailureMessage,
		// A termination is a rollback: it appends nothing.
		TermFrames: 0, TermBytes: 0,
		SuccessorSuffix: "-4", WantSuccessorSuffix: "-4",
		SuccessorListed: true, BystanderListed: true,
		GhostsLive: 0, GhostsRecovered: 0,
		CommittedLive: 1, CommittedRecovered: 1,
	}
	ev.Plan = make([]BoltTxTerminateRow, 0, len(txTerminateRoles))
	for k, role := range txTerminateRoles {
		spec := terminateRoleSpec(role)
		suffix := fmt.Sprintf("-%d", k+1)
		ev.Plan = append(ev.Plan, BoltTxTerminateRow{
			Role: role, Principal: "tx-term-" + role, Mode: spec.mode, Query: spec.query,
			IDSuffix: suffix, WantSuffix: suffix, Conn: k,
		})
	}
	// Sorted, because the arm never advances its clock and the listing's ORDER is
	// therefore Go map-iteration order (see the file comment in
	// bolt_tx_terminate.go).
	ev.ListedAtCap = []string{"-1", "-2", "-3"}
	ev.ListedAfterTerminate = []string{"-2", "-3"}
	ev.Calls = []BoltTxTerminateCall{
		{Name: txTermCallLive, Target: "-1", Got: txTermOutcomeOK, Want: txTermOutcomeOK},
		{Name: txTermCallStaleTerminated, Target: "-1", Got: txTermOutcomeNoSuchTx, Want: txTermOutcomeNoSuchTx},
		{Name: txTermCallStaleCommitted, Target: "-3", Got: txTermOutcomeNoSuchTx, Want: txTermOutcomeNoSuchTx},
		{Name: txTermCallStaleVsSuccessor, Target: "-1", Got: txTermOutcomeNoSuchTx, Want: txTermOutcomeNoSuchTx},
	}
	var frames, bytes uint64
	for _, name := range []string{txWindowBefore, txWindowAfter} {
		w := BoltTxWriteWindow{
			Name: name, Node: "honest-" + name, Committed: true,
			FramesBefore: frames, BytesBefore: bytes,
		}
		frames, bytes = frames+4, bytes+200
		w.FramesAfter, w.BytesAfter = frames, bytes
		ev.Windows = append(ev.Windows, w)
	}
	ev.HonestLive, ev.HonestRecovered = len(ev.Windows), len(ev.Windows)
	return ev
}

// TestBoltTxTerminate_OracleCanFail is the falsifiability proof for the CONTRACT
// checker: the clean evidence passes, and every single-field perturbation of it
// is caught. An oracle that cannot fail proves nothing, so each mutation below
// names the defect it stands for.
func TestBoltTxTerminate_OracleCanFail(t *testing.T) {
	// Sequential rather than parallel: goleak.VerifyNone reports every goroutine
	// alive at teardown, including ones a concurrently running test owns. The table
	// is pure and costs microseconds.
	defer goleak.VerifyNone(t)

	clean := cleanTxTerminateEvidence()
	if v := checkBoltTxTerminate(&clean); len(v) != 0 {
		t.Fatalf("clean evidence must pass the contract, got %d violation(s): %v", len(v), v)
	}

	cases := []struct {
		name    string
		mutate  func(*BoltTxTerminateEvidence)
		wantSub string
	}{
		// ── the ledger ───────────────────────────────────────────────────────
		{
			name:    "the server minted a transaction id out of sequence",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Plan[1].IDSuffix = "-9" },
			wantSub: "sequence is per SERVER",
		},
		{
			name:    "the registry did not list every open transaction",
			mutate:  func(e *BoltTxTerminateEvidence) { e.ListedAtCap = e.ListedAtCap[1:] },
			wantSub: "want exactly",
		},
		{
			name:    "the termination took a transaction it was not given",
			mutate:  func(e *BoltTxTerminateEvidence) { e.ListedAfterTerminate = []string{"-3"} },
			wantSub: "must end the transaction it was given and NO other",
		},
		{
			name:    "the termination left the transaction it WAS given",
			mutate:  func(e *BoltTxTerminateEvidence) { e.ListedAfterTerminate = []string{"-1", "-2", "-3"} },
			wantSub: "must end the transaction it was given and NO other",
		},
		{
			name:    "the successor reused its predecessor's id",
			mutate:  func(e *BoltTxTerminateEvidence) { e.SuccessorSuffix = "-1" },
			wantSub: "only ever increases",
		},
		{
			name:    "a stale id reached the transaction opened next",
			mutate:  func(e *BoltTxTerminateEvidence) { e.SuccessorListed = false },
			wantSub: "reached the transaction the same connection opened next",
		},
		{
			name:    "the read-only bystander went with the victim",
			mutate:  func(e *BoltTxTerminateEvidence) { e.BystanderListed = false },
			wantSub: "no terminate call ever named it",
		},
		// ── the calls ────────────────────────────────────────────────────────
		{
			name:    "terminating a LIVE id was refused",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Calls[0].Got = txTermOutcomeNoSuchTx },
			wantSub: "on the live id",
		},
		{
			name:    "a stale id was ACCEPTED",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Calls[2].Got = txTermOutcomeOK },
			wantSub: "on the stale-committed id",
		},
		{
			name:    "a stale id failed with an error the contract has no name for",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Calls[3].Got = txTermOutcomeOther },
			wantSub: "on the stale-vs-successor id",
		},
		{
			name:    "the terminated connection was not told its transaction had ended",
			mutate:  func(e *BoltTxTerminateEvidence) { e.VictimCode = "" },
			wantSub: "an absent entry alone does not prove a termination",
		},
		{
			name: "the termination's failure text changed under the arm",
			mutate: func(e *BoltTxTerminateEvidence) {
				e.VictimMessage = "the transaction was terminated by an operator"
			},
			wantSub: "txTerminateFailureMessage",
		},
		{
			// THE regression rmp #2560 fixed, replayed exactly: the operator path
			// answering with the DEADLINE reason. The generic mismatch above would
			// catch it, but only as "some other string"; this case asserts the arm
			// names what actually went wrong, because a reader who sees a bare
			// mismatch has no way to know the two reasons have been collapsed back
			// into one.
			name: "the operator path borrowed the deadline reason again",
			mutate: func(e *BoltTxTerminateEvidence) {
				e.VictimCode, e.VictimMessage = txReapFailureCode, txReapFailureMessage
			},
			wantSub: "borrowing it again",
		},
		// ── the residue ──────────────────────────────────────────────────────
		{
			name:    "a terminated transaction's uncommitted write survived",
			mutate:  func(e *BoltTxTerminateEvidence) { e.GhostsLive = 1 },
			wantSub: "survived the rollback",
		},
		{
			name:    "an uncommitted write reached the durable log",
			mutate:  func(e *BoltTxTerminateEvidence) { e.GhostsRecovered = 1 },
			wantSub: "reached the durable log",
		},
		{
			name:    "the termination appended a WAL frame",
			mutate:  func(e *BoltTxTerminateEvidence) { e.TermFrames = 1 },
			wantSub: "a rollback must append neither",
		},
		{
			name:    "the termination appended WAL bytes",
			mutate:  func(e *BoltTxTerminateEvidence) { e.TermBytes = 64 },
			wantSub: "a rollback must append neither",
		},
		{
			name: "an unrelated writer was refused while transactions sat open",
			mutate: func(e *BoltTxTerminateEvidence) {
				e.Windows[1].Committed = false
				e.HonestLive, e.HonestRecovered = 1, 1
			},
			wantSub: "must not stop an unrelated writer",
		},
		{
			name:    "an acknowledged honest write never reached the log",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Windows[0].FramesAfter = e.Windows[0].FramesBefore },
			wantSub: "appended NO WAL frame",
		},
		{
			name:    "an honest write is missing from the live engine",
			mutate:  func(e *BoltTxTerminateEvidence) { e.HonestLive-- },
			wantSub: "one per honest window",
		},
		{
			name:    "an acknowledged honest write did not survive recovery",
			mutate:  func(e *BoltTxTerminateEvidence) { e.HonestRecovered-- },
			wantSub: "acknowledged live",
		},
		{
			name:    "the transaction that COMMITTED beside the terminated one lost its write",
			mutate:  func(e *BoltTxTerminateEvidence) { e.CommittedLive = 0 },
			wantSub: "must keep its write",
		},
		{
			name:    "a committed write did not survive recovery",
			mutate:  func(e *BoltTxTerminateEvidence) { e.CommittedRecovered = 0 },
			wantSub: "acknowledged live",
		},
		// ── the clock seam ───────────────────────────────────────────────────
		{
			name:    "the server registered a ticker that eats advances",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Tickers = 1 },
			wantSub: "ticker",
		},
		{
			name:    "a timer was armed without measuring the deadline",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Untils-- },
			wantSub: "exactly once per arming",
		},
		{
			name: "a BEGIN armed no reaper at all",
			mutate: func(e *BoltTxTerminateEvidence) {
				e.TimersArmed--
				e.Untils--
			},
			wantSub: "want one each",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanTxTerminateEvidence()
			tc.mutate(&ev)
			v := checkBoltTxTerminate(&ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the oracle cannot detect it", tc.name)
			}
			if !anyViolationMentions(v, tc.wantSub) {
				t.Errorf("perturbation %q: no violation mentions %q; got %v", tc.name, tc.wantSub, v)
			}
		})
	}
}

// TestBoltTxTerminate_NonVacuityCanFail proves the non-vacuity gate is itself
// falsifiable — above all its central clause, that the arm advanced the fake
// clock zero times. Without that, every departure the arm attributes to the
// operator could have been a reaper's work.
func TestBoltTxTerminate_NonVacuityCanFail(t *testing.T) {
	defer goleak.VerifyNone(t) // see TestBoltTxTerminate_OracleCanFail on parallelism

	clean := cleanTxTerminateEvidence()
	if v := checkBoltTxTerminateNonVacuity(&clean); len(v) != 0 {
		t.Fatalf("clean evidence must pass non-vacuity, got %d violation(s): %v", len(v), v)
	}

	cases := []struct {
		name    string
		mutate  func(*BoltTxTerminateEvidence)
		wantSub string
	}{
		{
			name:    "the arm advanced the clock, so a reaper could have done the work",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Advances = 1 },
			wantSub: "every clause that attributes one to TerminateTransaction collapses",
		},
		{
			name:    "the idle bound was brought within the arm's reach",
			mutate:  func(e *BoltTxTerminateEvidence) { e.IdleBound = time.Second },
			wantSub: "EARLIER of the two",
		},
		{
			name:    "the total bound was brought within the arm's reach",
			mutate:  func(e *BoltTxTerminateEvidence) { e.TotalBound = time.Second },
			wantSub: "EARLIER of the two",
		},
		{
			name:    "the SetClock seam never reached the session",
			mutate:  func(e *BoltTxTerminateEvidence) { e.TimersArmed, e.Nows = 0, 0 },
			wantSub: "a reaper that was never armed",
		},
		{
			name:    "the harness never polled the clock at all",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Nows = 0 },
			wantSub: "a reaper that was never armed",
		},
		{
			name: "the ledger silently shrank, leaving no bystander",
			mutate: func(e *BoltTxTerminateEvidence) {
				e.Plan = e.Plan[:1]
			},
			wantSub: "no bystander",
		},
		{
			name:    "the transactions were never all open together",
			mutate:  func(e *BoltTxTerminateEvidence) { e.RegistryPeak = 1 },
			wantSub: "not all open together",
		},
		{
			name:    "a terminate call stopped being made",
			mutate:  func(e *BoltTxTerminateEvidence) { e.Calls = e.Calls[1:] },
			wantSub: "terminate call(s) were made",
		},
		{
			name: "no call was made against a LIVE id",
			mutate: func(e *BoltTxTerminateEvidence) {
				for i := range e.Calls {
					e.Calls[i].Want = txTermOutcomeNoSuchTx
				}
			},
			wantSub: "half a claim",
		},
		{
			name: "no call was made against a stale id",
			mutate: func(e *BoltTxTerminateEvidence) {
				for i := range e.Calls {
					e.Calls[i].Want = txTermOutcomeOK
				}
			},
			wantSub: "half a claim",
		},
		{
			name:    "the terminated connection was never probed",
			mutate:  func(e *BoltTxTerminateEvidence) { e.VictimCode = "" },
			wantSub: "never probed",
		},
		{
			name: "no honest window appended a WAL frame",
			mutate: func(e *BoltTxTerminateEvidence) {
				for i := range e.Windows {
					e.Windows[i].FramesAfter = e.Windows[i].FramesBefore
				}
			},
			wantSub: "not a live instrument",
		},
		{
			name:    "no explicit transaction on this server ever wrote anything",
			mutate:  func(e *BoltTxTerminateEvidence) { e.CommittedLive = 0 },
			wantSub: "could not have written",
		},
		{
			name: "the ledger holds only writing transactions",
			mutate: func(e *BoltTxTerminateEvidence) {
				for i := range e.Plan {
					e.Plan[i].Mode = "w"
				}
			},
			wantSub: "with only one kind",
		},
		{
			name: "the ledger holds only read-only transactions",
			mutate: func(e *BoltTxTerminateEvidence) {
				for i := range e.Plan {
					e.Plan[i].Mode = "r"
				}
			},
			wantSub: "with only one kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanTxTerminateEvidence()
			tc.mutate(&ev)
			v := checkBoltTxTerminateNonVacuity(&ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the gate cannot detect it", tc.name)
			}
			if !anyViolationMentions(v, tc.wantSub) {
				t.Errorf("perturbation %q: no violation mentions %q; got %v", tc.name, tc.wantSub, v)
			}
		})
	}
}

// TestBoltTxTerminate_ControlWrongTargetIsCaught is the LIVE control: it drives
// the same real server through the same real wire and changes exactly one thing —
// which transaction the terminate call is aimed at.
//
// It is what separates "TerminateTransaction ended the transaction it was given"
// from "TerminateTransaction ended a transaction". A perturbation table proves
// the checker reacts to a field; only a control proves the field is measuring the
// server. Four independent clauses must fire here, and each one is named, because
// a control that fired only ONE could be passing for an unrelated reason.
func TestBoltTxTerminate_ControlWrongTargetIsCaught(t *testing.T) {
	defer goleak.VerifyNone(t)

	opts := defaultBoltTxTerminateOptions()
	opts.TargetRole = txTermRoleBystander // the ONLY change

	ev, err := runBoltTxTerminate(context.Background(), txTerminateDefaultSeed, &opts)
	if err != nil {
		t.Fatalf("control run: %v", err)
	}
	v := checkBoltTxTerminate(ev)
	for _, want := range []string{
		"must end the transaction it was given and NO other",
		"no terminate call ever named it",
		"on the stale-terminated id",
		"an absent entry alone does not prove a termination",
	} {
		if !anyViolationMentions(v, want) {
			t.Errorf("aiming the termination at the bystander produced no violation mentioning %q; got %v\n%s",
				want, v, ev.String())
		}
	}
	// The arm's own geometry must be untouched by the control: it changed the
	// TARGET, not the number of BEGINs, the clock, or what a rollback leaves.
	if ev.Advances != 0 {
		t.Errorf("the control advanced the clock %d time(s); it is meant to change the target alone", ev.Advances)
	}
	if ev.GhostsLive != 0 || ev.GhostsRecovered != 0 {
		t.Errorf("the control left %d live and %d recovered ghost(s); terminating a different transaction must not "+
			"change what a rollback leaves behind", ev.GhostsLive, ev.GhostsRecovered)
	}
	t.Logf("termination aimed at the %s: %d contract violation(s)", opts.TargetRole, len(v))
}
