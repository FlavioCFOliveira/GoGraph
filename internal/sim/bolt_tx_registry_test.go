package sim

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestBoltTxFakeClockAdvance_DeliversOnDeadlineEquality pins the one property of
// [clock.Fake] the whole reap arithmetic rests on: an Advance that lands EXACTLY on
// a waiter's deadline delivers to it.
//
// advanceLocked skips a waiter only when w.deadline.After(target)
// (internal/clock/fake.go), so equality fires — but that is a one-character
// difference from a strict comparison, and if it were `!Before` instead then every
// predicted reap ordinal in [idleReapModel] would be one advance early and each arm
// would fail for a reason having nothing to do with the Bolt server. Measuring it
// here means a change to the fake is caught in the fake.
//
// It doubles as the proof that [txClockProbe.probeTimer] is UNCOUNTED: the timer it
// registers receives the advance while the counter the arms use as their arming
// barrier stays at zero.
func TestBoltTxFakeClockAdvance_DeliversOnDeadlineEquality(t *testing.T) {
	defer goleak.VerifyNone(t)

	const d = 100 * time.Millisecond
	probe := newTxClockProbe()
	timer := probe.probeTimer(d)
	defer timer.Stop()

	select {
	case at := <-timer.C():
		t.Fatalf("the timer fired at %s before any advance", at)
	default:
	}

	probe.Advance(d)

	select {
	case <-timer.C():
	default:
		t.Fatalf("clock.Fake.Advance(%s) did not deliver to a waiter whose deadline was exactly %s away; "+
			"every reap ordinal idleReapModel predicts assumes it does", d, d)
	}
	if got := probe.Timers(); got != 0 {
		t.Errorf("probeTimer registered %d counted timers; it must be uncounted so the arming barrier stays attributable", got)
	}
	if got := probe.Tickers(); got != 0 {
		t.Errorf("probeTimer registered %d tickers; want 0", got)
	}
}

// TestBoltTxIdleReapModel_PredictsTheDocumentedRule pins the harness's OWN model of
// the idle-reap rule, so an arm's comparison against it is a real check. A model
// that predicted the same ordinal for every transaction, or one that never
// predicted a reap, would make an arm's correspondence clause vacuous while looking
// identical from the outside.
//
// Every expectation here is hand-computed from the rule — deadline = last message +
// MaxTxIdle, reaped by the first advance that REACHES it — and none is transcribed
// from an observed run. The model is written before any run uses it precisely so
// that the ordinals are a prediction.
func TestBoltTxIdleReapModel_PredictsTheDocumentedRule(t *testing.T) {
	defer goleak.VerifyNone(t)

	const (
		idle = 100 * time.Millisecond
		step = 25 * time.Millisecond
	)

	// Three transactions opened before any advance all fall due together, a whole
	// idle window (four steps) away.
	together := &idleReapModel{MaxTxIdle: idle, Step: step, Offsets: []time.Duration{0, 0, 0}}
	if want := []int{4, 4, 4}; !equalInts(together.reapOrdinals(), want) {
		t.Errorf("%s; want ordinals %v", together, want)
	}
	if got, want := together.openAfter(3), 3; got != want {
		t.Errorf("after 3 advances the model leaves %d of 3 open; want %d", got, want)
	}
	if got, want := together.openAfter(4), 0; got != want {
		t.Errorf("after 4 advances the model leaves %d of 3 open; want %d", got, want)
	}

	// Staggered opens: two advances happened during setup, and each transaction was
	// last touched one step later than the previous one. The ordinals must be
	// DISTINCT — a plan whose transactions all die together cannot tell a correct
	// reaper from one that reaps the whole registry on the first fire.
	staggered := &idleReapModel{
		MaxTxIdle: idle,
		Step:      step,
		Elapsed:   2 * step,
		Offsets:   []time.Duration{0, step, 2 * step},
	}
	if want := []int{2, 3, 4}; !equalInts(staggered.reapOrdinals(), want) {
		t.Errorf("%s; want ordinals %v", staggered, want)
	}
	for n, want := range map[int]int{0: 3, 1: 3, 2: 2, 3: 1, 4: 0} {
		if got := staggered.openAfter(n); got != want {
			t.Errorf("after %d advances the staggered model leaves %d of 3 open; want %d", n, got, want)
		}
	}

	// The equality clause: a single step landing exactly on the deadline reaps. A
	// model built on a strict inequality would say 2 here, and every arm that
	// advanced by one whole MaxTxIdle would then look like a defect.
	exact := &idleReapModel{MaxTxIdle: idle, Step: idle, Offsets: []time.Duration{0}}
	if want := []int{1}; !equalInts(exact.reapOrdinals(), want) {
		t.Errorf("%s; want ordinals %v (an advance of exactly MaxTxIdle reaps)", exact, want)
	}

	// A step that does not divide the window: three steps of 30ms fall short of
	// 100ms, so the reap is the fourth.
	ragged := &idleReapModel{MaxTxIdle: idle, Step: 30 * time.Millisecond, Offsets: []time.Duration{0}}
	if want := []int{4}; !equalInts(ragged.reapOrdinals(), want) {
		t.Errorf("%s; want ordinals %v", ragged, want)
	}

	// A deadline already behind the start of the measured sequence still costs ONE
	// advance: syncTxTimer clamps a negative duration to zero and a waiter due now
	// is delivered by the next advance, not retroactively.
	behind := &idleReapModel{MaxTxIdle: idle, Step: step, Elapsed: 2 * idle, Offsets: []time.Duration{0}}
	if want := []int{1}; !equalInts(behind.reapOrdinals(), want) {
		t.Errorf("%s; want ordinals %v", behind, want)
	}

	// A non-positive step advances nothing, so nothing is ever reaped. This is the
	// model refusing to predict a reap a plan cannot produce.
	still := &idleReapModel{MaxTxIdle: idle, Step: 0, Offsets: []time.Duration{0, step}}
	if want := []int{reapNever, reapNever}; !equalInts(still.reapOrdinals(), want) {
		t.Errorf("%s; want ordinals %v", still, want)
	}
	if got, want := still.openAfter(1_000), 2; got != want {
		t.Errorf("with a zero step the model leaves %d of 2 open after 1000 advances; want %d", got, want)
	}
}

// TestBoltTxClockProbe_RendezvousIsExact proves the barrier every reaper arm will
// depend on: the transaction-timeout timer is armed on the INJECTED clock, exactly
// one of them exists per open transaction, further traffic on a still clock does not
// replace it, and one advance of exactly MaxTxIdleTime reaps.
//
// The reason it has to be proved rather than assumed is that the two signals an arm
// would naturally use as its barrier are both too early: handleBegin registers the
// transaction (so Transactions() lists it) and the response loop FLUSHES the
// SUCCESS, and only then does syncTxTimer arm the timer. See the file comment in
// bolt_tx_registry.go for the exact ordering and the line numbers.
func TestBoltTxClockProbe_RendezvousIsExact(t *testing.T) {
	defer goleak.VerifyNone(t)

	probe := newTxClockProbe()
	// The listener keeps REAL time while the server runs on the probe. Sharing one
	// fake between them would make every parked connection register a timer of its
	// own — see NewSimServerTxRegistry.
	srv, err := NewSimServerTxRegistry(SimEngineForServer(), clock.Real(), probe,
		txRegistryMaxTxIdle, txRegistryTxTimeout, 0)
	if err != nil {
		t.Fatalf("NewSimServerTxRegistry: %v", err)
	}
	defer func() {
		if cerr := srv.Close(); cerr != nil {
			t.Errorf("SimServer.Close: %v", cerr)
		}
	}()

	cli, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cli.Close() }()
	if err := cli.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Before any BEGIN the reaper has nothing to arm: syncTxTimer's second branch is
	// a no-op while txTimer is nil, so this reads 0 wherever the message loop
	// happens to be. It is what makes the count below attributable to the BEGIN
	// rather than to the handshake.
	if got := probe.Timers(); got != 0 {
		t.Fatalf("%d timers were registered on the server clock before any BEGIN; want 0", got)
	}
	// The clock is not read AT ALL before a transaction is open, which was MEASURED
	// here rather than assumed: the first version of this test required a Now call
	// during the handshake and failed. touchTx runs before every message but returns
	// at `if !s.txActive` WITHOUT reading the clock (bolt/server/session.go:759),
	// syncTxTimer's second branch is a no-op with txTimer nil, and reportTx returns
	// early while txID is empty — so HELLO and LOGON touch the seam zero times. That
	// makes every count below attributable to the transaction, and it is the reason
	// the SetClock seam cannot be confirmed until the BEGIN.
	if got := probe.Nows(); got != 0 {
		t.Fatalf("the server made %d Now calls on the injected clock before any BEGIN; want 0", got)
	}

	if resp, berr := cli.Begin(); berr != nil {
		t.Fatalf("BEGIN: %v", berr)
	} else if _, ok := resp.(*proto.Success); !ok {
		t.Fatalf("BEGIN returned %T (%+v); want *proto.Success", resp, resp)
	}

	// Sampled the instant the client has the SUCCESS in hand. The arming is code-
	// ordered AFTER the flush, so this is a RACE the client cannot win reliably:
	// whichever way it lands, the count can never exceed one, and the value is
	// logged rather than asserted because asserting either outcome would be
	// asserting the scheduler.
	armedAtSuccess := probe.Timers()
	if armedAtSuccess > 1 {
		t.Errorf("%d timers were armed for a single BEGIN; want at most 1", armedAtSuccess)
	}

	// The listing is available at or before this point — earlier than the SUCCESS in
	// the message loop's order — which is exactly why it cannot serve as the arming
	// barrier.
	if txs := srv.Server().Transactions(); len(txs) != 1 {
		t.Fatalf("Transactions() lists %d open transactions after one BEGIN; want 1", len(txs))
	}
	txID := srv.Server().Transactions()[0].ID

	// THE barrier: the timer registration itself.
	if werr := waitForTxRegistry(func() bool { return probe.Timers() >= 1 },
		"the server never registered a transaction-timeout timer on the injected clock after BEGIN"); werr != nil {
		t.Fatalf("%v", werr)
	}
	if got := probe.Timers(); got != 1 {
		t.Fatalf("the server registered %d timers for one open transaction; want EXACTLY 1 "+
			"(the reap ordinals every arm predicts assume one waiter per transaction)", got)
	}
	if got, want := probe.Untils(), probe.Timers(); got != want {
		t.Errorf("Until was called %d times against %d timer registrations; syncTxTimer calls "+
			"clk.Until exactly once per arming, so these must match", got, want)
	}
	// Now the seam IS confirmed: handleBegin derives the total deadline from it,
	// touchTx the idle one, and the registry stamps StartedAt with it.
	if got := probe.Nows(); got == 0 {
		t.Fatalf("the server made no Now call on the injected clock for an open transaction; " +
			"the SetClock seam is not reaching the session")
	}
	t.Logf("timers already registered when the client read BEGIN's SUCCESS: %d of 1 "+
		"(arming is code-ordered after the flush, so 0 and 1 are both legitimate; "+
		"this is why SUCCESS is not the barrier)", armedAtSuccess)

	// Further traffic on a STILL clock must not re-arm. touchTx recomputes the idle
	// deadline as clk.Now()+maxTxIdle — the same instant, because the fake has not
	// moved — and syncTxTimer early-returns on at.Equal(txTimerAt).
	nowsBefore := probe.Nows()
	for i := range 3 {
		runResp, rerr := cli.Run("RETURN 1", nil)
		if rerr != nil {
			t.Fatalf("RUN %d inside the transaction: %v", i, rerr)
		}
		if _, ok := runResp.(*proto.Success); !ok {
			t.Fatalf("RUN %d returned %T (%+v); want *proto.Success", i, runResp, runResp)
		}
		records, terminal, perr := cli.PullAll()
		if perr != nil {
			t.Fatalf("PULL %d: %v", i, perr)
		}
		if _, ok := terminal.(*proto.Success); !ok {
			t.Fatalf("PULL %d terminated with %T (%+v); want *proto.Success", i, terminal, terminal)
		}
		if len(records) != 1 {
			t.Fatalf("PULL %d returned %d records; want 1", i, len(records))
		}
	}

	// The registry update is the barrier that proves the server ran syncTxTimer for
	// the messages above: reportTx is called immediately AFTER it in the message
	// loop, so a listing that shows the RUN's query and a settled TX_READY state
	// cannot precede the arming decision for that message.
	if werr := waitForTxRegistry(func() bool {
		txs := srv.Server().Transactions()
		return len(txs) == 1 && txs[0].Query == "RETURN 1" && txs[0].State == "TX_READY"
	}, "the registry never reported the RUN's query with the session settled back in TX_READY"); werr != nil {
		t.Fatalf("%v (listing: %+v)", werr, srv.Server().Transactions())
	}
	if got := probe.Timers(); got != 1 {
		t.Errorf("three further round trips on a still clock left %d timers registered; want 1 "+
			"(touchTx recomputes the SAME instant, so syncTxTimer must early-return)", got)
	}
	if got := probe.Nows(); got <= nowsBefore {
		t.Errorf("the Now count did not move across three round trips (%d -> %d); "+
			"touchTx reads the clock once per message, so the messages did not reach the seam", nowsBefore, got)
	}

	// The rendezvous. The model PREDICTED this ordinal before the run: one step of
	// exactly MaxTxIdle reaps a transaction opened at offset zero on the first
	// advance.
	model := &idleReapModel{MaxTxIdle: txRegistryMaxTxIdle, Step: txRegistryMaxTxIdle, Offsets: []time.Duration{0}}
	if want := []int{1}; !equalInts(model.reapOrdinals(), want) {
		t.Fatalf("%s; the plan below advances once, so the model must predict %v", model, want)
	}

	probe.Advance(txRegistryMaxTxIdle)
	if werr := waitForTxRegistry(func() bool { return len(srv.Server().Transactions()) == 0 },
		"the idle reaper never removed the transaction after ONE advance of exactly MaxTxIdleTime"); werr != nil {
		t.Fatalf("%v (timers=%d listing=%+v)", werr, probe.Timers(), srv.Server().Transactions())
	}

	// Attribution: the transaction left the registry because it was REAPED, not
	// because the connection died or something rolled it back. reapTimedOutTx arms a
	// typed FAILURE for the next request-phase message, and only the reaper does.
	failResp, rerr := cli.Run("RETURN 1", nil)
	if rerr != nil {
		t.Fatalf("RUN after the reap: %v", rerr)
	}
	fail, ok := failResp.(*proto.Failure)
	if !ok {
		t.Fatalf("RUN after the reap returned %T (%+v); want *proto.Failure", failResp, failResp)
	}
	if want := "Neo.ClientError.Transaction.TransactionTimedOut"; fail.Code != want {
		t.Errorf("RUN after the reap failed with %q (%q); want %q", fail.Code, fail.Message, want)
	}

	// The reap consumed the timer and armed no replacement: the txTimerC branch
	// clears txTimer inline and never calls syncTxTimer.
	if got := probe.Timers(); got != 1 {
		t.Errorf("after the reap %d timers had been registered; want 1", got)
	}
	if got := probe.Tickers(); got != 0 {
		t.Errorf("the server registered %d tickers on the injected clock; want 0 "+
			"(a ticker would consume advances the reap ordinals depend on)", got)
	}
	// The id captured from the listing is gone, so a later TerminateTransaction for
	// it cannot reach whatever transaction the connection opens next.
	if terr := srv.Server().TerminateTransaction(txID); terr == nil {
		t.Errorf("TerminateTransaction(%q) succeeded after the reap; want ErrNoSuchTransaction", txID)
	}
}

// -----------------------------------------------------------------------------
// The abandoned-registry arm
// -----------------------------------------------------------------------------

// txAbandonDefaultSeed is the seed the clean run and the live controls use.
const txAbandonDefaultSeed = 0x2482_ABA1

// TestBoltTxRegistryAbandoned_Clean drives the whole arm against a real
// WAL-backed server on virtual time and asserts both the contract and the
// non-vacuity gate are satisfied.
//
// It also pins the arm's GEOMETRY, because every other clause is graded against
// it: five transactions staggered one step apart under a ten-step idle bound must
// give the reap ordinals 6..10, which leaves five ordinals at the front of the
// measured sequence at which the reaper must decline to reap. A geometry change
// that quietly removed the quiet ordinals, or that made the five transactions
// fall due together, would leave every other assertion looking identical while
// the run stopped distinguishing a correct reaper from a crude one.
func TestBoltTxRegistryAbandoned_Clean(t *testing.T) {
	defer goleak.VerifyNone(t)

	start := time.Now()
	ev, err := RunBoltTxRegistryAbandoned(context.Background(), txAbandonDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltTxRegistryAbandoned: %v", err)
	}
	elapsed := time.Since(start)

	if want := []int{6, 7, 8, 9, 10}; !equalInts(ev.PredictedReapOrdinals, want) {
		t.Errorf("the arm's geometry predicts reap ordinals %v, want %v (idle %s, step %s, %d transactions "+
			"staggered one step apart)", ev.PredictedReapOrdinals, want, ev.IdleBound, ev.Step, txAbandonCount)
	}
	if ev.Advances != 10 {
		t.Errorf("the measured sequence made %d advances, want 10 (the last predicted reap)", ev.Advances)
	}
	for _, v := range checkBoltTxRegistry(ev) {
		t.Errorf("contract violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	for _, v := range checkBoltTxRegistryNonVacuity(ev) {
		t.Errorf("non-vacuity violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	t.Logf("arm wall time %s (the %s of simulated time it advanced cost none of it)", elapsed, ev.SimElapsed)
	if t.Failed() {
		t.Log("\n" + ev.String())
	}
}

// TestBoltTxRegistryAbandoned_Deterministic asserts one seed produces one run.
//
// The decisive comparison is of the RENDERED evidence, because
// [BoltTxRegistryEvidence.String] deliberately renders only what is seed-pure: no
// raw transaction id (the prefix is crypto/rand), no Now count (it counts how
// often the harness polled the registry, which is scheduling-dependent), and no
// honest-window byte total (the frame embeds a node key drawn from a
// process-global counter, so its WIDTH depends on how many nodes the rest of the
// process created first — the limitation the auth-surface arm measured at 183 vs
// 186 bytes for the same frame). Everything else is compared, and the two
// excluded quantities are asserted to be PRESENT rather than equal, so a field
// that silently stopped being collected is still caught.
func TestBoltTxRegistryAbandoned_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const seed = 0x5EED_2482
	first, err := RunBoltTxRegistryAbandoned(context.Background(), seed)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunBoltTxRegistryAbandoned(context.Background(), seed)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("two runs of seed %d rendered differently:\n--- first ---\n%s\n--- second ---\n%s",
			seed, first.String(), second.String())
	}
	// Field by field as well as rendered, so a divergence names the field instead of
	// leaving two twenty-line blocks to be diffed by eye.
	if first.TimersAtRendezvous != second.TimersAtRendezvous || first.TimersTotal != second.TimersTotal ||
		first.Untils != second.Untils || first.Tickers != second.Tickers {
		t.Errorf("clock counters diverged: first(timers %d/%d untils %d tickers %d) second(timers %d/%d untils %d tickers %d)",
			first.TimersAtRendezvous, first.TimersTotal, first.Untils, first.Tickers,
			second.TimersAtRendezvous, second.TimersTotal, second.Untils, second.Tickers)
	}
	if first.SetupElapsed != second.SetupElapsed || first.SimElapsed != second.SimElapsed ||
		first.RendezvousAt != second.RendezvousAt || first.Advances != second.Advances {
		t.Errorf("geometry diverged: first=%+v second=%+v", first, second)
	}
	if !equalInts(first.PredictedReapOrdinals, second.PredictedReapOrdinals) ||
		!equalInts(first.ObservedReapOrdinals, second.ObservedReapOrdinals) ||
		!equalInts(first.ProbeFireOrdinals, second.ProbeFireOrdinals) ||
		!equalInts(first.SettleTimeoutOrdinals, second.SettleTimeoutOrdinals) {
		t.Errorf("ordinals diverged: first(pred %v obs %v fires %v timeouts %v) second(pred %v obs %v fires %v timeouts %v)",
			first.PredictedReapOrdinals, first.ObservedReapOrdinals, first.ProbeFireOrdinals, first.SettleTimeoutOrdinals,
			second.PredictedReapOrdinals, second.ObservedReapOrdinals, second.ProbeFireOrdinals, second.SettleTimeoutOrdinals)
	}
	if first.RegistryPeak != second.RegistryPeak || first.Accepted != second.Accepted ||
		first.Refused != second.Refused || first.Reaped != second.Reaped {
		t.Errorf("registry counters diverged: first=%+v second=%+v", first, second)
	}
	if first.ReapFrames != second.ReapFrames || first.ReapBytes != second.ReapBytes {
		t.Errorf("reap WAL bracket diverged: first(frames %d bytes %d) second(frames %d bytes %d)",
			first.ReapFrames, first.ReapBytes, second.ReapFrames, second.ReapBytes)
	}
	if first.SnapshotsBaseline != second.SnapshotsBaseline || first.SnapshotsPeak != second.SnapshotsPeak ||
		first.SnapshotsFinal != second.SnapshotsFinal {
		t.Errorf("snapshot occupancy diverged: first=%d/%d/%d second=%d/%d/%d",
			first.SnapshotsBaseline, first.SnapshotsPeak, first.SnapshotsFinal,
			second.SnapshotsBaseline, second.SnapshotsPeak, second.SnapshotsFinal)
	}
	if first.GhostsLive != second.GhostsLive || first.GhostsRecovered != second.GhostsRecovered ||
		first.HonestLive != second.HonestLive || first.HonestRecovered != second.HonestRecovered {
		t.Errorf("censuses diverged: first=%+v second=%+v", first, second)
	}
	if len(first.Plan) != len(second.Plan) {
		t.Fatalf("plan lengths differ: %d vs %d", len(first.Plan), len(second.Plan))
	}
	for i := range first.Plan {
		a, b := &first.Plan[i], &second.Plan[i]
		if *a != *b {
			t.Errorf("plan row %d diverged:\n first=%+v\nsecond=%+v", i, *a, *b)
		}
	}
	if len(first.Listing) != len(second.Listing) {
		t.Fatalf("listing lengths differ: %d vs %d", len(first.Listing), len(second.Listing))
	}
	for i := range first.Listing {
		a, b := &first.Listing[i], &second.Listing[i]
		if *a != *b {
			t.Errorf("listing row %d diverged:\n first=%+v\nsecond=%+v", i, *a, *b)
		}
	}
	// The fields String() renders are covered above; these are the ones it reduces
	// to a boolean or omits, compared where they ARE reproducible.
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
				"reap bracket being zero", a.Name, a.bytesAppended(), b.bytesAppended())
		}
	}
	if first.Nows == 0 || second.Nows == 0 {
		t.Errorf("the server made %d then %d Now call(s) on the injected clock; the seam must be reached in both runs",
			first.Nows, second.Nows)
	}
}

// TestBoltTxRegistryAbandoned_ExactListing is the clause the DST adds over the
// pre-existing bolt/server introspection tests, asserted directly on a live run
// rather than only through the adjudicator.
//
// bolt/server/tx_introspection_test.go can assert no more than `Elapsed > 0`,
// because it runs on the wall clock and cannot know what the server's own clock
// read when the entry was registered. Here the harness owns every advance, and
// the arming barrier guarantees no advance is ever in flight while a transaction
// registers, so the two instants are EXACT: StartedAt is the fake instant the
// harness opened that transaction at, and Elapsed is the listing instant minus
// that, to the nanosecond.
func TestBoltTxRegistryAbandoned_ExactListing(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunBoltTxRegistryAbandoned(context.Background(), txAbandonDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltTxRegistryAbandoned: %v", err)
	}
	if len(ev.Listing) != len(ev.Plan) || len(ev.Plan) != txAbandonCount {
		t.Fatalf("listed %d of %d planned transactions, want %d of %d",
			len(ev.Listing), len(ev.Plan), txAbandonCount, txAbandonCount)
	}
	for i := range ev.Listing {
		got, want := &ev.Listing[i], &ev.Plan[i]
		if got.StartedAt != want.OpenedAt {
			t.Errorf("entry %s: StartedAt = %s, want exactly %s (the fake instant the harness opened it at)",
				got.IDSuffix, got.StartedAt, want.OpenedAt)
		}
		if wantElapsed := ev.RendezvousAt - want.OpenedAt; got.Elapsed != wantElapsed {
			t.Errorf("entry %s: Elapsed = %s, want exactly %s (%s at the listing minus %s at the BEGIN)",
				got.IDSuffix, got.Elapsed, wantElapsed, ev.RendezvousAt, want.OpenedAt)
		}
		if got.Elapsed <= 0 && want.OpenedAt != ev.RendezvousAt {
			t.Errorf("entry %s: Elapsed = %s is not positive for a transaction opened before the listing",
				got.IDSuffix, got.Elapsed)
		}
	}
	// The distinctness the whole exactness claim rests on: if every transaction had
	// been stamped with the same instant, equality above would hold for a registry
	// that stamped them all at once, and the oldest-first order would be arbitrary.
	seen := make(map[time.Duration]bool, len(ev.Listing))
	for i := range ev.Listing {
		if seen[ev.Listing[i].StartedAt] {
			t.Errorf("two entries share StartedAt %s: the stagger did not reach the registry",
				ev.Listing[i].StartedAt)
		}
		seen[ev.Listing[i].StartedAt] = true
	}
}

// cleanTxRegistryEvidence returns a hand-built evidence value that both checkers
// pass, so a perturbation test can attribute any violation to the field it
// changed. Its shape mirrors the arm's nominal geometry, and
// TestBoltTxRegistryAbandoned_Clean pins that geometry on a real run, so the two
// cannot drift apart silently.
func cleanTxRegistryEvidence() BoltTxRegistryEvidence {
	const step = txAbandonStep
	ev := BoltTxRegistryEvidence{
		Arm: boltTxArmAbandoned, Seed: 1,
		TimersAtRendezvous: txAbandonCount, TimersTotal: txAbandonCount,
		Untils: txAbandonCount, Tickers: 0, Nows: 137,
		IdleBound: txAbandonIdleBound, TotalBound: txAbandonTotalBound,
		ModelIdleBound: txAbandonIdleBound, Step: step,
		SetupElapsed: 4 * step, SimElapsed: 14 * step, RendezvousAt: 4 * step,
		Advances:     10,
		RegistryPeak: txAbandonCount, Accepted: txAbandonCount, Reaped: txAbandonCount,
		PredictedReapOrdinals: []int{6, 7, 8, 9, 10},
		ObservedReapOrdinals:  []int{6, 7, 8, 9, 10},
		ProbeFireOrdinals:     []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		SnapshotsPeak:         txAbandonCount,
	}
	// Both modes present: the read pins a horizon slot without writing, the writes
	// leave the uncommitted ghosts the atomicity clause hunts for.
	modes := []string{"r", "w", "w", "w", "r"}
	ev.Plan = make([]BoltTxPlanRow, 0, len(modes))
	ev.Listing = make([]BoltTxListingRow, 0, len(modes))
	ev.ListingOrder = make([]string, 0, len(modes))
	for k, mode := range modes {
		var (
			principal = fmt.Sprintf("tx-principal-%d", k)
			suffix    = fmt.Sprintf("-%d", k+1)
			query     = abandonQuery(mode, k)
			openedAt  = time.Duration(k) * step
		)
		ev.Plan = append(ev.Plan, BoltTxPlanRow{
			Principal: principal, Mode: mode, IDSuffix: suffix, Query: query,
			OpenedAt: openedAt, Conn: k,
		})
		ev.Listing = append(ev.Listing, BoltTxListingRow{
			IDSuffix: suffix, Principal: principal, Mode: mode,
			Remote: txAbandonRemote, State: txAbandonReadyState, Query: query,
			StartedAt: openedAt, Elapsed: ev.RendezvousAt - openedAt,
		})
		ev.ListingOrder = append(ev.ListingOrder, suffix)
	}
	// The four honest windows, each appending a frame, positioned where their names
	// claim: none open, all open, part-way through the reap, none left.
	openAt := map[string]int{
		txWindowBefore: 0, txWindowAtCap: txAbandonCount, txWindowDuring: 2, txWindowAfter: 0,
	}
	ordinalAt := map[string]int{
		txWindowBefore: 0, txWindowAtCap: 0, txWindowDuring: 8, txWindowAfter: 10,
	}
	ev.Windows = make([]BoltTxWriteWindow, 0, len(txAbandonWindows))
	var frames, bytes uint64
	for _, name := range txAbandonWindows {
		w := BoltTxWriteWindow{
			Name: name, Node: "honest-" + name, Committed: true,
			OpenTx: openAt[name], Ordinal: ordinalAt[name],
			FramesBefore: frames, BytesBefore: bytes,
		}
		frames, bytes = frames+4, bytes+200
		w.FramesAfter, w.BytesAfter = frames, bytes
		ev.Windows = append(ev.Windows, w)
	}
	ev.HonestLive, ev.HonestRecovered = len(ev.Windows), len(ev.Windows)
	// Every reaped connection was told the reaper's typed FAILURE, which is what
	// attributes its absence from the registry to the reap.
	ev.ReapCodes = make([]string, 0, len(ev.Plan))
	ev.ReapMessages = make([]string, 0, len(ev.Plan))
	for range ev.Plan {
		ev.ReapCodes = append(ev.ReapCodes, txReapFailureCode)
		ev.ReapMessages = append(ev.ReapMessages, txReapFailureMessage)
	}
	return ev
}

// TestBoltTxRegistry_OracleCanFail is the falsifiability proof for the CONTRACT
// checker: the clean evidence passes, and every single-field perturbation of it
// is caught. An oracle that cannot fail proves nothing, so each mutation below
// names the defect it stands for.
func TestBoltTxRegistry_OracleCanFail(t *testing.T) {
	// Sequential rather than parallel: goleak.VerifyNone reports every goroutine
	// alive at teardown, including ones a concurrently running test owns, so the two
	// cannot be combined. The table is pure and costs microseconds, so sequential
	// costs nothing and keeps the leak check honest.
	defer goleak.VerifyNone(t)

	clean := cleanTxRegistryEvidence()
	if v := checkBoltTxRegistry(&clean); len(v) != 0 {
		t.Fatalf("clean evidence must pass the contract, got %d violation(s): %v", len(v), v)
	}

	cases := []struct {
		name    string
		mutate  func(*BoltTxRegistryEvidence)
		wantSub string
	}{
		// ── the clock seam ───────────────────────────────────────────────────
		{
			name:    "the reaper armed no timer for one open transaction",
			mutate:  func(e *BoltTxRegistryEvidence) { e.TimersAtRendezvous-- },
			wantSub: "one waiter per open transaction",
		},
		{
			name:    "a transaction re-armed a replacement timer",
			mutate:  func(e *BoltTxRegistryEvidence) { e.TimersTotal++ },
			wantSub: "re-armed or replacement timer",
		},
		{
			name:    "a timer was armed without measuring the deadline",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Untils-- },
			wantSub: "exactly once per arming",
		},
		{
			name:    "the server registered a ticker that eats advances",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Tickers = 1 },
			wantSub: "consumes advances",
		},
		// ── the listing ──────────────────────────────────────────────────────
		{
			name:    "the registry dropped an open transaction from the listing",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Listing = e.Listing[1:] },
			wantSub: "one per BEGIN",
		},
		{
			name:    "the listing names a principal that opened nothing",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Listing[2].Principal = "somebody-else" },
			wantSub: "opened no transaction in the plan",
		},
		{
			name: "the listing is not oldest-first",
			mutate: func(e *BoltTxRegistryEvidence) {
				e.ListingOrder[0], e.ListingOrder[1] = e.ListingOrder[1], e.ListingOrder[0]
			},
			wantSub: "oldest first",
		},
		{
			name:    "an entry carries the wrong id",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Listing[1].IDSuffix = "-99" },
			wantSub: "reports id=",
		},
		{
			name:    "a read transaction is reported as a writer",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Listing[0].Mode = "w" },
			wantSub: "reports mode=",
		},
		{
			name:    "the listing forgot what the transaction is running",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Listing[1].Query = "" },
			wantSub: "reports query=",
		},
		{
			name:    "the listing lost the client address",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Listing[3].Remote = "" },
			wantSub: "reports remote=",
		},
		{
			name:    "the listing reports the wrong session state",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Listing[2].State = "TX_STREAMING" },
			wantSub: "reports state=",
		},
		{
			name:    "StartedAt is not the instant the BEGIN completed",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Listing[2].StartedAt += time.Nanosecond },
			wantSub: "reports startedAt=",
		},
		{
			name:    "Elapsed contradicts the instant it was stamped with",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Listing[4].Elapsed = time.Second },
			wantSub: "reports elapsed=",
		},
		// ── the reaper ───────────────────────────────────────────────────────
		{
			name:    "a transaction was reaped an advance early",
			mutate:  func(e *BoltTxRegistryEvidence) { e.ObservedReapOrdinals[3]-- },
			wantSub: "the idle rule predicts",
		},
		{
			name: "an abandoned transaction was never reaped at all",
			mutate: func(e *BoltTxRegistryEvidence) {
				e.ObservedReapOrdinals[4] = reapNever
				e.Reaped--
			},
			wantSub: "the idle rule predicts",
		},
		{
			name:    "one advance never reached a waiter",
			mutate:  func(e *BoltTxRegistryEvidence) { e.ProbeFireOrdinals = e.ProbeFireOrdinals[1:] },
			wantSub: "a tick that never arrived",
		},
		{
			name:    "the registry never shrank to the predicted size",
			mutate:  func(e *BoltTxRegistryEvidence) { e.SettleTimeoutOrdinals = []int{7} },
			wantSub: "never fell to the size the model predicts",
		},
		{
			name:    "a transaction is still holding its slot at the end",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Reaped-- },
			wantSub: "still holding their slot",
		},
		{
			name:    "a connection was not told its transaction had been reaped",
			mutate:  func(e *BoltTxRegistryEvidence) { e.ReapCodes[2] = "" },
			wantSub: "absence alone does not prove a reap",
		},
		{
			name:    "the reaper's failure text changed under the arm",
			mutate:  func(e *BoltTxRegistryEvidence) { e.ReapMessages[1] = "the transaction was idle for too long" },
			wantSub: "txReapFailureMessage",
		},
		{
			name: "no connection was probed after the reap",
			mutate: func(e *BoltTxRegistryEvidence) {
				e.ReapCodes, e.ReapMessages = nil, nil
			},
			wantSub: "cannot say WHY they left the registry",
		},
		{
			name:    "a BEGIN was refused although no cap is installed",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Refused = 1 },
			wantSub: "no per-principal cap",
		},
		// ── the residue ──────────────────────────────────────────────────────
		{
			name:    "a reaped transaction's uncommitted write survived",
			mutate:  func(e *BoltTxRegistryEvidence) { e.GhostsLive = 1 },
			wantSub: "survived the rollback",
		},
		{
			name:    "an uncommitted write reached the durable log",
			mutate:  func(e *BoltTxRegistryEvidence) { e.GhostsRecovered = 1 },
			wantSub: "survived WAL replay",
		},
		{
			name:    "the reap appended a WAL frame",
			mutate:  func(e *BoltTxRegistryEvidence) { e.ReapFrames = 1 },
			wantSub: "a rollback must append neither",
		},
		{
			name:    "the reap appended WAL bytes",
			mutate:  func(e *BoltTxRegistryEvidence) { e.ReapBytes = 64 },
			wantSub: "a rollback must append neither",
		},
		{
			name:    "a reaped transaction is still pinning the horizon",
			mutate:  func(e *BoltTxRegistryEvidence) { e.SnapshotsFinal = 1 },
			wantSub: "still pinning version memory",
		},
		{
			name: "an unrelated writer was refused while transactions sat idle",
			mutate: func(e *BoltTxRegistryEvidence) {
				e.Windows[1].Committed = false
				e.HonestLive, e.HonestRecovered = 3, 3
			},
			wantSub: "must not stop an unrelated writer",
		},
		{
			name:    "an acknowledged honest write never reached the log",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Windows[2].FramesAfter = e.Windows[2].FramesBefore },
			wantSub: "appended NO WAL frame",
		},
		{
			name:    "an honest write is missing from the live engine",
			mutate:  func(e *BoltTxRegistryEvidence) { e.HonestLive-- },
			wantSub: "one per honest window",
		},
		{
			name:    "an acknowledged honest write did not survive recovery",
			mutate:  func(e *BoltTxRegistryEvidence) { e.HonestRecovered-- },
			wantSub: "acknowledged live",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanTxRegistryEvidence()
			tc.mutate(&ev)
			v := checkBoltTxRegistry(&ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the oracle cannot detect it", tc.name)
			}
			if !anyViolationMentions(v, tc.wantSub) {
				t.Errorf("perturbation %q: no violation mentions %q; got %v", tc.name, tc.wantSub, v)
			}
		})
	}
}

// TestBoltTxRegistry_NonVacuityCanFail proves the non-vacuity gate is itself
// falsifiable: a run that opened the wrong number of transactions, never used
// both access modes, never had them all open together, left no quiet ordinal, or
// never moved one of the two instruments its clauses read must be reported rather
// than pass quietly.
func TestBoltTxRegistry_NonVacuityCanFail(t *testing.T) {
	defer goleak.VerifyNone(t) // see TestBoltTxRegistry_OracleCanFail on parallelism

	clean := cleanTxRegistryEvidence()
	if v := checkBoltTxRegistryNonVacuity(&clean); len(v) != 0 {
		t.Fatalf("clean evidence must pass non-vacuity, got %d violation(s): %v", len(v), v)
	}

	cases := []struct {
		name    string
		mutate  func(*BoltTxRegistryEvidence)
		wantSub string
	}{
		{
			name: "the plan silently shrank",
			mutate: func(e *BoltTxRegistryEvidence) {
				e.Plan = e.Plan[1:]
				e.Listing = e.Listing[1:]
				e.ListingOrder = e.ListingOrder[1:]
			},
			wantSub: "grades a smaller registry",
		},
		{
			name: "no transaction was opened read-only",
			mutate: func(e *BoltTxRegistryEvidence) {
				for i := range e.Plan {
					e.Plan[i].Mode = "w"
				}
			},
			wantSub: "no transaction was opened READ-ONLY",
		},
		{
			name: "no transaction was opened for writing",
			mutate: func(e *BoltTxRegistryEvidence) {
				for i := range e.Plan {
					e.Plan[i].Mode = "r"
				}
			},
			wantSub: "no transaction was opened for WRITING",
		},
		{
			name: "nothing was accepted, so the listing oracle graded nothing",
			mutate: func(e *BoltTxRegistryEvidence) {
				e.Accepted, e.Listing, e.ListingOrder = 0, nil, nil
			},
			wantSub: "has nothing to adjudicate",
		},
		{
			name:    "the transactions were never all open together",
			mutate:  func(e *BoltTxRegistryEvidence) { e.RegistryPeak-- },
			wantSub: "not all open together",
		},
		{
			name:    "the measured sequence made no advance",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Advances = 0 },
			wantSub: "no advance at all",
		},
		{
			name:    "every transaction falls due on the same advance",
			mutate:  func(e *BoltTxRegistryEvidence) { e.PredictedReapOrdinals = []int{6, 6, 6, 6, 6} },
			wantSub: "not all distinct",
		},
		{
			name: "no ordinal at which the reaper had to decline",
			mutate: func(e *BoltTxRegistryEvidence) {
				e.Advances = 5
				e.PredictedReapOrdinals = []int{1, 2, 3, 4, 5}
			},
			wantSub: "never asserts that the reaper DECLINED",
		},
		{
			name:    "no transaction ever left the registry",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Reaped = 0 },
			wantSub: "never had an event to grade",
		},
		{
			name:    "the total-lifetime bound would reap before the idle one",
			mutate:  func(e *BoltTxRegistryEvidence) { e.TotalBound = e.IdleBound },
			wantSub: "measured the total-lifetime reaper",
		},
		{
			name:    "the SetClock seam never reached the session",
			mutate:  func(e *BoltTxRegistryEvidence) { e.TimersTotal, e.Nows = 0, 0 },
			wantSub: "every virtual-time clause is inert",
		},
		{
			name:    "the harness never polled the clock at all",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Nows = 0 },
			wantSub: "every virtual-time clause is inert",
		},
		{
			name:    "the horizon instrument never moved",
			mutate:  func(e *BoltTxRegistryEvidence) { e.SnapshotsPeak = e.SnapshotsBaseline },
			wantSub: "an instrument that never moved",
		},
		{
			name:    "an honest window stopped running",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Windows = e.Windows[1:] },
			wantSub: "not bracketed by writes on both sides",
		},
		{
			name: "no honest window appended a WAL frame",
			mutate: func(e *BoltTxRegistryEvidence) {
				for i := range e.Windows {
					e.Windows[i].FramesAfter = e.Windows[i].FramesBefore
				}
			},
			wantSub: "not a live instrument",
		},
		{
			name:    "the at-cap window ran with the registry half empty",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Windows[1].OpenTx = 2 },
			wantSub: "want all of them",
		},
		{
			name:    "the during-reap window ran outside the reap sequence",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Windows[2].OpenTx = 0 },
			wantSub: "sit INSIDE the reap sequence",
		},
		{
			name:    "the before window ran with transactions already open",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Windows[0].OpenTx = 1 },
			wantSub: "already open, want 0",
		},
		{
			name:    "the after window left a transaction open",
			mutate:  func(e *BoltTxRegistryEvidence) { e.Windows[3].OpenTx = 1 },
			wantSub: "still open, want 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanTxRegistryEvidence()
			tc.mutate(&ev)
			v := checkBoltTxRegistryNonVacuity(&ev)
			if len(v) == 0 {
				t.Fatalf("perturbation %q produced NO violation: the gate cannot detect it", tc.name)
			}
			if !anyViolationMentions(v, tc.wantSub) {
				t.Errorf("perturbation %q: no violation mentions %q; got %v", tc.name, tc.wantSub, v)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Live controls
// -----------------------------------------------------------------------------
//
// Both drive a REAL server through the real wire and vary exactly one thing about
// the arm, so what they falsify is the harness's own instrument rather than a
// doctored struct. A perturbation table proves the checker reacts to a field; a
// live control proves the field is measuring the server.

// TestBoltTxRegistry_ControlArmingBarrierIsLoadBearing is the control for the
// timer-count rendezvous: it makes the fake clock move at the one instant the
// barrier exists to forbid, and the reap ordinals collapse.
//
// # Why it is driven by a hook rather than by dropping the wait
//
// The obvious form of this control — do not wait for the timer count, just
// advance as soon as BEGIN's SUCCESS is in hand — does not reliably reproduce
// anything, and this was MEASURED rather than assumed. syncTxTimer runs a handful
// of instructions after the response is flushed (bolt/server/serve.go:1350), while
// the client still has to be scheduled, read the pipe, and decode the message
// before the harness can call Advance. The server therefore wins the race nearly
// every time, the un-barriered run passes cleanly, and a test asserting a
// deviation would be red for the wrong reason.
//
// So the control drives the hazard directly: [boltTxAbandonOptions.beforeArming]
// moves the clock ONE FULL IDLE WINDOW between syncTxTimer choosing the absolute
// deadline and its measuring how far away that deadline is. syncTxTimer then
// computes a non-positive duration, clamps it to zero (serve.go:1179), and
// registers a waiter for the fake's CURRENT instant — after Advance has already
// walked the waiter list, so the fire it should have received is gone and the very
// next advance delivers it instead. The measured result: a transaction the plan
// says must be reaped at ordinal 10 is reaped at ordinal 1.
//
// # Why the plan is one silent transaction
//
// Silent, because ANY later message re-enters syncTxTimer, and on a clock that has
// moved since the BEGIN the idle deadline is no longer the armed one, so the timer
// is rebuilt for a fresh instant. That was measured too: with the transaction's
// RUN left in, the second arming restored the original reap ordinal by accident
// and only the timer COUNT (2 for one transaction) and the listing's Elapsed
// betrayed the corruption. One transaction, because the arm staggers its opens by
// advancing between them, and a plan whose first transaction has already been
// reaped by the hook's advance cannot reach its own next barrier.
func TestBoltTxRegistry_ControlArmingBarrierIsLoadBearing(t *testing.T) {
	defer goleak.VerifyNone(t)

	opts := defaultBoltTxAbandonOptions()
	opts.Count = 1
	opts.Silent = true
	var once sync.Once
	// Runs on the SERVER's message-loop goroutine, inside syncTxTimer. clock.Fake
	// is mutex-guarded and holds no lock across the Until call, so this is safe.
	opts.beforeArming = func(p *txClockProbe) {
		once.Do(func() { p.Advance(opts.ServerIdleBound) })
	}

	ev, err := runBoltTxAbandoned(context.Background(), txAbandonDefaultSeed, &opts)
	if err != nil {
		t.Fatalf("control run: %v", err)
	}
	// The arming COUNT is still correct — one timer for one transaction — so this
	// control cannot pass by breaking a different clause.
	if got, want := ev.TimersAtRendezvous, int64(1); got != want {
		t.Errorf("the control armed %d timer(s) for %d transaction(s), want %d: it is meant to corrupt WHEN the "+
			"timer is armed for, not HOW MANY are armed", got, len(ev.Plan), want)
	}
	if equalInts(ev.ObservedReapOrdinals, ev.PredictedReapOrdinals) {
		t.Errorf("a clock that moved inside the arming still produced the predicted reap ordinals %v: the timer-count "+
			"rendezvous is not load-bearing, or the hook did not reach syncTxTimer\n%s",
			ev.PredictedReapOrdinals, ev.String())
	}
	v := checkBoltTxRegistry(ev)
	if !anyViolationMentions(v, "the idle rule predicts") {
		t.Errorf("the contract checker did not report the shifted reap: got %v\n%s", v, ev.String())
	}
	t.Logf("arming barrier bypassed: predicted %v, observed %v (%d violation(s))",
		ev.PredictedReapOrdinals, ev.ObservedReapOrdinals, len(v))
}

// TestBoltTxRegistry_ControlShorterIdleBoundBreaksTheOrdinals is the control for
// the reap-ordinal clause itself: it installs a MaxTxIdleTime one step shorter
// than the one the harness's model predicts from, changing nothing else.
//
// Every transaction then falls due one advance early, and the predicted ordinals
// must stop matching. Without this, "observed equals predicted" could be true of a
// harness that derived the prediction from the observation — the correspondence
// would be a transcription rather than a check. It is the live counterpart of
// TestBoltTxIdleReapModel_PredictsTheDocumentedRule, which pins the model in
// isolation.
func TestBoltTxRegistry_ControlShorterIdleBoundBreaksTheOrdinals(t *testing.T) {
	defer goleak.VerifyNone(t)

	opts := defaultBoltTxAbandonOptions()
	opts.ServerIdleBound = txAbandonIdleBound - txAbandonStep // the model keeps the nominal bound

	ev, err := runBoltTxAbandoned(context.Background(), txAbandonDefaultSeed, &opts)
	if err != nil {
		t.Fatalf("control run: %v", err)
	}
	if equalInts(ev.ObservedReapOrdinals, ev.PredictedReapOrdinals) {
		t.Fatalf("an idle bound one step shorter still produced the predicted reap ordinals %v: the observed ordinals "+
			"are not a measurement of the server's own bound\n%s", ev.PredictedReapOrdinals, ev.String())
	}
	// Precisely one step early, everywhere: the deviation is the bound, not noise.
	want := make([]int, len(ev.PredictedReapOrdinals))
	for i, ord := range ev.PredictedReapOrdinals {
		want[i] = ord - 1
	}
	if !equalInts(ev.ObservedReapOrdinals, want) {
		t.Errorf("with the idle bound shortened by one step the reaps landed at %v, want %v (one advance earlier "+
			"than the nominal %v)", ev.ObservedReapOrdinals, want, ev.PredictedReapOrdinals)
	}
	if v := checkBoltTxRegistry(ev); !anyViolationMentions(v, "the idle rule predicts") {
		t.Errorf("the contract checker did not report the shifted reap: got %v\n%s", v, ev.String())
	}
	// Everything else about the run must still be sound: the control changed the
	// reaper's bound, not the registry's fidelity or the rollback's atomicity.
	if ev.GhostsLive != 0 || ev.GhostsRecovered != 0 {
		t.Errorf("the control left %d live and %d recovered ghost(s); shortening the idle bound must not change what "+
			"a rollback leaves behind", ev.GhostsLive, ev.GhostsRecovered)
	}
	t.Logf("idle bound %s against a model built on %s: predicted %v, observed %v",
		ev.IdleBound, ev.ModelIdleBound, ev.PredictedReapOrdinals, ev.ObservedReapOrdinals)
}
