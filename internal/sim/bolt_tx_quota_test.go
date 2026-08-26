package sim

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// txQuotaDefaultSeed is the seed the clean run and the live control use.
const txQuotaDefaultSeed = 0x2482_9074

// TestBoltTxQuota_Clean drives the whole per-principal quota arm against a real
// WAL-backed server on virtual time and asserts both the contract and the
// non-vacuity gate are satisfied.
//
// It also pins the arm's GEOMETRY, because every other clause depends on it: a
// cap of at least two is what makes a refusal distinguishable from a BEGIN that
// never works, and the roster of six accepted and two refused BEGINs is what
// makes the three reclamation routes each have something to reclaim.
func TestBoltTxQuota_Clean(t *testing.T) {
	defer goleak.VerifyNone(t)

	start := time.Now()
	ev, err := RunBoltTxQuota(context.Background(), txQuotaDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltTxQuota: %v", err)
	}
	elapsed := time.Since(start)

	if ev.Limit < 2 {
		t.Errorf("the arm installed a cap of %d: below 2 a refusal cannot be told apart from a BEGIN that never "+
			"works at all", ev.Limit)
	}
	if got, want := len(ev.Begins), len(txQuotaAccepted)+len(txQuotaRefused); got != want {
		t.Errorf("the arm sent %d BEGIN(s), want %d (%d that must be accepted, %d that must be refused)",
			got, want, len(txQuotaAccepted), len(txQuotaRefused))
	}
	if want := []int{txQuotaAdvances, reapNever, reapNever}; !equalInts(ev.PredictedReapOrdinals, want) {
		t.Errorf("the arm's geometry predicts reap ordinals %v, want %v: exactly ONE slot must be freed by the "+
			"reaper, with the two touched transactions left open", ev.PredictedReapOrdinals, want)
	}
	for _, v := range checkBoltTxQuota(ev) {
		t.Errorf("contract violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	for _, v := range checkBoltTxQuotaNonVacuity(ev) {
		t.Errorf("non-vacuity violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	t.Logf("arm wall time %s (the %s of simulated time it advanced cost none of it)",
		elapsed, time.Duration(ev.Advances)*ev.Step)
	if t.Failed() {
		t.Log("\n" + ev.String())
	}
}

// TestBoltTxQuota_RefusalNamesWhoAndHowMany asserts, directly on a live run, the
// clause this arm adds over bolt/server/abandoned_tx_test.go.
//
// That file's TestAbandonedTx_PerPrincipalCapRejectsWithTypedError checks the
// failure CODE and that the message is non-empty. A code-only assertion is
// equally satisfied by a server that refused the wrong principal, or refused the
// right one at the wrong count, or refused for a different limit that happens to
// share the code. Here the harness RECOMPUTES the text from the principal and the
// cap it configured, and the server's own text must match it exactly — which is
// sound precisely because handleBegin returns the quota error VERBATIM rather
// than through Session.sanitiseErr (bolt/server/session.go:1604).
func TestBoltTxQuota_RefusalNamesWhoAndHowMany(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunBoltTxQuota(context.Background(), txQuotaDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltTxQuota: %v", err)
	}
	want := txQuotaRefusalMessage(txQuotaPrincipalA, txQuotaLimit)
	if ev.WantRefusalMessage != want {
		t.Fatalf("the arm recomputed %q, want %q", ev.WantRefusalMessage, want)
	}
	for _, phase := range txQuotaRefused {
		b := ev.beginFor(phase)
		if b == nil {
			t.Errorf("the run never drove the %q BEGIN", phase)
			continue
		}
		if b.Accepted {
			t.Errorf("the BEGIN in phase %q was ACCEPTED although %q already held its cap of %d",
				phase, txQuotaPrincipalA, txQuotaLimit)
			continue
		}
		if b.Code != txQuotaRefusalCode {
			t.Errorf("phase %q was refused with code %q, want %q", phase, b.Code, txQuotaRefusalCode)
		}
		if b.Message != want {
			t.Errorf("phase %q read %q, want %q recomputed from the principal and the cap", phase, b.Message, want)
		}
	}
	// The control that makes the refusals mean "per principal" rather than
	// "per server".
	other := ev.beginFor(txQuotaPhaseOther)
	if other == nil || !other.Accepted {
		t.Errorf("the second principal was not served while the first was at its cap (%+v): the cap would then be a "+
			"server-wide limit", other)
	}
	// rmp #2561, pinned as OBSERVED: the quota branch returns before Transition
	// and never enters FAILED, so the refused connection is still READY.
	if !ev.PostRefusalAccepted {
		t.Errorf("the refused connection was answered %q (%q) to its next statement instead of serving it; if the "+
			"quota branch now enters FAILED, rmp #2561 has been closed and this arm must be updated with the ticket",
			ev.PostRefusalCode, ev.PostRefusalMessage)
	}
}

// TestBoltTxQuota_SlotsComeBackByThreeRoutes asserts, on a live run, that each of
// the three reclamation routes genuinely returns a slot.
//
// bolt/server/abandoned_tx_test.go covers the fourth (a client ROLLBACK). None of
// the other three was tested at all, and the third — a de-authorised session
// whose COMMIT is refused — is the clause rmp #2482 carries over from the #2481
// security review: the auth scenario's WAL-frame and ghost-node oracles are
// satisfied whether or not the refusal RECLAIMED the transaction, because a
// refusal that skipped enterFailed writes nothing either.
func TestBoltTxQuota_SlotsComeBackByThreeRoutes(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunBoltTxQuota(context.Background(), txQuotaDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltTxQuota: %v", err)
	}
	for _, phase := range []string{txQuotaPhaseAfterReap, txQuotaPhaseAfterTerminate, txQuotaPhaseAfterLogoff} {
		b := ev.beginFor(phase)
		if b == nil {
			t.Errorf("the run never drove the %q BEGIN", phase)
			continue
		}
		if !b.Accepted {
			t.Errorf("the BEGIN in phase %q was refused (%s: %s): the slot the preceding step freed did not come back",
				phase, b.Code, b.Message)
		}
	}
	// Each route's own witness, so a BEGIN that succeeded for an unrelated reason
	// cannot stand in for the reclamation.
	if want := []int{txQuotaAdvances, reapNever, reapNever}; !equalInts(ev.ObservedReapOrdinals, want) {
		t.Errorf("the idle reaper reclaimed %v, want %v: it must take the UNTOUCHED transaction and leave the two "+
			"that were touched", ev.ObservedReapOrdinals, want)
	}
	if ev.ReapCode != txReapFailureCode {
		t.Errorf("the reaped connection was answered %q, want %q: only the reaper arms that failure, so without it "+
			"the freed slot could equally have been a rollback", ev.ReapCode, txReapFailureCode)
	}
	if ev.TerminateOutcome != txTermOutcomeOK || !ev.TerminatedGone {
		t.Errorf("the operator termination returned %s and left the entry gone=%t", ev.TerminateOutcome, ev.TerminatedGone)
	}
	if !ev.LogoffEntryGone {
		t.Errorf("the de-authorised session's transaction was still listed after its COMMIT was refused: the refusal " +
			"declined the message without reclaiming the transaction, which no WAL or census oracle can see")
	}
	if ev.GhostsLive != 0 || ev.GhostsRecovered != 0 {
		t.Errorf("the write staged before the LOGOFF survived: %d live, %d after recovery",
			ev.GhostsLive, ev.GhostsRecovered)
	}
}

// TestBoltTxQuota_Deterministic asserts one seed produces one run. See
// TestBoltTxTerminate_Deterministic for why the rendered evidence is the decisive
// comparison and which two quantities are deliberately excluded from it.
func TestBoltTxQuota_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const seed = 0x5EED_9074
	first, err := RunBoltTxQuota(context.Background(), seed)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunBoltTxQuota(context.Background(), seed)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("two runs of seed %d rendered differently:\n--- first ---\n%s\n--- second ---\n%s",
			seed, first.String(), second.String())
	}
	if first.TimersTotal != second.TimersTotal || first.Untils != second.Untils ||
		first.Tickers != second.Tickers || first.Touches != second.Touches {
		t.Errorf("clock counters diverged: first(timers %d untils %d tickers %d touches %d) "+
			"second(timers %d untils %d tickers %d touches %d)",
			first.TimersTotal, first.Untils, first.Tickers, first.Touches,
			second.TimersTotal, second.Untils, second.Tickers, second.Touches)
	}
	if len(first.Begins) != len(second.Begins) {
		t.Fatalf("BEGIN ledgers differ in length: %d vs %d", len(first.Begins), len(second.Begins))
	}
	for i := range first.Begins {
		if a, b := &first.Begins[i], &second.Begins[i]; *a != *b {
			t.Errorf("BEGIN row %d diverged:\n first=%+v\nsecond=%+v", i, *a, *b)
		}
	}
	if !equalInts(first.PredictedReapOrdinals, second.PredictedReapOrdinals) ||
		!equalInts(first.ObservedReapOrdinals, second.ObservedReapOrdinals) ||
		!equalInts(first.ReapProbeFires, second.ReapProbeFires) {
		t.Errorf("ordinals diverged: first(pred %v obs %v fires %v) second(pred %v obs %v fires %v)",
			first.PredictedReapOrdinals, first.ObservedReapOrdinals, first.ReapProbeFires,
			second.PredictedReapOrdinals, second.ObservedReapOrdinals, second.ReapProbeFires)
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
			t.Errorf("window %q appended %d then %d WAL byte(s); a window that writes nothing is not an instrument",
				a.Name, a.bytesAppended(), b.bytesAppended())
		}
	}
	if first.Nows == 0 || second.Nows == 0 {
		t.Errorf("the server made %d then %d Now call(s) on the injected clock; the seam must be reached in both runs",
			first.Nows, second.Nows)
	}
}

// cleanTxQuotaEvidence returns a hand-built evidence value that both checkers
// pass, so a perturbation test can attribute any violation to the field it
// changed. Its shape mirrors the arm's nominal geometry, and
// TestBoltTxQuota_Clean pins that geometry on a real run, so the two cannot
// drift apart silently.
func cleanTxQuotaEvidence() BoltTxQuotaEvidence {
	refusal := txQuotaRefusalMessage(txQuotaPrincipalA, txQuotaLimit)
	ev := BoltTxQuotaEvidence{
		Arm: boltTxArmQuota, Seed: 1,
		Limit: txQuotaLimit, PrincipalA: txQuotaPrincipalA, PrincipalB: txQuotaPrincipalB,
		IdleBound: txQuotaIdleBound, TotalBound: txQuotaTotalBound, Step: txQuotaStep,
		Advances: txQuotaAdvances, Touches: 2,
		TimersTotal: 8, WantTimers: 8, Untils: 8, Tickers: 0, Nows: 137,
		CapModes: []string{"r", "w"}, RegistryAtCap: txQuotaLimit,
		WantRefusalMessage:    refusal,
		PostRefusalAccepted:   true,
		PredictedReapOrdinals: []int{txQuotaAdvances, reapNever, reapNever},
		ObservedReapOrdinals:  []int{txQuotaAdvances, reapNever, reapNever},
		ReapProbeFires:        []int{1, 2},
		ReapCode:              txReapFailureCode,
		ReapMessage:           txReapFailureMessage,
		TerminateOutcome:      txTermOutcomeOK,
		TerminatedGone:        true,
		LogoffCommitCode:      txQuotaLogoffRefusalCode,
		LogoffCommitMessage:   "illegal message *proto.Commit in state " + txQuotaLogoffOriginState,
		LogoffEntryGone:       true,
		GhostsLive:            0,
		GhostsRecovered:       0,
	}
	// The BEGIN ledger, exactly as the nominal run produces it: the whole-server
	// registry size moves by one for every accepted BEGIN and not at all for a
	// refused one, and the capped principal sits at its limit from cap-1 onwards.
	type row struct {
		phase          string
		principal      string
		mode           string
		accepted       bool
		registryBefore int
		timersBefore   int64
		principalOpen  int
	}
	rows := []row{
		{txQuotaPhaseCap0, txQuotaPrincipalA, "r", true, 0, 0, 1},
		{txQuotaPhaseCap1, txQuotaPrincipalA, "w", true, 1, 1, 2},
		{txQuotaPhaseOverCap, txQuotaPrincipalA, "r", false, 2, 2, 2},
		{txQuotaPhaseOther, txQuotaPrincipalB, "r", true, 2, 2, 1},
		{txQuotaPhaseAfterReap, txQuotaPrincipalA, "w", true, 2, 5, 2},
		{txQuotaPhaseOverCapAgain, txQuotaPrincipalA, "r", false, 3, 6, 2},
		{txQuotaPhaseAfterTerminate, txQuotaPrincipalA, "w", true, 2, 6, 2},
		{txQuotaPhaseAfterLogoff, txQuotaPrincipalA, "r", true, 2, 7, 2},
	}
	accepted := 0
	ev.Begins = make([]BoltTxQuotaBegin, 0, len(rows))
	for _, r := range rows {
		b := BoltTxQuotaBegin{
			Phase: r.phase, Principal: r.principal, Mode: r.mode, Accepted: r.accepted,
			RegistryBefore: r.registryBefore, TimersBefore: r.timersBefore,
			PrincipalOpenAfter: r.principalOpen, WithinCeiling: true,
		}
		if r.accepted {
			accepted++
			b.IDSuffix = fmt.Sprintf("-%d", accepted)
			b.WantSuffix = b.IDSuffix
			b.RegistryAfter = r.registryBefore + 1
			b.TimersAfter = r.timersBefore + 1
		} else {
			b.Code, b.Message = txQuotaRefusalCode, refusal
			b.RegistryAfter = r.registryBefore
			b.TimersAfter = r.timersBefore
		}
		ev.Begins = append(ev.Begins, b)
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

// TestBoltTxQuota_OracleCanFail is the falsifiability proof for the CONTRACT
// checker: the clean evidence passes, and every single-field perturbation of it
// is caught. An oracle that cannot fail proves nothing, so each mutation below
// names the defect it stands for.
func TestBoltTxQuota_OracleCanFail(t *testing.T) {
	// Sequential rather than parallel: goleak.VerifyNone reports every goroutine
	// alive at teardown, including ones a concurrently running test owns.
	defer goleak.VerifyNone(t)

	clean := cleanTxQuotaEvidence()
	if v := checkBoltTxQuota(&clean); len(v) != 0 {
		t.Fatalf("clean evidence must pass the contract, got %d violation(s): %v", len(v), v)
	}

	cases := []struct {
		name    string
		mutate  func(*BoltTxQuotaEvidence)
		wantSub string
	}{
		// ── the roster ───────────────────────────────────────────────────────
		{
			name: "the cap admitted a BEGIN over the limit",
			mutate: func(e *BoltTxQuotaEvidence) {
				b := e.beginFor(txQuotaPhaseOverCap)
				b.Accepted, b.Code, b.Message = true, "", ""
			},
			wantSub: "was ACCEPTED",
		},
		{
			name: "the cap refused a BEGIN the principal had room for",
			mutate: func(e *BoltTxQuotaEvidence) {
				b := e.beginFor(txQuotaPhaseCap1)
				b.Accepted = false
			},
			wantSub: "was REFUSED",
		},
		{
			name:    "the server minted a transaction id out of sequence",
			mutate:  func(e *BoltTxQuotaEvidence) { e.beginFor(txQuotaPhaseCap1).IDSuffix = "-9" },
			wantSub: "only an ACCEPTED BEGIN consumes one",
		},
		{
			name:    "an accepted BEGIN registered no entry",
			mutate:  func(e *BoltTxQuotaEvidence) { e.beginFor(txQuotaPhaseCap0).RegistryAfter = 0 },
			wantSub: "want exactly one more",
		},
		{
			name:    "an accepted BEGIN armed no reaper",
			mutate:  func(e *BoltTxQuotaEvidence) { e.beginFor(txQuotaPhaseCap0).TimersAfter = 0 },
			wantSub: "one waiter per open transaction",
		},
		{
			name:    "an accepted BEGIN took longer than the ceiling",
			mutate:  func(e *BoltTxQuotaEvidence) { e.beginFor(txQuotaPhaseOther).WithinCeiling = false },
			wantSub: "of REAL time",
		},
		{
			name:    "the principal did not reach its cap before the refusal",
			mutate:  func(e *BoltTxQuotaEvidence) { e.RegistryAtCap = 1 },
			wantSub: "held its cap, want exactly",
		},
		// ── the refusal ──────────────────────────────────────────────────────
		{
			name:    "the refusal carried the wrong failure code",
			mutate:  func(e *BoltTxQuotaEvidence) { e.beginFor(txQuotaPhaseOverCap).Code = "Neo.ClientError.Request.Invalid" },
			wantSub: "refused with code",
		},
		{
			name: "the refusal named a different principal",
			mutate: func(e *BoltTxQuotaEvidence) {
				e.beginFor(txQuotaPhaseOverCap).Message = txQuotaRefusalMessage("somebody-else", txQuotaLimit)
			},
			wantSub: "recomputed from the principal and the cap",
		},
		{
			name: "the refusal named a different limit",
			mutate: func(e *BoltTxQuotaEvidence) {
				e.beginFor(txQuotaPhaseOverCap).Message = txQuotaRefusalMessage(txQuotaPrincipalA, 99)
			},
			wantSub: "recomputed from the principal and the cap",
		},
		{
			name:    "the refusal was a stall rather than an answer",
			mutate:  func(e *BoltTxQuotaEvidence) { e.beginFor(txQuotaPhaseOverCap).WithinCeiling = false },
			wantSub: "never by parking the client",
		},
		{
			name:    "the run never asked the cap to refuse",
			mutate:  func(e *BoltTxQuotaEvidence) { e.Begins = e.Begins[:2] },
			wantSub: "the cap was never asked to refuse",
		},
		{
			name:    "a refused BEGIN lost the principal a slot",
			mutate:  func(e *BoltTxQuotaEvidence) { e.beginFor(txQuotaPhaseOverCap).PrincipalOpenAfter = 1 },
			wantSub: "neither add a slot nor lose one",
		},
		{
			name:    "a refused BEGIN left an entry in the registry",
			mutate:  func(e *BoltTxQuotaEvidence) { e.beginFor(txQuotaPhaseOverCapAgain).RegistryAfter = 4 },
			wantSub: "exactly as it found it",
		},
		{
			name:    "a refused BEGIN armed a reaper for a transaction that does not exist",
			mutate:  func(e *BoltTxQuotaEvidence) { e.beginFor(txQuotaPhaseOverCap).TimersAfter = 3 },
			wantSub: "want ZERO",
		},
		{
			name:    "the refused session was left unable to speak",
			mutate:  func(e *BoltTxQuotaEvidence) { e.PostRefusalAccepted = false },
			wantSub: "rmp #2561",
		},
		// ── the three reclamation routes ─────────────────────────────────────
		{
			name:    "the reaper took a transaction that had been touched",
			mutate:  func(e *BoltTxQuotaEvidence) { e.ObservedReapOrdinals[1] = txQuotaAdvances },
			wantSub: "leave the two that were touched",
		},
		{
			name:    "the reaper took nothing at all",
			mutate:  func(e *BoltTxQuotaEvidence) { e.ObservedReapOrdinals[0] = reapNever },
			wantSub: "leave the two that were touched",
		},
		{
			name:    "an advance reached no waiter",
			mutate:  func(e *BoltTxQuotaEvidence) { e.ReapProbeFires = []int{1} },
			wantSub: "a tick that never arrived",
		},
		{
			name:    "the reaped connection was not told why",
			mutate:  func(e *BoltTxQuotaEvidence) { e.ReapCode = "" },
			wantSub: "could equally have been a rollback",
		},
		{
			name:    "the reaper's failure text changed under the arm",
			mutate:  func(e *BoltTxQuotaEvidence) { e.ReapMessage = "the transaction was idle for too long" },
			wantSub: "rmp #2560",
		},
		{
			name:    "the operator termination was refused",
			mutate:  func(e *BoltTxQuotaEvidence) { e.TerminateOutcome = txTermOutcomeNoSuchTx },
			wantSub: "TerminateTransaction on a live id",
		},
		{
			name:    "the terminated transaction was still listed",
			mutate:  func(e *BoltTxQuotaEvidence) { e.TerminatedGone = false },
			wantSub: "still listed after the termination settled",
		},
		{
			name:    "a de-authorised COMMIT was answered with the wrong code",
			mutate:  func(e *BoltTxQuotaEvidence) { e.LogoffCommitCode = txQuotaRefusalCode },
			wantSub: "de-authorised session was answered",
		},
		{
			name:    "the de-authorised refusal did not name the origin state",
			mutate:  func(e *BoltTxQuotaEvidence) { e.LogoffCommitMessage = "illegal message *proto.Commit in state FAILED" },
			wantSub: "cannot be attributed to the de-authorisation",
		},
		{
			name:    "the de-authorised refusal declined the message without reclaiming the transaction",
			mutate:  func(e *BoltTxQuotaEvidence) { e.LogoffEntryGone = false },
			wantSub: "only declined the message",
		},
		// ── the residue ──────────────────────────────────────────────────────
		{
			name:    "the write staged before the LOGOFF survived",
			mutate:  func(e *BoltTxQuotaEvidence) { e.GhostsLive = 1 },
			wantSub: "survived the refused COMMIT",
		},
		{
			name:    "the staged write reached the durable log",
			mutate:  func(e *BoltTxQuotaEvidence) { e.GhostsRecovered = 1 },
			wantSub: "reached the durable log",
		},
		{
			name: "an unrelated writer was refused while the principal sat at its cap",
			mutate: func(e *BoltTxQuotaEvidence) {
				e.Windows[1].Committed = false
				e.HonestLive, e.HonestRecovered = 1, 1
			},
			wantSub: "must not stop an unrelated writer",
		},
		{
			name:    "an acknowledged honest write never reached the log",
			mutate:  func(e *BoltTxQuotaEvidence) { e.Windows[0].FramesAfter = e.Windows[0].FramesBefore },
			wantSub: "appended NO WAL frame",
		},
		{
			name:    "an honest write is missing from the live engine",
			mutate:  func(e *BoltTxQuotaEvidence) { e.HonestLive-- },
			wantSub: "one per honest window",
		},
		{
			name:    "an acknowledged honest write did not survive recovery",
			mutate:  func(e *BoltTxQuotaEvidence) { e.HonestRecovered-- },
			wantSub: "acknowledged live",
		},
		// ── the clock seam ───────────────────────────────────────────────────
		{
			name:    "the timer total drifted from one per accepted BEGIN plus one per touch",
			mutate:  func(e *BoltTxQuotaEvidence) { e.TimersTotal++ },
			wantSub: "one per ACCEPTED BEGIN plus",
		},
		{
			name:    "a timer was armed without measuring the deadline",
			mutate:  func(e *BoltTxQuotaEvidence) { e.Untils-- },
			wantSub: "exactly once per arming",
		},
		{
			name:    "the server registered a ticker that eats advances",
			mutate:  func(e *BoltTxQuotaEvidence) { e.Tickers = 1 },
			wantSub: "consumes advances",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanTxQuotaEvidence()
			tc.mutate(&ev)
			v := checkBoltTxQuota(&ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the oracle cannot detect it", tc.name)
			}
			if !anyViolationMentions(v, tc.wantSub) {
				t.Errorf("perturbation %q: no violation mentions %q; got %v", tc.name, tc.wantSub, v)
			}
		})
	}
}

// TestBoltTxQuota_NonVacuityCanFail proves the non-vacuity gate is itself
// falsifiable — above all its central clause, that the cap is at least two. At a
// cap of one, "the server refuses BEGIN" and "the cap fired" produce the same
// wire trace, so a run at that geometry would grade nothing while looking
// identical from the outside.
func TestBoltTxQuota_NonVacuityCanFail(t *testing.T) {
	defer goleak.VerifyNone(t) // see TestBoltTxQuota_OracleCanFail on parallelism

	clean := cleanTxQuotaEvidence()
	if v := checkBoltTxQuotaNonVacuity(&clean); len(v) != 0 {
		t.Fatalf("clean evidence must pass non-vacuity, got %d violation(s): %v", len(v), v)
	}

	cases := []struct {
		name    string
		mutate  func(*BoltTxQuotaEvidence)
		wantSub string
	}{
		{
			name:    "the cap was lowered to one, where a refusal proves nothing",
			mutate:  func(e *BoltTxQuotaEvidence) { e.Limit = 1 },
			wantSub: "a BEGIN that never works at all",
		},
		{
			name:    "the principal never reached its cap before the refusal",
			mutate:  func(e *BoltTxQuotaEvidence) { e.CapModes = e.CapModes[:1] },
			wantSub: "grades something other than the cap",
		},
		{
			name:    "the cap was filled with only one kind of transaction",
			mutate:  func(e *BoltTxQuotaEvidence) { e.CapModes = []string{"w", "w"} },
			wantSub: "counts a READ transaction exactly as it counts a write",
		},
		{
			name: "one of the three reclamation routes stopped being driven",
			mutate: func(e *BoltTxQuotaEvidence) {
				out := e.Begins[:0]
				for i := range e.Begins {
					if e.Begins[i].Phase != txQuotaPhaseAfterLogoff {
						out = append(out, e.Begins[i])
					}
				}
				e.Begins = out
			},
			wantSub: "one of the routes by which a slot comes back",
		},
		{
			name: "the run stopped driving a refusal",
			mutate: func(e *BoltTxQuotaEvidence) {
				out := e.Begins[:0]
				for i := range e.Begins {
					if e.Begins[i].Phase != txQuotaPhaseOverCapAgain {
						out = append(out, e.Begins[i])
					}
				}
				e.Begins = out
			},
			wantSub: "one refusal cannot show a cap COUNTING",
		},
		{
			name: "no slot was ever returned",
			mutate: func(e *BoltTxQuotaEvidence) {
				for i := range e.Begins {
					if e.Begins[i].Phase != txQuotaPhaseCap0 && e.Begins[i].Phase != txQuotaPhaseCap1 {
						e.Begins[i].Accepted = false
					}
				}
			},
			wantSub: "no slot was ever returned",
		},
		{
			name: "the control ran under the capped principal itself",
			mutate: func(e *BoltTxQuotaEvidence) {
				e.beginFor(txQuotaPhaseOther).Principal = txQuotaPrincipalA
			},
			wantSub: "equally true of a server-wide limit",
		},
		{
			name:    "the reap composition never advanced the clock",
			mutate:  func(e *BoltTxQuotaEvidence) { e.Advances = 0 },
			wantSub: "moves the clock so a touch can genuinely re-arm",
		},
		{
			name:    "no transaction was touched between the advances",
			mutate:  func(e *BoltTxQuotaEvidence) { e.Touches = 0 },
			wantSub: "without the stagger",
		},
		{
			name:    "every transaction falls due on the same advance",
			mutate:  func(e *BoltTxQuotaEvidence) { e.PredictedReapOrdinals = []int{2, 2, 2} },
			wantSub: "with nothing left open",
		},
		{
			name:    "the SetClock seam never reached the session",
			mutate:  func(e *BoltTxQuotaEvidence) { e.TimersTotal, e.Nows = 0, 0 },
			wantSub: "every virtual-time clause is inert",
		},
		{
			name:    "the harness never polled the clock at all",
			mutate:  func(e *BoltTxQuotaEvidence) { e.Nows = 0 },
			wantSub: "every virtual-time clause is inert",
		},
		{
			name:    "the total-lifetime bound would reap before the idle one",
			mutate:  func(e *BoltTxQuotaEvidence) { e.TotalBound = e.IdleBound },
			wantSub: "measured the total-lifetime reaper",
		},
		{
			name: "no honest window appended a WAL frame",
			mutate: func(e *BoltTxQuotaEvidence) {
				for i := range e.Windows {
					e.Windows[i].FramesAfter = e.Windows[i].FramesBefore
				}
			},
			wantSub: "not a live instrument",
		},
		{
			name:    "nothing at all reached the engine, so the census finds nothing either way",
			mutate:  func(e *BoltTxQuotaEvidence) { e.HonestLive = 0 },
			wantSub: "a query that finds nothing at all",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanTxQuotaEvidence()
			tc.mutate(&ev)
			v := checkBoltTxQuotaNonVacuity(&ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the gate cannot detect it", tc.name)
			}
			if !anyViolationMentions(v, tc.wantSub) {
				t.Errorf("perturbation %q: no violation mentions %q; got %v", tc.name, tc.wantSub, v)
			}
		})
	}
}

// TestBoltTxQuota_ControlCapDisabledAdmitsTheOverCapBegin is the LIVE control: it
// drives the same real server through the same real wire and changes exactly one
// thing — server.Options.MaxOpenTxPerPrincipal is set NEGATIVE, which that option
// documents as disabling enforcement (bolt/server/serve.go:339).
//
// It is what pins every refusal on the cap and not on the state machine, the
// framing, or a mistake in the harness: with the cap switched off, the identical
// over-cap BEGIN must be ADMITTED. A perturbation table proves the checker reacts
// to a field; only a control proves the field is measuring the server.
func TestBoltTxQuota_ControlCapDisabledAdmitsTheOverCapBegin(t *testing.T) {
	defer goleak.VerifyNone(t)

	opts := defaultBoltTxQuotaOptions()
	opts.ServerLimit = -1 // the ONLY change; the harness still expects txQuotaLimit

	ev, err := runBoltTxQuota(context.Background(), txQuotaDefaultSeed, &opts)
	if err != nil {
		t.Fatalf("control run: %v", err)
	}
	for _, phase := range txQuotaRefused {
		b := ev.beginFor(phase)
		if b == nil {
			t.Fatalf("the control never drove the %q BEGIN", phase)
		}
		if !b.Accepted {
			t.Errorf("with MaxOpenTxPerPrincipal disabled the BEGIN in phase %q was still refused (%s: %s): the "+
				"refusals the arm records are not being produced by the cap\n%s", phase, b.Code, b.Message, ev.String())
		}
	}
	v := checkBoltTxQuota(ev)
	for _, want := range []string{
		"was ACCEPTED",
		"want \"Neo.TransientError.Transaction.MaximumTransactionLimitReached\"",
		"recomputed from the principal and the cap",
	} {
		if !anyViolationMentions(v, want) {
			t.Errorf("disabling the cap produced no violation mentioning %q; got %v", want, v)
		}
	}
	t.Logf("cap disabled: %d contract violation(s), over-cap BEGIN accepted", len(v))
}
