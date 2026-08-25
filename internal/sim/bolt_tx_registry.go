package sim

// bolt_tx_registry.go — the Bolt server's TRANSACTION REGISTRY and its idle
// reaper, driven on virtual time (rmp #2482): what server.Server.Transactions
// lists, what server.Server.TerminateTransaction ends, when
// server.Options.MaxTxIdleTime reclaims an abandoned transaction, and what
// server.Options.MaxOpenTxPerPrincipal refuses.
//
// This file holds the two pieces the arms are built on: the counting clock the
// reaper's timer is observed through ([txClockProbe]) and the independent model of
// the reap rule ([idleReapModel]). Neither drives a scenario on its own.
//
// # Why the timer needs an instrument at all
//
// The reap is armed by syncTxTimer in bolt/server/serve.go, and NOTHING the client
// can observe tells it when that has happened. The order in the message loop, read
// off the code rather than assumed, is:
//
//  1. sess.touchTx() — before dispatch, for every message (serve.go:~1305).
//  2. sess.HandleMessage → handleBegin, which sets txDeadline and txIdleDeadline
//     and calls registerTx, so the transaction is ALREADY LISTED by
//     server.Server.Transactions (bolt/server/session.go handleBegin, via
//     txRegistry.register).
//  3. the response loop writes the SUCCESS, and sendResponse FLUSHES it because it
//     is not a RECORD (serve.go:1501-1505) — so the client can read it here.
//  4. syncTxTimer() — the timer is armed HERE (serve.go:1350 → :1182).
//
// So neither "the client got SUCCESS" nor "Transactions() lists N" proves the
// reaper is armed: both are strictly earlier than the arming, and the listing is
// earlier still than the SUCCESS. An arm that advanced the fake clock on either
// signal would be advancing past a deadline no timer was yet waiting for; the
// clock would then be PAST the deadline when syncTxTimer computed
// d := s.clk.Until(at), which clamps a negative d to 0 and arms a timer for the
// fake's current instant — one that fires only on the NEXT advance. The reap would
// land an advance late and the ordinal arithmetic below would be wrong.
//
// [txClockProbe] closes that gap by counting the timer registrations. The count is
// a usable barrier only because syncTxTimer's NewTimer is the ONLY [clock.Clock]
// timer registration in bolt/server: the sole other clock uses on the server clock
// are Until at serve.go:1178, Now at session.go:763 (touchTx) and :1626 (the total
// deadline), and Now at txregistry.go:135/:174 (register/list). tls_reload.go:134
// uses time.NewTicker from the standard library, not the injected clock, so it
// cannot reach this probe. VERIFIED by exhaustive grep of bolt/server before this
// was written.
//
// # Why a later message does not re-arm the timer on a still clock
//
// touchTx sets txIdleDeadline = clk.Now().Add(maxTxIdle) (session.go:763). While
// the fake clock is NOT advancing, clk.Now() returns the same instant, so the new
// idle deadline is the same instant as the old one, and syncTxTimer's first branch
// returns early on `txTimer != nil && at.Equal(txTimerAt)` (serve.go:1170-1172)
// without stopping or rebuilding anything. A transaction can therefore be driven
// through as many messages as an arm likes without disturbing the armed deadline —
// which is what makes a single advance a deterministic reap rather than a race
// against the arm's own traffic. This is asserted, not assumed, in
// TestBoltTxClockProbe_RendezvousIsExact.
//
// # Why the reap ordinal is exact
//
// [clock.Fake.Advance] fires every waiter whose deadline is at OR BEFORE the
// target: advanceLocked skips a waiter only when `w.deadline.After(target)`
// (internal/clock/fake.go), so a deadline EQUAL to the target fires. One advance of
// exactly MaxTxIdleTime therefore reaps a transaction armed for exactly that
// instant — no slack advance is needed, and [idleReapModel] encodes the equality
// rather than a strict inequality.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// The virtual geometry of a reaper arm. These are FAKE durations: no wall time
// passes for them, and the server.Server.SetClock seam is what guarantees none
// can.
const (
	// txRegistryMaxTxIdle is the idle bound installed on the server, i.e. how much
	// FAKE time a transaction may go without a message before it is reaped.
	txRegistryMaxTxIdle = 100 * time.Millisecond
	// txRegistryTxTimeout is the TOTAL-lifetime bound installed alongside it, and
	// it is deliberately far above the idle bound. The serve loop reaps at the
	// EARLIER of the two (effectiveTxDeadline, serve.go:1157-1168), so an arm that
	// left this at the 30 s default would be timing the total bound whenever it
	// asked for an idle bound above it, and would be measuring the wrong reaper.
	txRegistryTxTimeout = 10 * time.Minute
)

// The REAL-time bounds. No virtual time is governed by either: they bound only
// how long an observer waits for a server GOROUTINE to act on what the fake clock
// has already delivered.
const (
	// txRegistryObserveTimeout bounds every wait on the server, so a regression
	// that stops arming or stops reaping fails the run instead of hanging the
	// package until the test binary's own timeout.
	txRegistryObserveTimeout = 30 * time.Second
	// txRegistryPollInterval is the granularity of those waits.
	txRegistryPollInterval = 200 * time.Microsecond
)

// -----------------------------------------------------------------------------
// The injected clock
// -----------------------------------------------------------------------------

// txClockProbe is the [clock.Clock] handed to server.Server.SetClock: a
// [clock.Fake] that COUNTS how the Bolt server uses it. The counts are what turn
// the clock seam into an observation rather than an assumption — a server that had
// kept reading wall time would register no timer and make no Now call — and the
// timer count in particular is the only barrier that tells an arm the idle reaper
// is actually armed (see the file comment for why SUCCESS and a registry listing
// are both too early).
//
// # Concurrency contract
//
// txClockProbe is safe for concurrent use: [clock.Fake] is, and the counters are
// atomics. An arm advances it from the controlling goroutine while every
// connection's message loop reads it from its own.
type txClockProbe struct {
	fake    *clock.Fake
	nows    atomic.Int64
	untils  atomic.Int64
	timers  atomic.Int64
	tickers atomic.Int64
}

// newTxClockProbe returns a counting fake clock positioned at the Unix epoch.
//
// The epoch is safe for the SERVER clock — every server-side use of it is
// self-consistent (the idle and total deadlines are both derived from Now, and the
// registry's Elapsed is Now minus its own StartedAt), so no comparison against
// wall time exists to be broken. It would NOT be safe for the LISTENER clock,
// whose deadlines come from time.Now(); see [NewSimServerTxRegistry].
func newTxClockProbe() *txClockProbe {
	return &txClockProbe{fake: clock.NewFake(txProbeEpoch())}
}

// Now reports the fake's current instant and counts the call. The server reads it
// in touchTx and handleBegin to derive the two deadlines, and in the registry to
// stamp StartedAt and compute Elapsed.
func (p *txClockProbe) Now() time.Time {
	p.nows.Add(1)
	return p.fake.Now()
}

// Since reports the fake's elapsed time since t. It is uncounted because no code
// path in bolt/server calls it (VERIFIED by grep); a count would be a permanently
// zero field pretending to be evidence.
func (p *txClockProbe) Since(t time.Time) time.Duration { return p.fake.Since(t) }

// Until reports the fake's duration until t and counts the call. syncTxTimer calls
// it once per arming to convert the absolute deadline into the timer's duration
// (serve.go:1178), so the count tracks arming attempts including the ones that go
// on to be clamped at zero.
func (p *txClockProbe) Until(t time.Time) time.Duration {
	p.untils.Add(1)
	return p.fake.Until(t)
}

// After returns a channel that fires once the fake advances at least d. It is
// routed through this type's own NewTimer rather than the fake's After so the
// registration IS counted: [clock.Fake.After] is implemented as NewTimer(d).C(),
// so a future server call to After would otherwise register a waiter on the fake
// that the timer count could not see.
func (p *txClockProbe) After(d time.Duration) <-chan time.Time { return p.NewTimer(d).C() }

// NewTimer registers a one-shot timer on the fake and counts the registration.
//
// The counter is incremented AFTER the fake has registered the waiter, so an
// observer that sees a non-zero count knows the next advance reaches it. The
// reverse order would let an arm read the count, advance, and find the waiter
// registered a moment too late to receive it.
func (p *txClockProbe) NewTimer(d time.Duration) clock.Timer {
	t := p.fake.NewTimer(d)
	p.timers.Add(1)
	return t
}

// NewTicker registers a ticker on the fake and counts the registration. bolt/server
// registers none on the injected clock (its only ticker, in tls_reload.go, uses the
// standard library), so this count is expected to stay at zero — which is asserted
// rather than assumed, so that a future ticker cannot quietly consume advances the
// reaper's ordinals depend on.
func (p *txClockProbe) NewTicker(d time.Duration) clock.Ticker {
	t := p.fake.NewTicker(d)
	p.tickers.Add(1)
	return t
}

// Advance moves the fake forward by d, delivering to every waiter the interval
// crosses — including one whose deadline is EXACTLY the target (see the file
// comment). It is called only from the controlling goroutine.
func (p *txClockProbe) Advance(d time.Duration) { p.fake.Advance(d) }

// probeTimer registers a timer on the underlying fake WITHOUT counting it, so an
// observer can wait on virtual time of its own without perturbing the count that
// attributes the single counted timer to the server's reaper.
func (p *txClockProbe) probeTimer(d time.Duration) clock.Timer { return p.fake.NewTimer(d) }

// Nows, Untils, Timers, Tickers report the server's use of the seam.
func (p *txClockProbe) Nows() int64    { return p.nows.Load() }
func (p *txClockProbe) Untils() int64  { return p.untils.Load() }
func (p *txClockProbe) Timers() int64  { return p.timers.Load() }
func (p *txClockProbe) Tickers() int64 { return p.tickers.Load() }

// -----------------------------------------------------------------------------
// The independent model of the reap rule
// -----------------------------------------------------------------------------

// reapNever is the ordinal [idleReapModel] reports for a transaction no advance in
// the plan can reap — a non-positive step, or a plan that stops short.
const reapNever = 0

// idleReapModel predicts, from the PLAN'S ARITHMETIC ALONE, the advance ordinal at
// which each open transaction must leave the registry. It is an independent
// restatement of the rule — a transaction's idle deadline is the virtual instant of
// its last message plus MaxTxIdleTime, and the reap lands on the first advance that
// reaches that instant — and it consults NOTHING from bolt/server, so comparing an
// arm's observed reap ordinals against it is a real check and not a transcription.
//
// All four inputs are virtual durations measured from ONE baseline: the fake
// clock's instant before the arm's setup phase began.
type idleReapModel struct {
	// MaxTxIdle is the server's server.Options.MaxTxIdleTime.
	MaxTxIdle time.Duration
	// Step is the size of one advance in the measured sequence. A non-positive
	// step advances nothing, so every transaction is [reapNever].
	Step time.Duration
	// Elapsed is how much virtual time the arm had already advanced when the
	// measured sequence began — the setup phase's own advances. Ordinal 1 is the
	// advance that takes the clock to Elapsed+Step.
	Elapsed time.Duration
	// Offsets carries, per open transaction, the virtual instant of that
	// transaction's LAST message, as a duration since the baseline. A transaction
	// opened before any advance and left alone has offset 0; one opened after the
	// arm advanced 3 steps, or merely touched there, has offset 3*Step.
	Offsets []time.Duration
}

// reapOrdinals returns, for each transaction in Offsets and in the same order, the
// 1-based ordinal of the advance at which it must have left the registry, or
// [reapNever] when no advance can reach its deadline.
//
// The arithmetic is exactly the rule: transaction i is due at
// Offsets[i]+MaxTxIdle, ordinal n puts the clock at Elapsed+n*Step, and the reap
// lands at the smallest n with Elapsed+n*Step >= due. The comparison is
// non-strict because [clock.Fake] delivers to a waiter whose deadline EQUALS the
// advance target, so a step that lands exactly on the deadline reaps.
//
// A deadline already at or behind Elapsed yields ordinal 1 rather than 0: the
// timer for it is armed for the fake's current instant (syncTxTimer clamps a
// negative duration to zero), and a waiter due now still needs one advance to be
// delivered.
func (m *idleReapModel) reapOrdinals() []int {
	out := make([]int, len(m.Offsets))
	for i, off := range m.Offsets {
		out[i] = m.ordinalFor(off)
	}
	return out
}

// ordinalFor is the single-transaction rule reapOrdinals applies.
func (m *idleReapModel) ordinalFor(offset time.Duration) int {
	if m.Step <= 0 {
		return reapNever
	}
	remaining := offset + m.MaxTxIdle - m.Elapsed
	if remaining <= 0 {
		return 1
	}
	// Ceiling division on the integer nanosecond counts: the reap is the first
	// advance that REACHES the deadline, and a step landing exactly on it counts.
	n := (int64(remaining) + int64(m.Step) - 1) / int64(m.Step)
	return int(n)
}

// reapOrdinalsWithin restates [idleReapModel.reapOrdinals] for a measured
// sequence that STOPS after n advances: an ordinal beyond n has not happened
// yet, so it is reported as [reapNever] — exactly what an arm that watched only
// n advances records for a transaction still open at the end.
//
// It exists because an arm may deliberately advance fewer times than the plan's
// last reap needs: the quota arm advances only far enough to free ONE slot, and
// comparing its observation against the untruncated prediction would fail on the
// transactions it never meant to reap.
func (m *idleReapModel) reapOrdinalsWithin(n int) []int {
	out := m.reapOrdinals()
	for i, ord := range out {
		if ord > n {
			out[i] = reapNever
		}
	}
	return out
}

// openAfter reports how many of Offsets' transactions are still open once n
// advances have been made. It is derived from reapOrdinals so there is exactly one
// statement of the rule, and it is what an arm polling the registry's LENGTH
// compares against.
func (m *idleReapModel) openAfter(n int) int {
	open := 0
	for _, ord := range m.reapOrdinals() {
		if ord == reapNever || ord > n {
			open++
		}
	}
	return open
}

// String renders the model's inputs and its prediction for a failure message.
func (m *idleReapModel) String() string {
	return fmt.Sprintf("idleReapModel{maxTxIdle=%s step=%s elapsed=%s offsets=%v} -> ordinals=%v",
		m.MaxTxIdle, m.Step, m.Elapsed, m.Offsets, m.reapOrdinals())
}

// -----------------------------------------------------------------------------
// Bounded waits
// -----------------------------------------------------------------------------

// waitForTxRegistry polls cond until it holds or [txRegistryObserveTimeout]
// elapses. The timeout is REAL time and bounds only how long the caller waits for
// a server goroutine to act on what the fake clock has already delivered; no
// virtual time passes here, so no reap is ever governed by it.
func waitForTxRegistry(cond func() bool, what string) error {
	deadline := time.Now().Add(txRegistryObserveTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			return fmt.Errorf("sim: bolt-tx-registry: %s (waited %s)", what, txRegistryObserveTimeout)
		}
		time.Sleep(txRegistryPollInterval)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Shared primitives (every transaction-registry arm)
// -----------------------------------------------------------------------------

// settleTxRegistry re-reads the registry [txAbandonSettlePolls] times and returns
// the last listing. See [txAbandonSettlePolls] for what the budget does and does
// not buy: it is a window in which a departure that should NOT have happened can
// still land and be recorded, never a proof that none will.
func settleTxRegistry(srv *SimServer) []server.TransactionInfo {
	var txs []server.TransactionInfo
	for range txAbandonSettlePolls {
		time.Sleep(txRegistryPollInterval)
		txs = srv.Server().Transactions()
	}
	return txs
}

// txSuffixes reduces a listing to the per-server sequence suffixes of its ids, in
// the order the registry returned them.
func txSuffixes(txs []server.TransactionInfo) []string {
	out := make([]string, 0, len(txs))
	for i := range txs {
		out = append(out, txIDSuffix(txs[i].ID))
	}
	return out
}

// txSortedSuffixes is [txSuffixes] sorted, for an arm whose listing ORDER is not
// deterministic.
//
// The order is deterministic only when the entries were opened at DISTINCT
// instants on the server clock: txRegistry.list's insertion sort swaps on a
// strict Before (bolt/server/txregistry.go:190-194), so entries sharing one
// instant are left in Go map-iteration order, which is randomised per range. An
// arm that never advances its fake clock therefore has to compare the listing as
// a set; the oldest-first ORDER is adjudicated by the abandoned-registry arm,
// which staggers its opens one advance apart for exactly that reason.
func txSortedSuffixes(txs []server.TransactionInfo) []string {
	out := txSuffixes(txs)
	slices.Sort(out)
	return out
}

// armTxReadDeadline bounds the next exchange on c in REAL time. Every registry
// arm is single-goroutine, so a server that stopped answering would otherwise
// hang the package until the test binary's own timeout instead of failing.
func armTxReadDeadline(c *WireClient) error {
	if err := c.Conn().SetReadDeadline(time.Now().Add(txRegistryObserveTimeout)); err != nil {
		return fmt.Errorf("sim: bolt-tx: set read deadline: %w", err)
	}
	return nil
}

// clearTxReadDeadline removes the bound armed by [armTxReadDeadline].
func clearTxReadDeadline(c *WireClient) error {
	if err := c.Conn().SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("sim: bolt-tx: clear read deadline: %w", err)
	}
	return nil
}

// runInOpenTx runs one statement inside an ALREADY-OPEN explicit transaction and
// drains it. It sets no deadline of its own; the caller brackets the exchange
// with [armTxReadDeadline].
//
// Neither message re-arms the reaper while the fake clock is still: touchTx
// recomputes the SAME idle deadline and syncTxTimer early-returns on
// at.Equal(txTimerAt) (bolt/server/serve.go:1170).
func runInOpenTx(c *WireClient, query string) error {
	resp, err := c.Run(query, nil)
	if err != nil {
		return fmt.Errorf("RUN: %w", err)
	}
	if !isSuccess(resp) {
		return fmt.Errorf("RUN refused: %s %s", failureCode(resp), failureMessage(resp))
	}
	_, terminal, err := c.PullAll()
	if err != nil {
		return fmt.Errorf("PULL: %w", err)
	}
	if !isSuccess(terminal) {
		return fmt.Errorf("PULL refused: %s %s", failureCode(terminal), failureMessage(terminal))
	}
	return nil
}

// countLabelOverWire returns the number of nodes carrying label, read over the
// wire on c, so the census is subject to the same server it is measuring.
func countLabelOverWire(c *WireClient, label string) (int, error) {
	if err := armTxReadDeadline(c); err != nil {
		return 0, err
	}
	recs, err := wireQuery(c, fmt.Sprintf("MATCH (n:%s) RETURN count(n) AS c", label), nil)
	if err != nil {
		return 0, fmt.Errorf("sim: bolt-tx: count %s: %w", label, err)
	}
	if err := clearTxReadDeadline(c); err != nil {
		return 0, err
	}
	if len(recs) != 1 || len(recs[0].Data) != 1 {
		return 0, fmt.Errorf("sim: bolt-tx: count %s: got %d record(s), want 1 with 1 field", label, len(recs))
	}
	n, ok := recs[0].Data[0].(int64)
	if !ok {
		return 0, fmt.Errorf("sim: bolt-tx: count %s: got %T, want int64", label, recs[0].Data[0])
	}
	return int(n), nil
}

// countLabelViaEngine returns the number of nodes carrying label on a REOPENED
// store, read through the engine because no server is running over it.
func countLabelViaEngine(ctx context.Context, st *SimStore, label string) (int, error) {
	n, err := scalarCountViaEngine(ctx, st.Engine(), fmt.Sprintf("MATCH (n:%s) RETURN count(n)", label))
	if err != nil {
		return 0, fmt.Errorf("sim: bolt-tx: recovered count %s: %w", label, err)
	}
	return int(n), nil
}

// -----------------------------------------------------------------------------
// Arm 1 — the abandoned-registry arm: geometry
// -----------------------------------------------------------------------------

// The virtual geometry of the abandoned-registry arm. All FAKE, all governed by
// the server clock installed through [NewSimServerTxRegistry].
const (
	// txAbandonIdleBound is the server's MaxTxIdleTime for the arm: ten steps, so
	// there is room for the stagger AND for quiet ordinals before the first reap.
	txAbandonIdleBound = 1 * time.Second
	// txAbandonTotalBound is DefaultTxTimeout, six hundred steps. It is far above
	// the idle bound on purpose: effectiveTxDeadline (bolt/server/serve.go:1155)
	// takes the EARLIER of the two, so a total bound anywhere near the idle bound
	// would silently do the reaping and the arm would be measuring the wrong
	// reaper. It is also the REAL-time bound of the engine transaction's
	// context.WithTimeout (bolt/server/tx.go:83), which no fake advance can expire,
	// so it must comfortably outlast the arm's wall-clock runtime as well.
	txAbandonTotalBound = 10 * time.Minute
	// txAbandonStep is one advance of the measured sequence.
	txAbandonStep = 100 * time.Millisecond
	// txAbandonCount is how many transactions are opened and abandoned. Their opens
	// are staggered one step apart, so their reap ordinals are DISTINCT and a
	// reaper that emptied the whole registry on the first fire could not pass.
	txAbandonCount = 5
)

// The labels the arm writes under.
const (
	// txAbandonLabel is carried by the nodes a WRITING abandoned transaction
	// creates and never commits. Every one of them must be absent from the live
	// engine AND from the recovered one: the reap is a rollback, not a commit.
	txAbandonLabel = "TxAbandonedGhost"
	// txAbandonHonestLabel is carried by the autocommit writes the arm makes around
	// the reap. They are the non-vacuity witnesses: they prove the WAL counter and
	// the engine are live instruments, so "the reap appended no frame" and "no
	// ghost survived recovery" are statements about the server rather than about a
	// harness that wrote nothing at all.
	txAbandonHonestLabel = "TxRegistryHonest"
	// txAbandonRemote is the address every SimConn reports. It is a CONSTANT for
	// every connection (internal/sim/simconn.go:260/267), so TransactionInfo.Remote
	// cannot discriminate connections; the arm attributes by Principal and pins
	// Remote only as a field-fidelity check.
	txAbandonRemote = "sim-client"
	// txAbandonReadyState is the session state a listing must show for a
	// transaction that has completed its RUN and is waiting for the client.
	txAbandonReadyState = "TX_READY"
	// txReapFailureCode and txReapFailureMessage are what a reaped connection is
	// told on its next request-phase message: Session.reapTimedOutTx arms them as
	// pendingTermErr (bolt/server/session.go:1832-1835) and ONLY the reaper and an
	// operator termination do, which is what makes them attribution rather than
	// decoration — a transaction absent from the registry could equally have been
	// rolled back, or have lost its connection.
	//
	// The MESSAGE is pinned even though BOTH of its halves are currently false, and
	// deliberately so (rmp #2560): an idle reap did not "exceed its timeout" in the
	// sense a reader will assume, and no "writer lock" has been held for a
	// transaction's lifetime since rmp #2305 retired it. Pinning the exact text
	// means the eventual correction fails this arm on purpose instead of slipping
	// through, and the arm is then updated with the ticket.
	txReapFailureCode    = "Neo.ClientError.Transaction.TransactionTimedOut"
	txReapFailureMessage = "the transaction has been terminated because it exceeded its timeout; the writer lock was released"
)

// The four honest-write windows, named for where they sit relative to the reap.
const (
	txWindowBefore = "before"
	txWindowAtCap  = "at-cap"
	txWindowDuring = "during-reap"
	txWindowAfter  = "after"
)

// txAbandonWindows is the window roster in the order it must be driven.
var txAbandonWindows = []string{txWindowBefore, txWindowAtCap, txWindowDuring, txWindowAfter}

// txAbandonSeedMix decorrelates the SimDisk sub-seed from the arm seed, so the
// disk's fault draw stream shares no low-order bits with the mode draw.
const txAbandonSeedMix = 0x7B_12_5E_60

// txAbandonSettlePolls is how many times, after an advance has been delivered and
// the registry has reached the size the model predicts, the arm re-reads the
// registry before sampling it.
//
// It is a SETTLE BUDGET, not a proof. What it buys is a real-time window in which
// a reap that fired an ordinal too early can still land and be recorded at the
// ordinal it happened, instead of being recorded at the next one and matching the
// prediction by accident. What actually forbids an early fire is [clock.Fake]'s
// own arithmetic — advanceLocked skips every waiter with deadline.After(target) —
// which TestBoltTxFakeClockAdvance_DeliversOnDeadlineEquality pins directly.
const txAbandonSettlePolls = 16

// -----------------------------------------------------------------------------
// The evidence
// -----------------------------------------------------------------------------

// BoltTxPlanRow is one transaction the arm opened, as the HARNESS built it. It
// carries what was sent, never what was observed: it is the independent side of
// every listing comparison.
type BoltTxPlanRow struct {
	// Principal is the identity the connection authenticated as. It is unique per
	// connection and is the only field that can attribute a listing entry to a
	// connection, because Remote is a constant.
	Principal string
	// Mode is the BEGIN access mode, "r" or "w", exactly as sent on the wire.
	Mode string
	// IDSuffix is the registry's per-server sequence suffix ("-1", "-2", …) taken
	// from the observed id. The id's prefix is crypto/rand and is never recorded,
	// so a report is reproducible; the suffix IS seed-reproducible, because it is
	// the ordinal of the BEGIN on this server.
	IDSuffix string
	// Query is the single statement run inside the transaction before it went
	// silent, which is what the listing must report back.
	Query string
	// OpenedAt is the fake instant the transaction was opened at, as a duration
	// since the probe's base instant.
	OpenedAt time.Duration
	// Conn is the connection's index in the plan, 0-based.
	Conn int
}

// BoltTxListingRow is one entry of [server.Server.Transactions] as OBSERVED at
// the rendezvous, reduced to reproducible values: the id keeps only its sequence
// suffix and the two instants are offsets from the probe's base instant.
type BoltTxListingRow struct {
	// IDSuffix, Principal, Mode, Remote, State and Query are the listing's fields
	// verbatim, except that the id is reduced to its sequence suffix.
	IDSuffix  string
	Principal string
	Mode      string
	Remote    string
	State     string
	Query     string
	// StartedAt is TransactionInfo.StartedAt as an offset from the probe's base
	// instant. Because no advance is ever in flight while a transaction registers
	// — the arm advances only from its single controlling goroutine, and only
	// after the arming barrier — this must equal the plan's OpenedAt EXACTLY, not
	// approximately.
	StartedAt time.Duration
	// Elapsed is TransactionInfo.Elapsed verbatim. txRegistry.list computes it as
	// clk.Now() minus its own StartedAt (bolt/server/txregistry.go:174,185), so on
	// a still fake clock it must equal the listing instant minus StartedAt exactly.
	Elapsed time.Duration
}

// BoltTxWriteWindow is one honest autocommit write, bracketed by the WAL
// counters. The four windows straddle the reap so that "the reap appended
// nothing" is measured against a counter that demonstrably moves.
type BoltTxWriteWindow struct {
	// Name is the window's position in the run: one of [txAbandonWindows].
	Name string
	// Node is the name property the window wrote, unique per window.
	Node string
	// FramesBefore/FramesAfter and BytesBefore/BytesAfter bracket the window with
	// [wal.Stats].
	FramesBefore, FramesAfter uint64
	BytesBefore, BytesAfter   uint64
	// OpenTx is how many abandoned transactions the registry listed when the
	// window ran. It is what proves the window sat where its name says.
	OpenTx int
	// Ordinal is the advance ordinal the window ran after; 0 before the measured
	// sequence began.
	Ordinal int
	// Committed records that the server answered SUCCESS to both the RUN and the
	// PULL.
	Committed bool
}

// framesAppended reports how many WAL frames the window appended. The receiver is
// a pointer because the value is 88 bytes.
func (w *BoltTxWriteWindow) framesAppended() uint64 { return w.FramesAfter - w.FramesBefore }

// bytesAppended reports how many WAL bytes the window appended.
func (w *BoltTxWriteWindow) bytesAppended() uint64 { return w.BytesAfter - w.BytesBefore }

// BoltTxRegistryEvidence is everything one run of a transaction-registry arm
// observed, and NO verdict. The checkers are pure functions of it, which is what
// lets a test perturb exactly one field and prove the corresponding clause fires.
//
// Every field is either seed-pure or documented here as not being so. Two are
// not: Nows, whose value counts how many times the HARNESS polled the registry
// (every txRegistry.list call reads the clock, bolt/server/txregistry.go:174) and
// is therefore a function of scheduling; and the byte deltas of the honest
// windows, whose frame payload embeds a node's hidden key "__cx_"+hex(n) taken
// from a process-global counter in cypher/exec, so the frame's SIZE depends on
// how many nodes the rest of the process created first. Neither is rendered as a
// number by [BoltTxRegistryEvidence.String], which is what keeps a report
// byte-identical across runs of one seed.
type BoltTxRegistryEvidence struct {
	// Arm names the arm; Seed is the seed it was built from.
	Arm  string
	Seed uint64

	// TimersAtRendezvous is the counted timer registrations observed once every
	// transaction was open and its arming barrier had been reached. TimersTotal is
	// the count at the end of the run. Untils and Tickers are the other two
	// registration counters, and Nows is the read counter (not seed-pure; see the
	// type comment).
	TimersAtRendezvous int64
	TimersTotal        int64
	Untils             int64
	Tickers            int64
	Nows               int64

	// IdleBound and TotalBound are the two bounds installed on the SERVER.
	// ModelIdleBound is the bound the harness's own [idleReapModel] was built
	// from; a control desynchronises the two deliberately, and in the arm proper
	// they are equal.
	IdleBound      time.Duration
	TotalBound     time.Duration
	ModelIdleBound time.Duration
	// Step is one advance. SetupElapsed is the virtual time the staggered opens
	// consumed before the measured sequence began, and SimElapsed the total the
	// arm advanced. RendezvousAt is the virtual instant the listing was taken at.
	Step         time.Duration
	SetupElapsed time.Duration
	SimElapsed   time.Duration
	RendezvousAt time.Duration
	// Advances is how many advances the measured sequence made.
	Advances int

	// Plan is what the harness opened, in the order it opened it.
	Plan []BoltTxPlanRow
	// Listing is [server.Server.Transactions] at the rendezvous, in the order it
	// was returned. ListingOrder repeats that order as id suffixes alone, so a
	// re-ordering is legible in a report without reading the rows.
	Listing      []BoltTxListingRow
	ListingOrder []string

	// RegistryPeak is the largest listing the run ever observed. Accepted and
	// Refused count BEGINs by the server's answer — Refused is the per-principal
	// quota's refusal, which this arm installs no cap for and must therefore be
	// zero. Reaped counts the transactions observed to leave the registry across
	// the measured advances.
	RegistryPeak int
	Accepted     int
	Refused      int
	Reaped       int

	// PredictedReapOrdinals is [idleReapModel.reapOrdinals] computed BEFORE the
	// measured sequence ran; ObservedReapOrdinals is the ordinal at which each
	// transaction was first absent from the listing, in the same order as Plan,
	// or [reapNever] for one that never left.
	PredictedReapOrdinals []int
	ObservedReapOrdinals  []int
	// ProbeFireOrdinals are the ordinals at which the arm's own UNCOUNTED timer
	// received the advance. An ordinal missing here is an advance that never
	// reached a waiter, which would make "no reap at that ordinal" a statement
	// about the harness rather than about the reaper.
	ProbeFireOrdinals []int
	// SettleTimeoutOrdinals are the ordinals at which the registry never shrank to
	// the size the model predicts within [txRegistryObserveTimeout].
	SettleTimeoutOrdinals []int

	// ReapCodes and ReapMessages are what each abandoned connection was told on the
	// one message it was sent AFTER the measured sequence, in plan order: the typed
	// FAILURE the reaper arms, or "" when the server answered something else. They
	// are what attributes a transaction's ABSENCE from the registry to the reaper
	// rather than to a rollback, a dropped connection, or a harness mistake.
	ReapCodes    []string
	ReapMessages []string

	// Windows are the four honest autocommit writes, in the order they ran.
	Windows []BoltTxWriteWindow

	// ReapFrames and ReapBytes are the SUM of the per-advance WAL brackets. Each
	// bracket opens before an advance and closes when that advance has settled,
	// and the honest windows run BETWEEN advances, outside every bracket — which
	// is why this sum can be zero in a run that appended plenty of frames.
	//
	// The bracket is a LIVE instrument, measured rather than assumed: moving the
	// during-reap window inside an advance bracket made this read 4 frames and 195
	// bytes and fired the reap-wal clause, so a clean zero here is a statement
	// about the reap and not about a bracket that cannot see anything.
	ReapFrames uint64
	ReapBytes  uint64

	// SnapshotsBaseline, SnapshotsPeak and SnapshotsFinal are
	// [lpg.MVCCStats.ActiveSnapshots] before the first BEGIN, at its highest
	// reading, and once every transaction has been reaped. The final reading is
	// race-free: Session.abortTx rolls the engine transaction back — releasing the
	// horizon slot — BEFORE txClosed unregisters it (bolt/server/session.go:830),
	// so a registry the arm has watched empty cannot still be holding a slot.
	SnapshotsBaseline int
	SnapshotsPeak     int
	SnapshotsFinal    int

	// GhostsLive and GhostsRecovered count [txAbandonLabel] in the live engine and
	// after a crash and a real WAL replay. HonestLive and HonestRecovered do the
	// same for [txAbandonHonestLabel].
	GhostsLive      int
	GhostsRecovered int
	HonestLive      int
	HonestRecovered int
}

// -----------------------------------------------------------------------------
// The arm
// -----------------------------------------------------------------------------

// boltTxAbandonOptions parameterises the abandoned-registry arm. The zero value
// is not usable; [defaultBoltTxAbandonOptions] builds the nominal geometry and a
// control varies exactly one field of it.
type boltTxAbandonOptions struct {
	// ServerIdleBound is installed as server.Options.MaxTxIdleTime.
	ServerIdleBound time.Duration
	// ModelIdleBound is what the harness's [idleReapModel] predicts from. It is
	// equal to ServerIdleBound in the arm proper; a control that changes only the
	// SERVER's bound leaves this at the nominal value, which is exactly what makes
	// the predicted ordinals stop matching.
	ModelIdleBound time.Duration
	// TotalBound is server.Options.DefaultTxTimeout.
	TotalBound time.Duration
	// Step is one advance of the measured sequence.
	Step time.Duration
	// Count is how many transactions are opened and abandoned.
	Count int
	// Silent opens each transaction with a bare BEGIN and no statement at all.
	//
	// The arm proper leaves it false, because the statement is what leaves the
	// uncommitted ghost the atomicity clause hunts for and what gives the listing
	// a Query to report. A control sets it to isolate the arming, since ANY later
	// message re-enters syncTxTimer and — on a clock that has moved since the BEGIN
	// — re-arms the timer for a fresh deadline (MEASURED: a control that moved the
	// clock inside the first arming saw the RUN arm a second timer and restore the
	// original reap ordinal by accident).
	Silent bool
	// beforeArming, when non-nil, runs at the START of every Until on the server
	// clock — that is, INSIDE syncTxTimer, before it converts the absolute deadline
	// into the timer's duration. It is the seam a control uses to make the harness
	// win the arming race deterministically instead of by luck; see
	// [txArmingRaceProbe].
	beforeArming func(*txClockProbe)
}

// defaultBoltTxAbandonOptions returns the arm's nominal geometry: five
// transactions staggered one step apart under a ten-step idle bound, which the
// model turns into the reap ordinals 6..10 with five quiet ordinals in front.
func defaultBoltTxAbandonOptions() boltTxAbandonOptions {
	return boltTxAbandonOptions{
		ServerIdleBound: txAbandonIdleBound,
		ModelIdleBound:  txAbandonIdleBound,
		TotalBound:      txAbandonTotalBound,
		Step:            txAbandonStep,
		Count:           txAbandonCount,
	}
}

// txArmingRaceProbe is a [txClockProbe] that runs a hook at the START of every
// Until, so a control can move the fake clock at the one instant the arm's timer
// barrier exists to prevent it moving: after syncTxTimer has decided WHICH
// absolute deadline to arm for, but before it has measured how far away that is.
//
// It exists because the naive form of that control — "do not wait for the timer
// count, just advance as soon as BEGIN's SUCCESS arrives" — does not reliably
// reproduce anything. By the time the client has decoded the SUCCESS the server
// has usually already reached serve.go:1182, so the un-barriered arm passes
// cleanly nearly every time and a test asserting a deviation would be red for the
// wrong reason. Driving the hook makes the same hazard deterministic.
//
// # Concurrency contract
//
// Safe for concurrent use exactly as [txClockProbe] is. The hook runs on the
// SERVER's message-loop goroutine, not on the harness's, so it may only touch
// things that are themselves safe for concurrent use. Advancing the clock from it
// is sound because [clock.Fake] guards its own state with a mutex and holds no
// lock across the delivery, which is precisely why the hazard this reproduces is
// a real one rather than an artefact of the probe.
type txArmingRaceProbe struct {
	*txClockProbe
	hook func(*txClockProbe)
}

// Until runs the hook and then delegates, so the duration syncTxTimer receives is
// measured against whatever instant the hook left behind.
func (p *txArmingRaceProbe) Until(t time.Time) time.Duration {
	p.hook(p.txClockProbe)
	return p.txClockProbe.Until(t)
}

// RunBoltTxRegistryAbandoned drives the abandoned-registry arm once and returns
// the evidence. It is bit-reproducible from seed: the whole arm runs on ONE
// goroutine — the abandoned connections are silent by construction and need none
// — every advance is made by that goroutine, and the only seeded choice is each
// transaction's access mode.
//
// The returned error is reserved for harness failures (the store would not open,
// a connection would not negotiate, a barrier was never reached). A reap that
// lands on the wrong ordinal, a listing that misreports a field, or a ghost that
// survived recovery is EVIDENCE, not an error.
func RunBoltTxRegistryAbandoned(ctx context.Context, seed uint64) (*BoltTxRegistryEvidence, error) {
	opts := defaultBoltTxAbandonOptions()
	return runBoltTxAbandoned(ctx, seed, &opts)
}

// runBoltTxAbandoned is [RunBoltTxRegistryAbandoned] with the geometry made
// explicit, so a live control can vary one dimension of it and drive the same
// real server through the same real wire.
func runBoltTxAbandoned(ctx context.Context, seed uint64, opts *boltTxAbandonOptions) (*BoltTxRegistryEvidence, error) {
	disk := NewSimDisk(NewSeed(seed^txAbandonSeedMix), 0) // faultRate 0: this arm faults nothing
	cfg := durableStoreConfig()
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-tx-registry open store: %w", err)
	}

	probe := newTxClockProbe()
	var serverClk clock.Clock = probe
	if opts.beforeArming != nil {
		serverClk = &txArmingRaceProbe{txClockProbe: probe, hook: opts.beforeArming}
	}
	// The listener keeps REAL time. Sharing the fake with it would make every
	// parked connection register a timer of its own and destroy the attribution the
	// counted probe exists for; see [NewSimServerTxRegistry].
	srv, err := NewSimServerTxRegistry(st.Engine(), clock.Real(), serverClk,
		opts.ServerIdleBound, opts.TotalBound, 0)
	if err != nil {
		st.Crash()
		return nil, fmt.Errorf("sim: bolt-tx-registry server: %w", err)
	}

	r := &boltTxAbandonRunner{
		srv: srv, st: st, probe: probe, opts: opts,
		rng: NewSeed(seed),
		ev: &BoltTxRegistryEvidence{
			Arm: boltTxArmAbandoned, Seed: seed,
			IdleBound: opts.ServerIdleBound, TotalBound: opts.TotalBound,
			ModelIdleBound: opts.ModelIdleBound, Step: opts.Step,
		},
	}
	if err := r.drive(ctx); err != nil {
		r.closeConns()
		_ = srv.Close()
		st.Crash()
		return nil, err
	}
	r.closeConns()
	_ = srv.Close()

	// Crash (drop the engine, keep the SimDisk image — never a graceful flush) and
	// reopen through real recovery. A ghost frame that reached the durable log
	// without ever being visible in the live engine is invisible to the live census
	// and surfaces only here.
	st.Crash()
	st2, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-tx-registry reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()
	if err := r.censusRecovered(ctx, st2); err != nil {
		return nil, err
	}
	return r.ev, nil
}

// boltTxArmAbandoned names the arm in every violation message.
const boltTxArmAbandoned = "abandoned-registry"

// boltTxAbandonRunner threads the server, the store, the clock probe and the
// accumulating evidence through the arm's phases. It is driven by exactly one
// goroutine.
type boltTxAbandonRunner struct {
	srv    *SimServer
	st     *SimStore
	probe  *txClockProbe
	opts   *boltTxAbandonOptions
	rng    *Seed
	ev     *BoltTxRegistryEvidence
	honest *WireClient
	conns  []*WireClient
	model  idleReapModel
	// now is the fake instant the HARNESS believes it has advanced to, as an
	// offset from the probe's base instant. It is the harness's own arithmetic and
	// is never read back from the clock, so a control that moves the clock behind
	// the harness's back falsifies it — which is the point.
	now time.Duration
	// departed[i] is the ordinal at which plan row i was first absent.
	departed []int
}

// closeConns closes every client the run opened. Idempotent.
func (r *boltTxAbandonRunner) closeConns() {
	for _, c := range r.conns {
		_ = c.Close()
	}
	r.conns = nil
	if r.honest != nil {
		_ = r.honest.Close()
		r.honest = nil
	}
}

// walCounters reads the live WAL frame/byte counters.
func (r *boltTxAbandonRunner) walCounters() (frames, bytes uint64) {
	s := r.st.WAL().Stats()
	return s.Frames, s.Bytes
}

// snapshots reads the engine's registered-snapshot occupancy.
func (r *boltTxAbandonRunner) snapshots() int {
	return r.st.Graph().MVCCStats().ActiveSnapshots
}

// noteSnapshotPeak folds one reading into the running maximum.
func (r *boltTxAbandonRunner) noteSnapshotPeak() {
	if n := r.snapshots(); n > r.ev.SnapshotsPeak {
		r.ev.SnapshotsPeak = n
	}
}

// noteRegistryPeak folds one listing size into the running maximum.
func (r *boltTxAbandonRunner) noteRegistryPeak(n int) {
	if n > r.ev.RegistryPeak {
		r.ev.RegistryPeak = n
	}
}

// txProbeEpoch is the instant every [txClockProbe] starts at, and the baseline
// every duration offset in [BoltTxRegistryEvidence] is measured from. It is a
// function rather than a package-level variable so nothing can move it.
func txProbeEpoch() time.Time { return time.Unix(0, 0).UTC() }

// txIDSuffix reduces a registry id to its per-server sequence suffix ("-1",
// "-2", …), which is the only part of it that is reproducible: the prefix is a
// crypto/rand session id (bolt/server/session.go:530). An id with no suffix at
// all is reported as a fixed marker rather than passed through, so no run can
// leak a random value into a report.
func txIDSuffix(id string) string {
	if i := strings.LastIndexByte(id, '-'); i >= 0 {
		return id[i:]
	}
	return "<no-suffix>"
}

// drive runs the arm's phases in order. Every phase is on this one goroutine.
func (r *boltTxAbandonRunner) drive(ctx context.Context) error {
	hc, err := r.srv.Dial()
	if err != nil {
		return fmt.Errorf("sim: bolt-tx-registry dial honest: %w", err)
	}
	r.honest = hc
	if cerr := hc.Connect(ctx); cerr != nil {
		return fmt.Errorf("sim: bolt-tx-registry connect honest: %w", cerr)
	}

	// Window 1, before anything is open: the baseline proof that this connection
	// can write and that the WAL counter moves when it does.
	if werr := r.honestWindow(txWindowBefore, 0); werr != nil {
		return werr
	}
	r.ev.SnapshotsBaseline = r.snapshots()
	r.ev.SnapshotsPeak = r.ev.SnapshotsBaseline

	if oerr := r.openAbandoned(ctx); oerr != nil {
		return oerr
	}
	// Window 2, with every abandoned transaction open: a writer is served while
	// five explicit transactions sit idle. It is single-goroutine on purpose, so a
	// regression that put a transaction-lifetime writer hold back (rmp #2305) would
	// stall this call rather than fail it — a known and deliberate failure mode,
	// bounded by the read deadline honestWindow sets before every exchange.
	if werr := r.honestWindow(txWindowAtCap, 0); werr != nil {
		return werr
	}

	r.rendezvous()
	r.buildModel()
	if merr := r.measure(); merr != nil {
		return merr
	}

	if perr := r.probeReaped(); perr != nil {
		return perr
	}
	if werr := r.honestWindow(txWindowAfter, r.ev.Advances); werr != nil {
		return werr
	}
	r.ev.SnapshotsFinal = r.snapshots()
	r.ev.TimersTotal = r.probe.Timers()
	r.ev.Untils = r.probe.Untils()
	r.ev.Tickers = r.probe.Tickers()

	ghosts, honest, cerr := r.censusLive()
	if cerr != nil {
		return cerr
	}
	r.ev.GhostsLive, r.ev.HonestLive = ghosts, honest
	// Read last, so the count covers every registry listing the run took.
	r.ev.Nows = r.probe.Nows()
	return nil
}

// drawModes draws each transaction's access mode from the seed and then forces
// the plan to contain both kinds when it is long enough to hold them.
//
// The forcing is what makes two clauses non-vacuous: a READ transaction is what
// moves ActiveSnapshots without writing anything, and a WRITING one is what
// leaves the uncommitted ghost the atomicity clause looks for. A one-transaction
// plan (a control) keeps its draw untouched, because it cannot hold both.
func (r *boltTxAbandonRunner) drawModes() []string {
	modes := make([]string, r.opts.Count)
	for i := range modes {
		modes[i] = "w"
		if r.rng.Bool(0.5) {
			modes[i] = "r"
		}
	}
	if len(modes) < 2 {
		return modes
	}
	if !slices.Contains(modes, "r") {
		modes[len(modes)-1] = "r"
	}
	if !slices.Contains(modes, "w") {
		modes[0] = "w"
	}
	return modes
}

// abandonQuery returns the single statement transaction k runs before going
// silent. A writing transaction creates a node it never commits — the ghost the
// atomicity clause hunts for; a read-only one counts the honest nodes, which is
// a statement a read-only handle is permitted to run and which gives the listing
// a Query to report either way.
func abandonQuery(mode string, k int) string {
	if mode == "r" {
		return fmt.Sprintf("MATCH (n:%s) RETURN count(n) AS c", txAbandonHonestLabel)
	}
	return fmt.Sprintf("CREATE (:%s {name: %q})", txAbandonLabel, fmt.Sprintf("ghost-%d", k))
}

// openAbandoned opens every abandoned transaction, staggered one step apart, and
// records the plan. Each connection authenticates as its own principal, BEGINs,
// runs one statement, and is never spoken to again.
func (r *boltTxAbandonRunner) openAbandoned(ctx context.Context) error {
	modes := r.drawModes()
	r.ev.Plan = make([]BoltTxPlanRow, 0, len(modes))
	for k, mode := range modes {
		principal := fmt.Sprintf("tx-principal-%d", k)
		c, err := r.srv.Dial()
		if err != nil {
			return fmt.Errorf("sim: bolt-tx-registry dial %d: %w", k, err)
		}
		r.conns = append(r.conns, c)
		authResp, err := c.ConnectAs(ctx, principal, "")
		if err != nil {
			return fmt.Errorf("sim: bolt-tx-registry auth %q: %w", principal, err)
		}
		if !isSuccess(authResp) {
			return fmt.Errorf("sim: bolt-tx-registry auth %q refused: %s %s",
				principal, failureCode(authResp), failureMessage(authResp))
		}

		beginResp, err := c.BeginMode(mode)
		if err != nil {
			return fmt.Errorf("sim: bolt-tx-registry BEGIN %q: %w", principal, err)
		}
		if !isSuccess(beginResp) {
			// A refused BEGIN is evidence, not a harness failure — but this arm
			// installs no per-principal cap, so it cannot legitimately happen here
			// and the plan below would be built on a transaction that does not
			// exist. Record it and stop.
			r.ev.Refused++
			return fmt.Errorf("sim: bolt-tx-registry BEGIN %q refused: %s %s",
				principal, failureCode(beginResp), failureMessage(beginResp))
		}
		r.ev.Accepted++

		query := ""
		if !r.opts.Silent {
			query = abandonQuery(mode, k)
			if err := r.runInTx(c, query); err != nil {
				return fmt.Errorf("sim: bolt-tx-registry statement for %q: %w", principal, err)
			}
		}

		// THE barrier, in the order the message loop establishes them: the listing
		// is available first, the arming strictly later. Waiting on the arming is
		// what makes the next advance a deterministic reap rather than a race
		// against syncTxTimer; waiting on the listing as well is what lets the plan
		// take the id from the registry instead of inventing one.
		want := k + 1
		if err := waitForTxRegistry(func() bool { return len(r.srv.Server().Transactions()) == want },
			fmt.Sprintf("the registry never listed %d open transaction(s) after BEGIN %d", want, k)); err != nil {
			return err
		}
		if err := waitForTxRegistry(func() bool { return r.probe.Timers() >= int64(want) },
			fmt.Sprintf("the server never armed timer %d on the injected clock", want)); err != nil {
			return err
		}

		txs := r.srv.Server().Transactions()
		r.noteRegistryPeak(len(txs))
		// Attributed by PRINCIPAL, never by position: the listing's ORDER is what a
		// later clause adjudicates, so deriving the plan from it would make that
		// clause a transcription of itself. Remote cannot attribute anything — it
		// is the same constant on every SimConn.
		idx := -1
		for i := range txs {
			if txs[i].Principal == principal {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("sim: bolt-tx-registry: the registry lists %d transaction(s) but none for %q",
				len(txs), principal)
		}
		r.ev.Plan = append(r.ev.Plan, BoltTxPlanRow{
			Principal: principal, Mode: mode, IDSuffix: txIDSuffix(txs[idx].ID),
			Query: query, OpenedAt: r.now, Conn: k,
		})
		r.noteSnapshotPeak()

		if k < len(modes)-1 {
			r.probe.Advance(r.opts.Step)
			r.now += r.opts.Step
		}
	}
	return nil
}

// runInTx runs one statement inside an open explicit transaction and drains it.
// Neither message re-arms the reaper: the fake clock has not moved since the
// BEGIN, so touchTx recomputes the SAME idle deadline and syncTxTimer early-
// returns on at.Equal(txTimerAt) (bolt/server/serve.go:1170).
func (r *boltTxAbandonRunner) runInTx(c *WireClient, query string) error {
	if derr := armTxReadDeadline(c); derr != nil {
		return derr
	}
	if rerr := runInOpenTx(c, query); rerr != nil {
		return rerr
	}
	return clearTxReadDeadline(c)
}

// honestWindow drives one autocommit write on the honest connection, bracketed
// by the WAL counters, and records where in the run it happened.
//
// It is AUTOCOMMIT on purpose. An explicit transaction here would register a
// sixth entry in the registry and arm a sixth timer, and the arming count is the
// arm's only barrier — a window that armed one of its own would make the count
// unattributable. An autocommit RUN touches the injected clock zero times:
// touchTx returns at `if !s.txActive` without reading it, syncTxTimer's second
// branch is a no-op while txTimer is nil, and reportTx returns early while txID
// is empty.
func (r *boltTxAbandonRunner) honestWindow(name string, ordinal int) error {
	open := len(r.srv.Server().Transactions())
	r.noteRegistryPeak(open)
	w := BoltTxWriteWindow{
		Name:    name,
		Node:    "honest-" + name,
		OpenTx:  open,
		Ordinal: ordinal,
	}
	w.FramesBefore, w.BytesBefore = r.walCounters()
	err := r.runAutocommit(fmt.Sprintf("CREATE (:%s {name: %q})", txAbandonHonestLabel, w.Node))
	w.FramesAfter, w.BytesAfter = r.walCounters()
	if err != nil {
		// A refused honest write is EVIDENCE (the checker fails the run for it), not
		// a harness failure — but a transport error is, so the two are separated:
		// only a clean refusal reaches the evidence with Committed false.
		if !isWireRefusal(err) {
			return fmt.Errorf("sim: bolt-tx-registry honest window %q: %w", name, err)
		}
	} else {
		w.Committed = true
	}
	r.ev.Windows = append(r.ev.Windows, w)
	r.noteSnapshotPeak()
	return nil
}

// runAutocommit runs one statement outside any explicit transaction on the
// honest connection and drains it.
func (r *boltTxAbandonRunner) runAutocommit(query string) error {
	if err := armTxReadDeadline(r.honest); err != nil {
		return err
	}
	if _, err := wireQuery(r.honest, query, nil); err != nil {
		return err
	}
	return clearTxReadDeadline(r.honest)
}

// isWireRefusal reports whether err came from the server refusing a statement
// (a Bolt FAILURE) rather than from the transport. [wireQuery] renders both, so
// the arm classifies on the text it produces for a refusal.
func isWireRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "refused:")
}

// rendezvous records the listing every field-level clause is adjudicated
// against, taken with every transaction open, every arming barrier reached, and
// no advance in flight — which is precisely what makes StartedAt and Elapsed
// exact rather than approximate.
func (r *boltTxAbandonRunner) rendezvous() {
	r.ev.TimersAtRendezvous = r.probe.Timers()
	r.ev.RendezvousAt = r.now
	txs := r.srv.Server().Transactions()
	r.noteRegistryPeak(len(txs))
	epoch := txProbeEpoch()
	r.ev.Listing = make([]BoltTxListingRow, 0, len(txs))
	r.ev.ListingOrder = make([]string, 0, len(txs))
	for i := range txs {
		suffix := txIDSuffix(txs[i].ID)
		r.ev.Listing = append(r.ev.Listing, BoltTxListingRow{
			IDSuffix:  suffix,
			Principal: txs[i].Principal,
			Mode:      txs[i].Mode,
			Remote:    txs[i].Remote,
			State:     txs[i].State,
			Query:     txs[i].Query,
			StartedAt: txs[i].StartedAt.Sub(epoch),
			Elapsed:   txs[i].Elapsed,
		})
		r.ev.ListingOrder = append(r.ev.ListingOrder, suffix)
	}
}

// buildModel builds the independent prediction from the PLAN's arithmetic and
// sizes the measured sequence so it reaches the last predicted reap.
func (r *boltTxAbandonRunner) buildModel() {
	offsets := make([]time.Duration, len(r.ev.Plan))
	for i := range r.ev.Plan {
		offsets[i] = r.ev.Plan[i].OpenedAt
	}
	r.model = idleReapModel{
		MaxTxIdle: r.opts.ModelIdleBound,
		Step:      r.opts.Step,
		Elapsed:   r.now,
		Offsets:   offsets,
	}
	r.ev.SetupElapsed = r.now
	r.ev.PredictedReapOrdinals = r.model.reapOrdinals()
	for _, ord := range r.ev.PredictedReapOrdinals {
		if ord > r.ev.Advances {
			r.ev.Advances = ord
		}
	}
}

// measure runs the advance sequence one step at a time, recording at which
// ordinal each transaction left the registry and bracketing every advance with
// the WAL counters.
func (r *boltTxAbandonRunner) measure() error {
	r.departed = make([]int, len(r.ev.Plan))
	during := r.duringOrdinal()
	for n := 1; n <= r.ev.Advances; n++ {
		framesBefore, bytesBefore := r.walCounters()

		// The uncounted witness: registered for exactly one step, so [clock.Fake]'s
		// deadline-equality delivery hands it the very advance below. It leaves the
		// counted timer total — the arm's attribution of "one timer per open
		// transaction" — untouched.
		witness := r.probe.probeTimer(r.opts.Step)
		r.probe.Advance(r.opts.Step)
		r.now += r.opts.Step
		select {
		case <-witness.C():
			r.ev.ProbeFireOrdinals = append(r.ev.ProbeFireOrdinals, n)
		default:
		}
		witness.Stop()

		want := r.model.openAfter(n)
		if err := waitForTxRegistry(func() bool { return len(r.srv.Server().Transactions()) <= want },
			fmt.Sprintf("the registry never fell to the %d transaction(s) the model predicts after advance %d", want, n)); err != nil {
			r.ev.SettleTimeoutOrdinals = append(r.ev.SettleTimeoutOrdinals, n)
		}
		txs := r.settle()
		r.noteRegistryPeak(len(txs))
		r.recordDepartures(txs, n)

		framesAfter, bytesAfter := r.walCounters()
		r.ev.ReapFrames += framesAfter - framesBefore
		r.ev.ReapBytes += bytesAfter - bytesBefore
		r.noteSnapshotPeak()

		if n == during {
			if err := r.honestWindow(txWindowDuring, n); err != nil {
				return err
			}
		}
	}
	r.ev.ObservedReapOrdinals = r.departed
	for _, ord := range r.departed {
		if ord != reapNever {
			r.ev.Reaped++
		}
	}
	r.ev.SimElapsed = r.now
	return nil
}

// duringOrdinal picks the advance the third honest window runs after: the median
// predicted reap, so the window sits with some transactions already reclaimed and
// some still open. It is 0 — never reached — for an empty plan.
func (r *boltTxAbandonRunner) duringOrdinal() int {
	if len(r.ev.PredictedReapOrdinals) == 0 {
		return 0
	}
	return r.ev.PredictedReapOrdinals[len(r.ev.PredictedReapOrdinals)/2]
}

// settle re-reads the registry [txAbandonSettlePolls] times and returns the last
// listing. See [txAbandonSettlePolls] for what the budget does and does not buy.
func (r *boltTxAbandonRunner) settle() []server.TransactionInfo {
	return settleTxRegistry(r.srv)
}

// recordDepartures marks, for every plan row not yet seen to leave, the ordinal
// at which it was first absent from the listing.
func (r *boltTxAbandonRunner) recordDepartures(txs []server.TransactionInfo, ordinal int) {
	present := make(map[string]bool, len(txs))
	for i := range txs {
		present[txIDSuffix(txs[i].ID)] = true
	}
	for i := range r.ev.Plan {
		if r.departed[i] == reapNever && !present[r.ev.Plan[i].IDSuffix] {
			r.departed[i] = ordinal
		}
	}
}

// censusLive counts both labels in the LIVE engine, over the wire on the honest
// connection, so the census is subject to the same server it is measuring.
func (r *boltTxAbandonRunner) censusLive() (ghosts, honest int, err error) {
	if ghosts, err = countLabelOverWire(r.honest, txAbandonLabel); err != nil {
		return 0, 0, err
	}
	if honest, err = countLabelOverWire(r.honest, txAbandonHonestLabel); err != nil {
		return 0, 0, err
	}
	return ghosts, honest, nil
}

// censusRecovered counts both labels on a reopened store, after recovery has
// replayed the WAL.
func (r *boltTxAbandonRunner) censusRecovered(ctx context.Context, st *SimStore) error {
	ghosts, err := countLabelViaEngine(ctx, st, txAbandonLabel)
	if err != nil {
		return err
	}
	honest, err := countLabelViaEngine(ctx, st, txAbandonHonestLabel)
	if err != nil {
		return err
	}
	r.ev.GhostsRecovered, r.ev.HonestRecovered = ghosts, honest
	return nil
}

// -----------------------------------------------------------------------------
// The oracles
// -----------------------------------------------------------------------------

// txOp renders the Op field of a violation: the arm and the clause that failed.
func txOp(arm, clause string) string { return "<bolt-tx-registry:" + arm + ":" + clause + ">" }

// checkBoltTxRegistry adjudicates the evidence against the CONTRACT: what the
// registry must list, when the idle reaper must reclaim, and what a reclamation
// must leave behind. It is split from [checkBoltTxRegistryNonVacuity] so an
// uninformative run cannot read as a faulty one (rmp #2470): every violation here
// names a property of the SERVER, never a property of the run's own coverage.
//
// The receiver is a pointer because the value is well over the copy threshold; it
// mutates nothing.
func checkBoltTxRegistry(e *BoltTxRegistryEvidence) []Violation {
	// slices.Concat rather than four appends onto a nil slice: it sizes the result
	// once, and it returns nil — not an empty non-nil slice — when every clause is
	// satisfied, which is the shape a caller testing len(v) == 0 expects.
	return slices.Concat(
		checkBoltTxClockSeam(e),
		checkBoltTxListing(e),
		checkBoltTxReap(e),
		checkBoltTxResidue(e),
	)
}

// checkBoltTxClockSeam adjudicates the reaper's use of the injected clock: one
// timer per open transaction, one Until per timer, and no ticker at all.
func checkBoltTxClockSeam(e *BoltTxRegistryEvidence) []Violation {
	var v []Violation
	want := int64(len(e.Plan))
	if e.TimersAtRendezvous != want {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming"),
			Message: fmt.Sprintf("%d timer(s) were armed on the injected clock with %d transaction(s) open, want %d: "+
				"the reap ordinals below assume exactly one waiter per open transaction",
				e.TimersAtRendezvous, len(e.Plan), want),
		})
	}
	if e.TimersTotal != want {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming-total"),
			Message: fmt.Sprintf("the run registered %d timer(s) in total for %d transaction(s), want %d: "+
				"a re-armed or replacement timer would move every reap ordinal",
				e.TimersTotal, len(e.Plan), want),
		})
	}
	if e.Untils != e.TimersTotal {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming-until"),
			Message: fmt.Sprintf("Until was called %d time(s) against %d timer registration(s); syncTxTimer calls "+
				"clk.Until exactly once per arming (bolt/server/serve.go:1178), so these must match",
				e.Untils, e.TimersTotal),
		})
	}
	if e.Tickers != 0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "arming-ticker"),
			Message: fmt.Sprintf("the server registered %d ticker(s) on the injected clock, want 0: "+
				"a ticker consumes advances the reap ordinals depend on", e.Tickers),
		})
	}
	return v
}

// checkBoltTxListing adjudicates the rendezvous listing field by field against
// the plan the harness built. It is the clause the DST adds over the pre-existing
// bolt/server introspection tests, which can only assert Elapsed > 0: because the
// harness owns every advance and none is in flight while a transaction registers,
// StartedAt and Elapsed are EXACT here, not merely positive.
func checkBoltTxListing(e *BoltTxRegistryEvidence) []Violation {
	var v []Violation
	if len(e.Listing) != len(e.Plan) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "listing-size"),
			Message: fmt.Sprintf("the registry listed %d transaction(s) at the rendezvous, want %d (one per BEGIN)",
				len(e.Listing), len(e.Plan)),
		})
		return v
	}
	// Set equality by principal, adjudicated before order so a MISSING entry and a
	// REORDERED one are distinguishable in the report. Principal is the only field
	// that can attribute an entry to a connection: Remote is the same constant for
	// every SimConn.
	planned := make(map[string]bool, len(e.Plan))
	for i := range e.Plan {
		planned[e.Plan[i].Principal] = true
	}
	for i := range e.Listing {
		if !planned[e.Listing[i].Principal] {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "listing-principal"),
				Message: fmt.Sprintf("the listing names principal %q, which opened no transaction in the plan",
					e.Listing[i].Principal),
			})
		}
		delete(planned, e.Listing[i].Principal)
	}
	for principal := range planned {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "listing-principal"),
			Message: fmt.Sprintf("principal %q opened a transaction that the listing does not name", principal),
		})
	}
	// Oldest-first order, against the harness's own staggered insertion order.
	wantOrder := make([]string, len(e.Plan))
	for i := range e.Plan {
		wantOrder[i] = e.Plan[i].IDSuffix
	}
	if !slices.Equal(e.ListingOrder, wantOrder) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "listing-order"),
			Message: fmt.Sprintf("the listing is ordered %v, want %v: txRegistry.list documents oldest first and the "+
				"plan opened them one step apart", e.ListingOrder, wantOrder),
		})
	}
	for i := range e.Listing {
		v = append(v, checkBoltTxListingRow(e, &e.Listing[i], &e.Plan[i])...)
	}
	return v
}

// checkBoltTxListingRow adjudicates one listing entry against the plan row at the
// same position.
func checkBoltTxListingRow(e *BoltTxRegistryEvidence, got *BoltTxListingRow, want *BoltTxPlanRow) []Violation {
	var v []Violation
	deviate := func(field, gotV, wantV string) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "listing-"+field),
			Message: fmt.Sprintf("listing entry %s reports %s=%s, want %s", got.IDSuffix, field, gotV, wantV),
		})
	}
	if got.IDSuffix != want.IDSuffix {
		deviate("id", got.IDSuffix, want.IDSuffix)
	}
	if got.Principal != want.Principal {
		deviate("principal", got.Principal, want.Principal)
	}
	if got.Mode != want.Mode {
		deviate("mode", got.Mode, want.Mode)
	}
	if got.Query != want.Query {
		deviate("query", got.Query, want.Query)
	}
	if got.Remote != txAbandonRemote {
		deviate("remote", got.Remote, txAbandonRemote)
	}
	if got.State != txAbandonReadyState {
		deviate("state", got.State, txAbandonReadyState)
	}
	if got.StartedAt != want.OpenedAt {
		deviate("startedAt", got.StartedAt.String(), want.OpenedAt.String())
	}
	// Elapsed is graded against the entry's OWN StartedAt, so an Elapsed that
	// contradicts the instant it was stamped with is caught even when StartedAt is
	// itself wrong. txRegistry.list computes it as clk.Now() minus startedAt with
	// no advance in flight, so this is an equality and not a tolerance.
	if wantElapsed := e.RendezvousAt - got.StartedAt; got.Elapsed != wantElapsed {
		deviate("elapsed", got.Elapsed.String(), wantElapsed.String())
	}
	return v
}

// checkBoltTxReap adjudicates the idle reaper: every transaction left the
// registry at the advance the independent model predicts, every advance reached a
// waiter, and nothing was left open.
func checkBoltTxReap(e *BoltTxRegistryEvidence) []Violation {
	var v []Violation
	if !slices.Equal(e.ObservedReapOrdinals, e.PredictedReapOrdinals) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "reap-ordinal"),
			Message: fmt.Sprintf("transactions left the registry at advance ordinals %v, but the idle rule predicts %v "+
				"(idle bound %s, step %s, %s already elapsed when the sequence began; ordinal %d means never)",
				e.ObservedReapOrdinals, e.PredictedReapOrdinals, e.ModelIdleBound, e.Step, e.SetupElapsed, reapNever),
		})
	}
	for n := 1; n <= e.Advances; n++ {
		if !slices.Contains(e.ProbeFireOrdinals, n) {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "advance-delivery"),
				Message: fmt.Sprintf("advance %d of %d reached no waiter on the fake clock: "+
					"\"no reap at ordinal %d\" would be a statement about a tick that never arrived",
					n, e.Advances, n),
			})
		}
	}
	if len(e.SettleTimeoutOrdinals) != 0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "reap-settle"),
			Message: fmt.Sprintf("the registry never fell to the size the model predicts at advance ordinal(s) %v "+
				"within %s", e.SettleTimeoutOrdinals, txRegistryObserveTimeout),
		})
	}
	if e.Reaped != len(e.Plan) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "reap-count"),
			Message: fmt.Sprintf("%d of %d abandoned transaction(s) left the registry across %d advance(s): "+
				"the rest are still holding their slot", e.Reaped, len(e.Plan), e.Advances),
		})
	}
	v = append(v, checkBoltTxReapAttribution(e)...)
	if e.Refused != 0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "begin-refused"),
			Message: fmt.Sprintf("%d BEGIN(s) were refused although this arm installs no per-principal cap "+
				"(MaxOpenTxPerPrincipal 0 takes the server default)", e.Refused),
		})
	}
	return v
}

// checkBoltTxReapAttribution adjudicates WHY each transaction left the registry.
// The reaper arms a typed FAILURE for the connection's next request-phase
// message, and nothing else on the idle path does, so this is what distinguishes
// a reap from a rollback, a dropped connection, or a harness mistake.
func checkBoltTxReapAttribution(e *BoltTxRegistryEvidence) []Violation {
	var v []Violation
	if len(e.ReapCodes) != len(e.Plan) || len(e.ReapMessages) != len(e.Plan) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "reap-attribution"),
			Message: fmt.Sprintf("%d reap code(s) and %d message(s) were recorded for %d transaction(s): the run "+
				"cannot say WHY they left the registry", len(e.ReapCodes), len(e.ReapMessages), len(e.Plan)),
		})
		return v
	}
	for i := range e.ReapCodes {
		if e.ReapCodes[i] != txReapFailureCode {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "reap-attribution"),
				Message: fmt.Sprintf("connection %d was answered %q after its transaction left the registry, want %q: "+
					"only the reaper arms that failure, so absence alone does not prove a reap",
					e.Plan[i].Conn, e.ReapCodes[i], txReapFailureCode),
			})
			continue
		}
		// Pinned verbatim, both halves false, on purpose — see [txReapFailureMessage]
		// and rmp #2560.
		if e.ReapMessages[i] != txReapFailureMessage {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "reap-attribution"),
				Message: fmt.Sprintf("connection %d was told %q, want the text pinned in txReapFailureMessage (rmp "+
					"#2560): if that text has been corrected, update this arm with the ticket",
					e.Plan[i].Conn, e.ReapMessages[i]),
			})
		}
	}
	return v
}

// checkBoltTxResidue adjudicates what a reclamation leaves behind: no ghost in
// the engine or in the log, no WAL frame charged to the reap, no horizon slot
// still pinned, and every honest write durable.
func checkBoltTxResidue(e *BoltTxRegistryEvidence) []Violation {
	var v []Violation
	if e.GhostsLive != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: txOp(e.Arm, "ghost-live"),
			Message: fmt.Sprintf("%d node(s) labelled %s exist in the live engine: a reaped transaction's uncommitted "+
				"write survived the rollback", e.GhostsLive, txAbandonLabel),
		})
	}
	if e.GhostsRecovered != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(e.Arm, "ghost-recovered"),
			Message: fmt.Sprintf("%d node(s) labelled %s survived WAL replay: an uncommitted write reached the durable log",
				e.GhostsRecovered, txAbandonLabel),
		})
	}
	if e.ReapFrames != 0 || e.ReapBytes != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(e.Arm, "reap-wal"),
			Message: fmt.Sprintf("the advances that reaped %d transaction(s) appended %d WAL frame(s) and %d byte(s); "+
				"a rollback must append neither", len(e.Plan), e.ReapFrames, e.ReapBytes),
		})
	}
	if e.SnapshotsFinal > e.SnapshotsBaseline {
		v = append(v, Violation{
			Kind: ViolationACIDIsolation, Op: txOp(e.Arm, "horizon-slot"),
			Message: fmt.Sprintf("the reclamation horizon holds %d registered snapshot(s) once every transaction has been "+
				"reaped, against a baseline of %d: a reaped transaction is still pinning version memory",
				e.SnapshotsFinal, e.SnapshotsBaseline),
		})
	}
	for i := range e.Windows {
		v = append(v, checkBoltTxWindow(e.Arm, &e.Windows[i])...)
	}
	if e.HonestLive != len(e.Windows) {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(e.Arm, "honest-census"),
			Message: fmt.Sprintf("the live engine holds %d node(s) labelled %s, want %d (one per honest window)",
				e.HonestLive, txAbandonHonestLabel, len(e.Windows)),
		})
	}
	if e.HonestRecovered != e.HonestLive {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(e.Arm, "honest-recovered"),
			Message: fmt.Sprintf("WAL replay recovered %d node(s) labelled %s, but %d were acknowledged live",
				e.HonestRecovered, txAbandonHonestLabel, e.HonestLive),
		})
	}
	return v
}

// checkBoltTxWindow adjudicates one honest write: it was accepted, and it reached
// the durable log. It takes the ARM NAME rather than an evidence value so every
// transaction-registry arm grades its own windows through this one statement of
// the rule.
func checkBoltTxWindow(arm string, w *BoltTxWriteWindow) []Violation {
	var v []Violation
	if !w.Committed {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: txOp(arm, "honest:"+w.Name),
			Message: fmt.Sprintf("the honest write in window %q was REFUSED while %d transaction(s) were open: "+
				"an abandoned transaction must not stop an unrelated writer", w.Name, w.OpenTx),
		})
		return v
	}
	if w.framesAppended() == 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: txOp(arm, "honest:"+w.Name),
			Message: fmt.Sprintf("the honest write in window %q was acknowledged but appended NO WAL frame "+
				"(frames stuck at %d)", w.Name, w.FramesBefore),
		})
	}
	return v
}

// checkBoltTxRegistryNonVacuity proves the run actually exercised the surface, so
// a green contract verdict cannot come from an arm that quietly did nothing. It
// is deliberately separate from [checkBoltTxRegistry]: a shortfall here says the
// RUN was uninformative, never that the server is faulty (rmp #2470).
func checkBoltTxRegistryNonVacuity(e *BoltTxRegistryEvidence) []Violation {
	var v []Violation
	shortfall := func(clause, msg string) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: txOp(e.Arm, clause), Message: msg})
	}
	if len(e.Plan) != txAbandonCount {
		shortfall("nonvacuity-plan", fmt.Sprintf("the run opened %d transaction(s), want %d: "+
			"a shorter plan grades a smaller registry than the arm claims to", len(e.Plan), txAbandonCount))
	}
	var reads, writes int
	for i := range e.Plan {
		if e.Plan[i].Mode == "r" {
			reads++
			continue
		}
		writes++
	}
	if reads == 0 {
		shortfall("nonvacuity-mode", "no transaction was opened READ-ONLY: nothing registered a reader snapshot, so "+
			"\"the horizon is back to its baseline\" is a statement about an instrument that never moved for a reader")
	}
	if writes == 0 {
		shortfall("nonvacuity-mode", fmt.Sprintf("no transaction was opened for WRITING: none wrote an uncommitted "+
			"node, so \"no %s survived\" is satisfied by a run that never created one", txAbandonLabel))
	}
	if e.Accepted == 0 || len(e.Listing) == 0 {
		shortfall("nonvacuity-listing", fmt.Sprintf("the run accepted %d BEGIN(s) and graded a listing of %d entries: "+
			"the field-level oracle has nothing to adjudicate", e.Accepted, len(e.Listing)))
	}
	if e.RegistryPeak != len(e.Plan) {
		shortfall("nonvacuity-peak", fmt.Sprintf("the registry never held more than %d transaction(s) although the plan "+
			"opened %d: they were not all open together", e.RegistryPeak, len(e.Plan)))
	}
	v = append(v, checkBoltTxOrdinalShape(e, shortfall)...)
	v = append(v, checkBoltTxWindowShape(e, shortfall)...)
	if e.TotalBound <= e.IdleBound {
		shortfall("nonvacuity-bound", fmt.Sprintf("the total-lifetime bound is %s against an idle bound of %s: "+
			"effectiveTxDeadline reaps at the EARLIER of the two (bolt/server/serve.go:1155), so this run measured "+
			"the total-lifetime reaper and not the idle one", e.TotalBound, e.IdleBound))
	}
	if e.TimersTotal == 0 || e.Nows == 0 {
		shortfall("nonvacuity-seam", fmt.Sprintf("the server registered %d timer(s) and made %d Now call(s) on the "+
			"injected clock: the SetClock seam never reached the session, so every virtual-time clause is inert",
			e.TimersTotal, e.Nows))
	}
	if e.SnapshotsPeak <= e.SnapshotsBaseline {
		shortfall("nonvacuity-horizon", fmt.Sprintf("registered snapshots never rose above the baseline of %d "+
			"(peak %d): \"the horizon is back to its baseline\" is then true of an instrument that never moved",
			e.SnapshotsBaseline, e.SnapshotsPeak))
	}
	return v
}

// checkBoltTxOrdinalShape adjudicates the SHAPE of the reap plan: it must contain
// quiet ordinals and distinct reap ordinals, or the correspondence clause could
// not tell a correct reaper from a crude one.
func checkBoltTxOrdinalShape(e *BoltTxRegistryEvidence, shortfall func(clause, msg string)) []Violation {
	if e.Advances < 1 {
		shortfall("nonvacuity-advance", "the measured sequence made no advance at all")
		return nil
	}
	distinct := make(map[int]bool, len(e.PredictedReapOrdinals))
	for _, ord := range e.PredictedReapOrdinals {
		distinct[ord] = true
	}
	if len(distinct) != len(e.PredictedReapOrdinals) {
		shortfall("nonvacuity-stagger", fmt.Sprintf("the plan predicts reap ordinals %v, which are not all distinct: "+
			"a plan whose transactions fall due together cannot tell a correct reaper from one that empties the whole "+
			"registry on the first fire", e.PredictedReapOrdinals))
	}
	if quiet := e.Advances - len(distinct); quiet < 1 {
		shortfall("nonvacuity-quiet", fmt.Sprintf("%d of the %d advances reap nothing: with no quiet ordinal the run "+
			"never asserts that the reaper DECLINED to reap", quiet, e.Advances))
	}
	if e.Reaped == 0 {
		shortfall("nonvacuity-reap", "no transaction left the registry: the reap oracle never had an event to grade")
	}
	return nil
}

// checkBoltTxWindowShape adjudicates that the honest writes straddled the reap
// and that the WAL counter they are measured with is a live instrument.
func checkBoltTxWindowShape(e *BoltTxRegistryEvidence, shortfall func(clause, msg string)) []Violation {
	byName := make(map[string]*BoltTxWriteWindow, len(e.Windows))
	for i := range e.Windows {
		byName[e.Windows[i].Name] = &e.Windows[i]
	}
	var appended uint64
	for i := range e.Windows {
		appended += e.Windows[i].framesAppended()
	}
	for _, name := range txAbandonWindows {
		if byName[name] == nil {
			shortfall("nonvacuity-window", fmt.Sprintf("honest window %q did not run: the reap was not bracketed by "+
				"writes on both sides", name))
		}
	}
	if appended == 0 {
		shortfall("nonvacuity-wal", "no honest window appended a WAL frame: the frame counter is not a live instrument "+
			"here, so \"the reap appended none\" proves nothing")
	}
	// Where each window sat, measured rather than assumed. A window that claims to
	// be "at-cap" but ran with an empty registry would make its own clause vacuous.
	if w := byName[txWindowBefore]; w != nil && w.OpenTx != 0 {
		shortfall("nonvacuity-window", fmt.Sprintf("window %q ran with %d transaction(s) already open, want 0",
			txWindowBefore, w.OpenTx))
	}
	if w := byName[txWindowAtCap]; w != nil && w.OpenTx != len(e.Plan) {
		shortfall("nonvacuity-window", fmt.Sprintf("window %q ran with %d of %d transaction(s) open, want all of them",
			txWindowAtCap, w.OpenTx, len(e.Plan)))
	}
	if w := byName[txWindowDuring]; w != nil && (w.OpenTx == 0 || w.OpenTx >= len(e.Plan)) {
		shortfall("nonvacuity-window", fmt.Sprintf("window %q ran with %d of %d transaction(s) open, want strictly "+
			"between none and all: it is meant to sit INSIDE the reap sequence", txWindowDuring, w.OpenTx, len(e.Plan)))
	}
	if w := byName[txWindowAfter]; w != nil && w.OpenTx != 0 {
		shortfall("nonvacuity-window", fmt.Sprintf("window %q ran with %d transaction(s) still open, want 0",
			txWindowAfter, w.OpenTx))
	}
	return nil
}

// -----------------------------------------------------------------------------
// The renderer
// -----------------------------------------------------------------------------

// String renders the evidence for a report.
//
// It renders NO raw transaction id — only the per-server sequence suffix, which
// is the reproducible half — and no quantity that is a function of the process
// rather than of the seed. Two such quantities exist and are rendered as
// presence rather than as numbers: the Now count, which counts how often the
// HARNESS polled the registry, and each honest window's byte delta, whose frame
// payload embeds a node key drawn from a process-global counter. Everything else
// is seed-pure, so two runs of one seed render byte-identically.
func (e *BoltTxRegistryEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bolt-tx-registry evidence (arm=%s seed=%d):", e.Arm, e.Seed)
	fmt.Fprintf(&b, "\n  geometry: idle=%s model-idle=%s total=%s step=%s setup-elapsed=%s sim-elapsed=%s rendezvous-at=%s advances=%d",
		e.IdleBound, e.ModelIdleBound, e.TotalBound, e.Step, e.SetupElapsed, e.SimElapsed, e.RendezvousAt, e.Advances)
	fmt.Fprintf(&b, "\n  clock: timers=%d (at rendezvous %d) untils=%d tickers=%d nows-observed=%t",
		e.TimersTotal, e.TimersAtRendezvous, e.Untils, e.Tickers, e.Nows > 0)
	b.WriteString("\n  plan:")
	for i := range e.Plan {
		p := &e.Plan[i]
		fmt.Fprintf(&b, "\n    conn=%d id=%-4s principal=%-16s mode=%s opened=%-8s query=%q",
			p.Conn, p.IDSuffix, p.Principal, p.Mode, p.OpenedAt, p.Query)
	}
	fmt.Fprintf(&b, "\n  listing as returned: %v", e.ListingOrder)
	for i := range e.Listing {
		l := &e.Listing[i]
		fmt.Fprintf(&b, "\n    id=%-4s principal=%-16s mode=%s remote=%s state=%-12s started=%-8s elapsed=%-8s query=%q",
			l.IDSuffix, l.Principal, l.Mode, l.Remote, l.State, l.StartedAt, l.Elapsed, l.Query)
	}
	fmt.Fprintf(&b, "\n  registry: peak=%d accepted=%d refused=%d reaped=%d",
		e.RegistryPeak, e.Accepted, e.Refused, e.Reaped)
	fmt.Fprintf(&b, "\n  ordinals: predicted=%v observed=%v probe-fires=%v settle-timeouts=%v",
		e.PredictedReapOrdinals, e.ObservedReapOrdinals, e.ProbeFireOrdinals, e.SettleTimeoutOrdinals)
	b.WriteString("\n  honest windows:")
	for i := range e.Windows {
		w := &e.Windows[i]
		fmt.Fprintf(&b, "\n    %-12s ordinal=%-3d openTx=%d committed=%t frames+%d bytes-moved=%t",
			w.Name, w.Ordinal, w.OpenTx, w.Committed, w.framesAppended(), w.bytesAppended() > 0)
	}
	fmt.Fprintf(&b, "\n  reap attribution: codes=%v messages-as-pinned=%t", e.ReapCodes, txAllMessagesPinned(e))
	fmt.Fprintf(&b, "\n  reap bracket: frames+%d bytes+%d", e.ReapFrames, e.ReapBytes)
	fmt.Fprintf(&b, "\n  mvcc snapshots: baseline=%d peak=%d final=%d",
		e.SnapshotsBaseline, e.SnapshotsPeak, e.SnapshotsFinal)
	fmt.Fprintf(&b, "\n  census: ghosts=%d honest=%d | after recovery: ghosts=%d honest=%d",
		e.GhostsLive, e.HonestLive, e.GhostsRecovered, e.HonestRecovered)
	return b.String()
}

// probeReaped sends ONE statement down each abandoned connection, after the
// measured sequence has ended, and records what the server answered.
//
// It is what turns "the transaction is no longer listed" into "the reaper ended
// it". An absent entry is an ambiguous post-state on its own: a rollback, a
// dropped connection, or a harness that closed the wrong client would all produce
// it. Only [Session.reapTimedOutTx] arms the typed FAILURE recorded here.
//
// It cannot perturb any ordinal: every advance has already been made. Nor can it
// perturb the timer count — the session holds no transaction by now, so touchTx
// clears the idle deadline without reading the clock and syncTxTimer's second
// branch is a no-op with txTimer already consumed by the fire.
func (r *boltTxAbandonRunner) probeReaped() error {
	r.ev.ReapCodes = make([]string, 0, len(r.conns))
	r.ev.ReapMessages = make([]string, 0, len(r.conns))
	for i, c := range r.conns {
		if err := armTxReadDeadline(c); err != nil {
			return err
		}
		resp, err := c.Run("RETURN 1", nil)
		if err != nil {
			return fmt.Errorf("sim: bolt-tx-registry reap probe on connection %d: %w", i, err)
		}
		r.ev.ReapCodes = append(r.ev.ReapCodes, failureCode(resp))
		r.ev.ReapMessages = append(r.ev.ReapMessages, failureMessage(resp))
		if err := clearTxReadDeadline(c); err != nil {
			return err
		}
	}
	return nil
}

// txAllMessagesPinned reports whether every reaped connection was told exactly
// the text [txReapFailureMessage] pins. It is rendered as a boolean because the
// text is long and identical on every row; a row that deviates is named in full
// by the violation.
func txAllMessagesPinned(e *BoltTxRegistryEvidence) bool {
	if len(e.ReapMessages) == 0 {
		return false
	}
	for _, m := range e.ReapMessages {
		if m != txReapFailureMessage {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// The scenarios
// -----------------------------------------------------------------------------

// The catalogue defaults for the two transaction-registry scenarios.
const (
	boltTxRegistryDefaultSeed = 0x2482_ABA1
	boltTxQuotaDefaultSeed    = 0x2482_9074
)

// boltTxRegistryScenario drives the registry's LISTING, its IDLE REAPER and
// OPERATOR TERMINATION on virtual time — arms 1 and 2 in that order, each on its
// own server so their timer counts stay attributable.
func boltTxRegistryScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltTxRegistry,
		Description: "Bolt transaction registry on a fake clock: exact listing fields, idle reap at the predicted " +
			"advance ordinal, and operator termination — a live id ends, a stale one is refused, and a rollback " +
			"leaves no ghost and no WAL frame",
		Mode:        ModeDeterministic,
		DefaultSeed: boltTxRegistryDefaultSeed,
		run:         runBoltTxRegistryScenario,
	}
}

// boltTxQuotaScenario drives server.Options.MaxOpenTxPerPrincipal: who it
// refuses, at what number, and the three ways a slot comes back.
func boltTxQuotaScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltTxQuota,
		Description: "Bolt per-principal open-transaction cap: a typed refusal naming the principal and the limit, " +
			"a second principal unaffected, and a slot returned by the idle reaper, by TerminateTransaction, and by " +
			"a de-authorised session's refused COMMIT",
		Mode:        ModeDeterministic,
		DefaultSeed: boltTxQuotaDefaultSeed,
		run:         runBoltTxQuotaScenario,
	}
}

// runBoltTxRegistryScenario is the registry scenario's entry point: drive both
// arms, then adjudicate each against its contract and its non-vacuity gate.
//
// Both arms run, and their violations are concatenated, because they measure
// different halves of one surface and a caller selecting the scenario by name
// expects the whole of it. An arm that cannot even be DRIVEN is a harness error
// and aborts the scenario; a violation is a report.
func runBoltTxRegistryScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	abandoned, err := RunBoltTxRegistryAbandoned(ctx, seed)
	if err != nil {
		return nil, err
	}
	terminate, err := RunBoltTxTerminate(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := slices.Concat(
		checkBoltTxRegistry(abandoned),
		checkBoltTxRegistryNonVacuity(abandoned),
		checkBoltTxTerminate(terminate),
		checkBoltTxTerminateNonVacuity(terminate),
	)
	if len(v) == 0 {
		return nil, nil
	}
	return boltTxReport(ScenarioBoltTxRegistry, seed, v), nil
}

// runBoltTxQuotaScenario is the quota scenario's entry point.
func runBoltTxQuotaScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, err := RunBoltTxQuota(ctx, seed)
	if err != nil {
		return nil, err
	}
	v := slices.Concat(checkBoltTxQuota(ev), checkBoltTxQuotaNonVacuity(ev))
	if len(v) == 0 {
		return nil, nil
	}
	return boltTxReport(ScenarioBoltTxQuota, seed, v), nil
}

// boltTxReport wraps violations in a scenario report.
func boltTxReport(name string, seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Scenario:   name,
		Mode:       ModeDeterministic,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<bolt transaction registry>"},
		Violations: v,
	}
}
