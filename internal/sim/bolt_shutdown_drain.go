package sim

// bolt_shutdown_drain.go — the Bolt server's graceful teardown under simulation
// (rmp #2483): [server.Server.Shutdown]'s connection drain, and the
// [server.Options.Closer] ordering that store/db.go says a Bolt server provides.
//
// # The gap this closes
//
// Two things were driven by nothing in this package.
//
// [server.Options.Closer] was passed by no code in the module outside
// bolt/server's own tests (verified by grep over the whole tree: the only
// assignments were bolt/server/shutdown_closer_test.go and
// serve_ctx_cancel_closer_test.go). So the DST never once handed the server a
// store to own, and the ordering store/db.go names explicitly — "a Bolt server
// does this by draining its connections in bolt/server.Server.Shutdown before
// tearing down the DB" (store/db.go:54-57) — had no end-to-end test at all.
//
// [server.Server.Shutdown] was likewise never called: [SimServer.Close] cancels
// the serve context and closes the listener, which is the OTHER documented stop
// path, and ST3 ([checkpointTeardownScenario]) closes the store directly with
// db.CloseCtx while the server keeps serving — deliberately racing the close
// against in-flight commits to exercise store.DB's own quiesce. ST3 is therefore
// the INVERSE of this file and is left exactly as it is: it tests what happens
// when the store is torn down UNDER a live server, and this file tests the
// ordering that stops that from happening.
//
// # What is already covered elsewhere, and is not re-covered here
//
//   - db_teardown.go (rmp #2475) drives store.DB's OWN teardown variants: a
//     cancelled context, N concurrent Close/CloseCtx callers, a commit parked
//     inside its fsync, and the identity of the value the sync.Once publishes.
//     Every one of those is a property of store.DB, reached without a server.
//   - bolt/server/shutdown_closer_test.go drains an IDLE connection set and then
//     asserts the WAL is closed; serve_ctx_cancel_closer_test.go does the same
//     for ctx cancellation and counts the closer's Close calls.
//
// None of them has a connection in flight while Shutdown runs, none observes
// the drain against the close, and none takes either failure branch of Shutdown.
// Those three are what this file adds.
//
// # The ordering observable: a decorated connection, not a timing guess
//
// "The close must not begin until the last connection has finished" is only an
// assertion if the two events can be ordered. They can, exactly:
//
//   - the per-connection handler's conn.Close runs STRICTLY BEFORE the accept
//     loop's s.wg.Done. handleConn registers `defer conn.Close()` as its first
//     deferred call (bolt/server/serve.go:904) and the message loop's own
//     teardown closes it earlier still (:1063); s.wg.Done runs in the accept
//     loop's wrapper AFTER handleConn returns (:798).
//   - the closer can only be reached after s.wg.Wait() returns, from Shutdown's
//     drain-success branch (:874) or Serve's deferred exit path (:737).
//
// So a decorator that counts accepted-and-not-yet-closed connections
// ([boltDrainConnTracker], installed through [NewSimServerOwnedCloser]) gives a
// ONE-SIDED oracle: a closer body entered while a decorated connection is still
// open is a genuine breach, and the nanosecond window between a connection's
// Close and its wg.Done can only read as drained. The arms never dial while a
// teardown is in progress, which is what keeps the other direction sound too —
// the accept path increments the tracker just before s.wg.Add, and a connection
// dialed into that window would be live-but-unwaited-for.
//
// The second observable is stronger still and it is a CONSTRUCTION: a commit is
// parked inside its WAL fsync with [SimDisk.ArmSyncGateAt], so a server handler
// goroutine is provably mid-statement, and the arm then requires the closer to
// have been called ZERO times across a bounded window in which Shutdown has
// demonstrably already closed the listener (a fresh Dial is refused).
//
// # THE RULE THIS FILE LEARNED TWICE: only a TERMINAL reply is an acknowledgement
//
// This arm produced TWO harness defects, and both were the same mistake: a
// non-terminal Bolt reply read as a durability acknowledgement. They are recorded
// here rather than quietly fixed, because the rule that resolves both is the most
// transferable thing in the task.
//
//  1. IGNORED read as an acknowledgement. A session answered a FAILURE is in FAILED
//     and answers every subsequent request-phase message with [proto.Ignored]. The
//     first oracle classified the reply as "not a FAILURE", so an ignored statement —
//     never dispatched at all — was recorded as a durable commit. It manufactured an
//     ACID_DURABILITY violation in 8 of 30 concurrent runs. bolt_auth_surface.go
//     (rmp #2481) had already added [isIgnored] against exactly this class and noted
//     its own arms did not reach the branch; these arms do, and now reuse it.
//
//  2. RUN SUCCESS read as an acknowledgement. Even a genuine [proto.Success] at RUN
//     is not one. handleRun replies SUCCESS whenever the engine call returned no
//     error and never consults the result's own error
//     (bolt/server/session.go:1185-1229); its metadata is "fields", "qid" and "db",
//     a statement-ACCEPTED reply. The BOOKMARK — the causal-consistency token a
//     driver uses to establish that a write landed — is delivered only under
//     `if !hasMore` on the terminal PULL (session.go:1393-1397), on DISCARD (:1500)
//     and on COMMIT (:1696). And the engine records a commit failure ON the Result
//     rather than returning it: commitUnderBarrier has no return value and its
//     second statement is an early return on `r.rs.Err() != nil || r.rowsErr != nil`
//     (cypher/api.go:5639-5641), so a statement whose materialise failed appends no
//     WAL frame and makes nothing visible.
//
// Both were caught by the same measurement, and it is worth repeating because it is
// what distinguishes a false acknowledgement from a durability loss: for every
// suspect name, ask whether it is in the LIVE engine and whether it appears
// ANYWHERE in the raw WAL byte image. Measured for the lost rows: absent from both,
// with the WAL's durable watermark equal to the image size — so nothing had been
// made durable and nothing acknowledged had been lost. The engine was right and the
// harness was wrong, twice.
//
// [BoltDrainCommit.RunAcked] and [BoltDrainCommit.RunIgnored] are therefore kept as
// recorded WITNESSES, never as the oracle: they are what lets a report tell "never
// dispatched" from "dispatched, outcome unknown to the client".
//
// # Four further things were MEASURED here, and three refute the obvious model
//
// 1. An auto-commit write's fsync happens inside RUN, not inside PULL. The gate was
// reached while the client was still blocked in its RUN round trip, the disk's Sync
// count advanced 0 -> 1 during that call, the node was already present in the engine
// when RUN returned, and the following PULL added no Sync. So the write COMPLETES
// during RUN — which is why the parked commit is durable — while the client's
// ACKNOWLEDGEMENT of it still only arrives with the terminal reply, per the rule
// above. Both facts are true at once and neither implies the other.
//
// 2. A graceful Shutdown delivers the in-flight statement's reply and then closes
// the connection; it does NOT wait for the client to drain its result stream. The
// message loop returns on the FIRST pass through its select after connCtx is
// cancelled (bolt/server/serve.go:1194), which for a parked statement is immediately
// after that statement's reply is flushed. Measured: the gated commit's RUN returned
// SUCCESS and the following PULL failed to write at all, because the server end was
// already closed. It costs no durability — the write was committed and is recovered —
// so [checkBoltDrainInFlightCompleted] adjudicates dispatch and durability, and the
// PULL is a witness.
//
// 3. Shutdown's `<-ctx.Done()` branch is UNREACHABLE for a deadline-bearing context.
// Shutdown clamps its drain timeout to time.Until(deadline)
// (bolt/server/serve.go:861-867) and then selects over both, so the clamped
// time.After is armed from a marginally earlier reading and wins. Measured 12/12: an
// 80 ms deadline returned "bolt: shutdown: drain timeout exceeded" — built with
// errors.New at the call site, so there is no sentinel to match — never
// context.DeadlineExceeded. The ctx branch is reached only by an explicit cancel on a
// context with no deadline, measured 12/12 as context.Canceled. Both are arms here
// ([ArmBoltDrainExpiryDrainTimeout], [ArmBoltDrainExpiryCtxCancel]), because a single
// "the expiry error" arm would have pinned one branch and left the other untested
// while looking complete.
//
// 4. WHO closes the store is a RACE on the success path and a certainty on the
// failure paths. Shutdown cancels the accept context before it drains, so Serve's
// deferred exit path and Shutdown's drain-success branch end up waiting on the SAME
// s.wg and either may reach closeOwned first. Measured over 25 successful drains: 22
// closed by Serve's exit, 3 by Shutdown — so a Shutdown returning nil does NOT mean
// Shutdown closed the store. The once-guard makes the outcome identical, so the
// attribution is carried as a WITNESS on the ordered arm and adjudicated on neither.
// On the two expiry arms it IS adjudicated: those branches provably never call
// closeOwned (measured: zero closer calls at the instant Shutdown returned, 12/12 in
// both modes), so the store is closed by Serve's exit path, after Shutdown has
// already returned its error — and the arms require exactly that.
//
// # What a client is told, and why the obvious oracle cannot be written
//
// The task this file implements asked for "no wal.ErrWriterClosed reaches a
// client". That string can never reach a client: [wal.ErrWriterClosed] is not a
// client-fault error, so Session.sanitiseErr replaces its text with a generic
// internal-error message (bolt/server/session.go:1945-1946) and FailureCode maps
// it to the catch-all Neo.DatabaseError.General.UnknownError
// (bolt/server/errors.go:247). Measured, by closing the store under a live
// connection: code "Neo.DatabaseError.General.UnknownError", message "An internal
// error occurred. See server logs for details (session: <random hex>)." — and the
// session id is crypto-random (session.go:530), so the message is not
// reproducible and never enters the evidence's String.
//
// The oracle is therefore split across the two places where each half IS
// observable:
//
//   - on the WIRE, no statement issued before the teardown may be answered with a
//     DatabaseError-class failure ([boltDrainDatabaseErrorCode]). That is the
//     signature a raced teardown produces, and the [ArmBoltDrainUnordered]
//     control produces it deliberately by closing the store with a live client
//     mid-session, which is what proves the clause can fail.
//   - at the STORE, the identity of the error is checkable: after the teardown the
//     arm attempts one commit through the transaction layer and requires
//     errors.Is(err, wal.ErrWriterClosed). That is both the proof the WAL really
//     closed and the proof the writer-closed detector is not blind.
//
// # Determinism
//
// The four arms of [boltShutdownDrainScenario] are deterministic: every name is
// drawn from the seed in a fixed order, every rendezvous is a [SyncGate] rather
// than a sleep, and no arm depends on which goroutine wins a race — the one race
// that exists (the close attribution on the success path) is a witness, not a
// clause. [ArmBoltDrainFleet] is NOT bit-reproducible and says so: it is
// registered as a separate ModeConcurrent scenario
// ([boltShutdownFleetScenario]) and adjudicated only on the invariants that hold
// under any interleaving.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// The arm names, carried in the evidence and used by the arm-specific clauses.
const (
	// ArmBoltDrainOrdered is the drain-success arm: a commit is parked inside its
	// WAL fsync, Shutdown drains it, and only then is the store closed.
	ArmBoltDrainOrdered = "ordered-drain"
	// ArmBoltDrainExpiryDrainTimeout is the expiry arm driven by a DEADLINE, which
	// takes Shutdown's clamped drain-timeout branch (see the file comment).
	ArmBoltDrainExpiryDrainTimeout = "expiry-drain-timeout"
	// ArmBoltDrainExpiryCtxCancel is the expiry arm driven by an explicit cancel on
	// a context with no deadline, which is the only way to reach Shutdown's
	// `<-ctx.Done()` branch.
	ArmBoltDrainExpiryCtxCancel = "expiry-ctx-cancel"
	// ArmBoltDrainOnce is the publication arm: the store's close is made to FAIL,
	// so the value the server's sync.Once caches is a non-nil, freshly allocated
	// error whose IDENTITY across callers is a discriminating assertion.
	ArmBoltDrainOnce = "once-published"
	// ArmBoltDrainUnordered is the SENSITIVITY control: the store is closed with a
	// live client mid-session and no drain at all, which is what the ordering
	// exists to prevent. It is expected to FAIL the ordering and wire clauses.
	ArmBoltDrainUnordered = "unordered-control"
	// ArmBoltDrainFleet is the concurrent arm: several committers in flight when
	// Shutdown fires. Leak/no-panic/convergence guarded, NOT bit-reproducible.
	ArmBoltDrainFleet = "fleet-drain"
)

// The attribution tokens [boltDrainAttribute] resolves a close body's caller to.
// They are normalised on purpose: the raw frame list carries no addresses, but it
// does carry closure ordinals (Serve.func2) that a refactor of an unrelated
// deferred call would renumber, so the evidence records the decision and not the
// stack.
const (
	// boltDrainClosedByServeExit is [server.Server.Serve]'s deferred exit path.
	boltDrainClosedByServeExit = "serve-exit"
	// boltDrainClosedByShutdown is [server.Server.Shutdown]'s drain-success branch.
	boltDrainClosedByShutdown = "shutdown"
	// boltDrainClosedByHarness is the control arm's deliberate out-of-order close.
	boltDrainClosedByHarness = "harness-out-of-order"
	// boltDrainClosedByUnknown means no recognised frame was on the stack, which
	// would mean a new close path exists that this file does not model.
	boltDrainClosedByUnknown = "unknown"
)

// boltDrainDatabaseErrorCode is the Bolt failure code a commit that hits a closed
// WAL writer produces. It is the catch-all bucket
// ([github.com/FlavioCFOliveira/GoGraph/bolt/server].FailureCode's final return,
// bolt/server/errors.go:247), reached because [wal.ErrWriterClosed] matches none
// of the specific branches above it — which is also why the message the client
// gets is sanitised. Any DatabaseError-class reply to a statement issued before
// the teardown is the wire signature of a raced close.
const boltDrainDatabaseErrorCode = "Neo.DatabaseError.General.UnknownError"

// The three phases a write attempt is dated by, which is the whole basis of the
// wire clause: a statement issued BEFORE the teardown must not be answered with a
// storage failure, while one issued after it legitimately is.
const (
	// boltDrainPhasePre is a statement issued and answered before the teardown
	// began.
	boltDrainPhasePre = "pre"
	// boltDrainPhaseWALSuffix is a statement committed AFTER the arm's one
	// checkpoint, so it lives only in the WAL suffix. It is what stops the
	// durability clause being answered by the published snapshot underneath it.
	boltDrainPhaseWALSuffix = "wal-suffix"
	// boltDrainPhaseInFlight is the statement parked inside its WAL fsync when the
	// teardown began.
	boltDrainPhaseInFlight = "inflight"
	// boltDrainPhaseUndrained is a statement issued on a connection the server had
	// NOT drained, after the store was closed. Only the control arm produces one:
	// it is exactly the shape the drain ordering exists to make impossible.
	boltDrainPhaseUndrained = "undrained"
)

// boltDrainInterruptedCode is what a client is told when its in-flight statement is
// cut short by the shutdown itself rather than by a storage failure. Shutdown
// cancels the accept context before it drains, every connection context derives
// from it (bolt/server/serve.go:1043), and a statement that checks its context is
// therefore interrupted and answered with a TRANSIENT code — which is the right
// thing to tell a driver, because retrying against another server will work.
//
// MEASURED on the concurrent arm: one committer's RUN was answered with exactly
// this code. It is accepted on any arm, because the stronger clauses do the real
// work: an interrupted statement is never acknowledged, so it is never in the acked
// set the durability clause is answered against, and a parked commit is separately
// required to be ACKNOWLEDGED, so a regression that interrupted in-flight writes
// wholesale could not hide behind it.
const boltDrainInterruptedCode = "Neo.TransientError.General.RequestInterrupted"

// boltDrainTerminatedCode is the SECOND code a shutdown-interrupted statement can
// receive, and it is the more interesting one. It appears when the cancellation
// surfaces as a context.Canceled coming back from the ENGINE rather than from one of
// the session's own pre-dispatch checks: [FailureCode] maps context.Canceled to
// Neo.ClientError.Transaction.Terminated (bolt/server/errors.go:34-36), where the
// path above maps the session's own check to a TRANSIENT code
// (bolt/server/session.go:554 and :1313).
//
// The two differ in a way that matters to a driver fleet, and this module already
// documents why: neo4j-go-driver v5.28.4 retries on `classification ==
// "TransientError"`, and its reclassify() demotes Transaction.Terminated out of that
// family before the classification is even parsed — the trap spelled out at
// bolt/server/errors.go:167-176. So a write cut short by a GRACEFUL SERVER SHUTDOWN
// can be reported to the client as a non-retryable client error.
//
// It is MEASURED here rather than judged: both codes are pinned as named constants
// and accepted on the concurrent arm only, so if the classification is ever
// corrected this arm fails deliberately and the change is a decision rather than a
// drift.
const boltDrainTerminatedCode = "Neo.ClientError.Transaction.Terminated"

// boltDrainLabel is the node label every commit in this file writes, so the
// recovered graph can be interrogated by NAME rather than by count.
const boltDrainLabel = "BoltDrain"

// boltDrainDiskSeedMix decorrelates this file's SimDisk sub-stream from the run
// seed's other consumers, so arming a gate or a fault here never perturbs another
// scenario's reproducible fault stream.
const boltDrainDiskSeedMix uint64 = 0x2483_D5A1_4E_0DDF

// Bounded, short-layer sizes.
const (
	// boltDrainPreCommits is how many commits are acknowledged over the wire
	// BEFORE the teardown. They are what the durability clause is answered by, and
	// they must live in the WAL rather than in a snapshot: the checkpointer is
	// configured with no MaxAge and no Interval, and no arm triggers it, so nothing
	// folds them away.
	boltDrainPreCommits = 3
	// boltDrainIdleConns is how many further connections are open and IDLE when the
	// teardown starts. They cost nothing and they are what makes the drain a drain:
	// each is a live handler goroutine in s.wg that must exit before the close.
	boltDrainIdleConns = 3
	// boltDrainBlockWindow is how long an arm waits before deciding the closer is
	// genuinely NOT being called while a commit is parked. It mirrors
	// [dbTeardownBlockWindow], which mirrors store/db_quiesce_test.go.
	boltDrainBlockWindow = 50 * time.Millisecond
	// boltDrainExpiryBudget is the expiry arms' shutdown budget. It is comfortably
	// longer than the ~1 ms a local drain needs and far shorter than the gate is
	// held for, so the expiry is a certainty rather than a race.
	boltDrainExpiryBudget = 80 * time.Millisecond
	// boltDrainJoinTimeout bounds every wait on a goroutine this file owns, so a
	// regression that deadlocks a teardown fails the run instead of hanging the
	// package until the binary's own timeout.
	boltDrainJoinTimeout = 30 * time.Second
	// boltDrainDialPollStep is the poll interval used to observe that Shutdown has
	// closed the listener. It is a POSITIVE observation (a Dial that is refused),
	// never a sleep standing in for one.
	boltDrainDialPollStep = 2 * time.Millisecond
	// boltDrainFleetConns / boltDrainFleetOps bound the concurrent arm.
	boltDrainFleetConns = 4
	boltDrainFleetOps   = 4
)

// -----------------------------------------------------------------------------
// The connection-drain observable
// -----------------------------------------------------------------------------

// boltDrainConnTracker counts the connections the server's accept loop holds. It
// is installed as [NewSimServerOwnedCloser]'s wrapConn decorator, so every
// server-side connection passes through it, and it is read by
// [boltDrainCloser.Close] at the instant the store's teardown begins.
//
// # Concurrency contract
//
// Safe for concurrent use: every field is an atomic and the decorated
// connection's Close is once-guarded, because the server closes a connection
// twice on purpose (the message loop's teardown at bolt/server/serve.go:1063 and
// handleConn's outer defer at :904).
type boltDrainConnTracker struct {
	live     atomic.Int64
	peak     atomic.Int64
	accepted atomic.Int64
	closed   atomic.Int64
}

// wrap decorates one accepted connection, counting it live until it is closed.
func (t *boltDrainConnTracker) wrap(c net.Conn) net.Conn {
	t.accepted.Add(1)
	live := t.live.Add(1)
	for {
		peak := t.peak.Load()
		if live <= peak || t.peak.CompareAndSwap(peak, live) {
			break
		}
	}
	return &boltDrainConn{Conn: c, tracker: t}
}

// boltDrainConn is one decorated server-side connection.
type boltDrainConn struct {
	net.Conn
	tracker *boltDrainConnTracker
	once    sync.Once
}

// Close implements [net.Conn.Close], releasing the connection's live count once
// however many times the server closes it.
func (c *boltDrainConn) Close() error {
	c.once.Do(func() {
		c.tracker.live.Add(-1)
		c.tracker.closed.Add(1)
	})
	return c.Conn.Close()
}

// -----------------------------------------------------------------------------
// The closer under observation
// -----------------------------------------------------------------------------

// errBoltDrainCloseFault is the sentinel the publication arm's close failure
// wraps, so [errors.Is] can recognise the value every teardown caller observed
// without comparing the (freshly allocated, per-invocation) wrapper by text.
var errBoltDrainCloseFault = errors.New("sim: bolt-drain: injected store-close failure")

// boltDrainCloseBody is one invocation of the store's teardown, as the closer
// itself observed it.
type boltDrainCloseBody struct {
	// Attribution is which code path called it (see the boltDrainClosedBy*
	// constants).
	Attribution string
	// AcceptedAt / ClosedAt / LiveAt are the connection tracker's readings at the
	// instant the body was entered. LiveAt must be zero: the drain is complete.
	AcceptedAt, ClosedAt, LiveAt int64
	// AfterShutdownReturned records whether the arm's first Shutdown call had
	// already returned. It is what dates the close on the expiry arms.
	AfterShutdownReturned bool
}

// boltDrainCloser is the [io.Closer] the server owns. It wraps the real
// [store.DB] teardown and records, per invocation, who called it and what the
// connection drain looked like at that instant.
//
// # Concurrency contract
//
// Safe for concurrent use. The server's own sync.Once should make Close a
// single-caller affair; this type does not assume that, because proving it is the
// point.
type boltDrainCloser struct {
	inner io.Closer
	conns *boltDrainConnTracker
	// failCloses makes every invocation return a FRESHLY allocated error wrapping
	// [errBoltDrainCloseFault]. A fresh value per invocation is what makes "every
	// caller observed the same value" discriminating rather than a tautology over a
	// shared sentinel — the distinction rmp #2472 established.
	failCloses bool
	// shutdownReturned is set by the arm the moment its first Shutdown call
	// returns, so a close body can date itself against that event.
	shutdownReturned atomic.Bool
	mu               sync.Mutex
	bodies           []boltDrainCloseBody
	runs             atomic.Int64
}

// Close implements [io.Closer]. It records the observation FIRST, so a body that
// then panicked or blocked would still be visible in the evidence.
func (c *boltDrainCloser) Close() error {
	run := c.runs.Add(1)
	accepted, closed := c.conns.accepted.Load(), c.conns.closed.Load()
	body := boltDrainCloseBody{
		Attribution:           boltDrainAttribute(3),
		AcceptedAt:            accepted,
		ClosedAt:              closed,
		LiveAt:                c.conns.live.Load(),
		AfterShutdownReturned: c.shutdownReturned.Load(),
	}
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()

	err := c.inner.Close()
	if c.failCloses {
		return fmt.Errorf("%w (invocation %d): %s", errBoltDrainCloseFault, run, errText(err))
	}
	return err
}

// Runs reports how many times the close body has been entered.
func (c *boltDrainCloser) Runs() int64 { return c.runs.Load() }

// Bodies returns a copy of the recorded invocations.
func (c *boltDrainCloser) Bodies() []boltDrainCloseBody {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.bodies)
}

// boltDrainAttribute walks the calling stack and reports which code path reached
// the closer. skip is passed to [runtime.Callers] verbatim.
//
// It is an INDEPENDENT reference: the Go runtime, not bolt/server, answers who
// called. The alternative — inferring the caller from which of Shutdown or Serve
// returned first — cannot work, because on the success path both wait on the same
// sync.WaitGroup and either may win (measured 22/25 Serve, 3/25 Shutdown).
func boltDrainAttribute(skip int) string {
	var pcs [32]uintptr
	n := runtime.Callers(skip, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		switch {
		case strings.Contains(f.Function, "bolt/server.(*Server).Shutdown"):
			return boltDrainClosedByShutdown
		case strings.Contains(f.Function, "bolt/server.(*Server).Serve"):
			return boltDrainClosedByServeExit
		case strings.Contains(f.Function, "internal/sim.boltDrainCloseOutOfOrder"):
			return boltDrainClosedByHarness
		}
		if !more {
			return boltDrainClosedByUnknown
		}
	}
}

// boltDrainCloseOutOfOrder closes the store DIRECTLY, bypassing the server
// entirely. It exists as a NAMED function so [boltDrainAttribute] has a stable
// frame to recognise the control arm by, rather than matching on a method of an
// anonymous closure.
func boltDrainCloseOutOfOrder(c io.Closer) error { return c.Close() }

// -----------------------------------------------------------------------------
// Evidence
// -----------------------------------------------------------------------------

// BoltDrainCommit is one write attempt over the wire and what the client was
// told. Phase is what dates it against the teardown, which is the whole basis of
// the wire clause: a statement issued BEFORE the teardown must not be answered
// with a storage failure, while one issued after it legitimately is.
type BoltDrainCommit struct {
	// Name is the node name the statement would create.
	Name string
	// Phase is "pre" (issued and answered before the teardown began), "inflight"
	// (parked inside its WAL fsync when the teardown began) or "post" (issued after
	// the teardown).
	Phase string
	// RunCode is the failure code the RUN was answered with, "" for a SUCCESS.
	// RunAcked records an EXPLICIT [proto.Success], classified with [isSuccess]. For
	// an auto-commit write the RUN reply is the durability acknowledgement (see
	// [boltDrainAckIsRunSuccess]).
	RunCode  string
	RunAcked bool
	// RunIgnored records a [proto.Ignored] reply ([isIgnored]): the session was
	// already FAILED, so the statement was never dispatched and nothing was written.
	//
	// It has its own field because reading the acknowledgement as "not a FAILURE"
	// counts an IGNORED as a durable commit. That is not hypothetical: it is the
	// defect this file's FIRST oracle had, and it manufactured an ACID_DURABILITY
	// violation in 8 of 30 concurrent runs. The mechanism, measured: the shutdown
	// answered a committer's RUN with Neo.ClientError.Transaction.Terminated, which
	// puts the session in FAILED; the committer's NEXT RUN was answered IGNORED; the
	// harness recorded an acknowledgement; and the name it then demanded from
	// recovery had never been written at all — confirmed by finding it neither in the
	// live engine nor anywhere in the raw WAL image. bolt_auth_surface.go (rmp #2481)
	// added [isIgnored] against exactly this class of false positive and noted that
	// its own arms did not reach the branch; these arms do.
	RunIgnored bool
	// PullAcked records whether the client also drained the result stream. It is a
	// WITNESS, not a clause: a graceful Shutdown closes the connection as soon as
	// the in-flight statement's reply is flushed, so an acknowledged in-flight
	// commit routinely never gets its PULL through (measured; see the file
	// comment).
	PullAcked bool
	// PullCode is the failure code the PULL terminal carried, "" when there was
	// none, and PullIgnored records an IGNORED terminal.
	PullCode    string
	PullIgnored bool
	// Transport records that the round trip failed below the protocol (the
	// connection was closed under the client). Rendered as a boolean in the
	// evidence's String because the underlying text names a connection object.
	Transport bool
}

// BoltDrainEvidence is what one arm OBSERVED. It carries measurements and no
// verdicts, so the adjudicators below are pure functions of it and can be
// falsified by a doctored value rather than by hoping a real run misbehaves.
//
// It is passed by POINTER everywhere: the value is far over gocritic's hugeParam
// threshold, and nothing mutates it after the arm returns.
type BoltDrainEvidence struct {
	// Arm is the variant that produced this evidence; Seed is the seed it was
	// built from.
	Arm  string
	Seed uint64

	// CloserWired records that the server was actually given an
	// [server.Options.Closer]. Without it every ordering clause below is inert.
	CloserWired bool
	// ConnDecorated records that the connection tracker was installed, which is
	// what the ordering clause reads.
	ConnDecorated bool

	// CloseBodies is every invocation of the store's teardown, in call order. The
	// contract is exactly one.
	CloseBodies []boltDrainCloseBody
	// CloseBodiesWhileParked is how many teardown bodies had been entered while a
	// commit was still parked inside its WAL fsync AND Shutdown had demonstrably
	// closed the listener. It must be zero.
	CloseBodiesWhileParked int64
	// CloseBodiesAtShutdownReturn is how many teardown bodies had been entered at
	// the instant the FIRST Shutdown call returned. On an expiry arm it must be
	// zero: neither of Shutdown's failure branches closes the owned store
	// (bolt/server/serve.go:874-879), so the store is closed later, by Serve's exit
	// path — which is what makes the attribution clause below adjudicable there and
	// only a witness on the success path.
	CloseBodiesAtShutdownReturn int64
	// CloseFaultArmed records that a one-shot fsync fault was armed on the WAL
	// close, so the value the server caches is non-nil and its identity across
	// callers is a discriminating assertion.
	CloseFaultArmed bool

	// The four declarative echoes of the arm's configuration. They are in the
	// evidence so the adjudicators are pure functions of it and never re-derive an
	// expectation from the arm's NAME — which is what lets a falsifiability table
	// build a healthy value for any arm and perturb one field of it.
	//
	// ParkExpected: the arm intended to park a commit inside its fsync.
	// ExpiryExpected: the arm bounded Shutdown so it could not drain in time.
	// OutOfOrderArm: the arm closed the store directly, with no drain at all.
	// FleetArm: the arm drove concurrent committers and is not bit-reproducible.
	ParkExpected   bool
	ExpiryExpected bool
	OutOfOrderArm  bool
	FleetArm       bool

	// AckedAfterCheckpoint is how many acknowledged commits landed after the arm's
	// one checkpoint and therefore live ONLY in the WAL suffix. Without at least
	// one, every acked name could come back from the published snapshot and a WAL
	// close that flushed nothing would satisfy the durability clause — the same
	// measurement db_teardown.go records for its own arms.
	AckedAfterCheckpoint int

	// ConnsAccepted / ConnsClosed / ConnsPeak / ConnsLiveAtEnd are the connection
	// tracker's totals for the whole arm.
	ConnsAccepted   int64
	ConnsClosed     int64
	ConnsPeak       int64
	ConnsLiveAtEnd  int64
	IdleConnsOpened int

	// GateArmed records that a fsync rendezvous was armed, GateFired that it was
	// actually entered — the reachability observable rmp #2465 established, since
	// an ordinal that never matched is a silent no-op.
	GateArmed bool
	GateFired bool
	// ParkedLiveConns is how many server-side connections were live while the
	// commit was parked. ListenerClosedWhileParked records that the listener was
	// already closed in that window, which is what proves Shutdown had passed
	// ln.Close() and was inside its drain wait rather than not yet started.
	// DialRefusedAfterTeardown is the same observable from a client's point of view,
	// taken once the teardown is over (where a probe Dial can create nothing).
	ParkedLiveConns           int64
	ListenerClosedWhileParked bool
	DialRefusedAfterTeardown  bool

	// ShutdownErrs renders what each Shutdown call returned, in call order.
	ShutdownErrs []string
	// ShutdownFirstNil records whether the FIRST Shutdown drained cleanly, and
	// LastShutdownNil whether the LAST one did. They differ on the expiry arms: the
	// first reports the expiry, and the last — made after the drain finally
	// completed — observes the cached, successful close result.
	ShutdownFirstNil bool
	LastShutdownNil  bool
	// ShutdownCalls is how many Shutdown calls the arm made, and
	// DistinctShutdownErrs how many distinct error VALUES they returned — compared
	// by identity, never by class, because the class cannot tell one published
	// value from N re-derived ones.
	ShutdownCalls        int
	DistinctShutdownErrs int
	// ShutdownErrIsDrainTimeout / ShutdownErrIsCtx classify the FIRST call's error:
	// which of Shutdown's two failure branches was taken.
	ShutdownErrIsDrainTimeout bool
	ShutdownErrIsCtx          bool
	// ShutdownErrIsCloseFault records that the first call returned the store's own
	// close failure, which is what the publication arm asserts identity over.
	ShutdownErrIsCloseFault bool
	// ShutdownExpiryBudget is the bound the arm gave Shutdown (zero when it gave
	// none), and ShutdownExpiryByCancel records that the bound was an explicit
	// cancel on a deadline-free context rather than a deadline.
	ShutdownExpiryBudget   time.Duration
	ShutdownExpiryByCancel bool
	// ServeExitErr renders what [server.Server.Serve] returned, and
	// ServeExitErrIsCloseFault whether it carried the store's close failure —
	// which is how Serve's exit path is shown to observe the SAME cached result
	// without comparing a joined error by pointer.
	ServeExitErr             string
	ServeExitErrIsCloseFault bool

	// Commits is every write attempt over the wire, in the order it was made.
	Commits []BoltDrainCommit
	// InterruptedRuns and TerminatedRuns count the writes answered with each of the
	// two codes a shutdown-interrupted statement can receive. They are MEASUREMENTS,
	// carried so the difference between them is visible in a report: one is transient
	// and a driver retries it, the other is demoted to a client error and is never
	// retried (see [boltDrainTerminatedCode]).
	InterruptedRuns int
	TerminatedRuns  int
	// IgnoredReplies counts the RUN and PULL replies that were IGNORED because the
	// session was already FAILED. Such a reply is never an acknowledgement and never
	// enters the acked set; it is counted so a run in which the arms measured nothing
	// but ignored traffic is visible rather than silently green.
	IgnoredReplies int

	// PostCloseCommitErr renders what a commit attempted through the transaction
	// layer AFTER the teardown returned, and PostCloseCommitRefused whether it was
	// refused with [wal.ErrWriterClosed]. This is the one place the writer-closed
	// error is identifiable at all (over the wire it is sanitised), so it is both
	// the proof the WAL closed and the proof the detector is not blind.
	PostCloseCommitErr     string
	PostCloseCommitRefused bool

	// LoopAliveBeforeTeardown is the liveness probe taken BEFORE the teardown (a
	// checkpoint request succeeded, so there was a goroutine to join), and
	// LoopStoppedAfterTeardown the join verdict (a request afterwards returned
	// [checkpoint.ErrCheckpointerStopped]). A WAL-closed error instead would mean
	// the loop outlived the WAL — the failure the composed teardown exists to
	// prevent.
	LoopAliveBeforeTeardown  bool
	LoopStoppedAfterTeardown bool

	// AckedNames are the names whose RUN was acknowledged, IssuedNames every name
	// the arm sent, RecoveredNames what a reopen through real recovery found.
	// MissingAcked is acked minus recovered (a durability defect) and
	// PhantomNames recovered minus issued (a consistency defect).
	AckedNames     []string
	IssuedNames    []string
	RecoveredNames []string
	MissingAcked   []string
	PhantomNames   []string
	// PartialNames are recovered nodes missing their age property: a torn
	// transaction resurrected by recovery.
	PartialNames []string
	// RecoveredWALOps is how many WAL ops the reopen's recovery replayed. It is
	// what stops the durability clause being answered by a snapshot underneath it,
	// and it is a pure function of the seed: an op count does not depend on how wide
	// any encoded value happened to be.
	RecoveredWALOps int
	// WALBytes is the size of the durable WAL image the reopen read. It is a
	// DIAGNOSTIC ONLY and deliberately absent from [BoltDrainEvidence.String] and
	// from every violation message: a created node's hidden internal key is minted
	// as "__cx_"+hex(n) from a PROCESS-GLOBAL counter (cypher/exec/create_node.go)
	// and travels inside the WAL frame, so the same seed produces frames of
	// different widths depending on how many nodes every other test in the process
	// created first. The same limitation, from the same counter, is documented in
	// bolt_auth_surface.go and schema_mutation.go.
	WALBytes int
	// SnapshotPublished records whether a snapshot manifest sits beside the WAL,
	// and ReopenClean whether recovery reported a clean image.
	SnapshotPublished bool
	ReopenClean       bool
}

// String renders the evidence for a failure message or a test log.
//
// It is deliberately ID-FREE: no session id, no error text that embeds one, no
// wall-clock duration and no goroutine identity, so two runs of one seed produce
// byte-identical output. The sanitised failure message a storage error produces
// carries a crypto-random session id (bolt/server/session.go:530), which is why
// only the CODE is rendered.
func (e *BoltDrainEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bolt-shutdown-drain evidence (arm=%s seed=%#x):", e.Arm, e.Seed)
	fmt.Fprintf(&b, "\n  wiring: closer=%t decorated=%t idle-conns=%d gate=armed:%t/fired:%t",
		e.CloserWired, e.ConnDecorated, e.IdleConnsOpened, e.GateArmed, e.GateFired)
	fmt.Fprintf(&b, "\n  conns: accepted=%d closed=%d peak=%d live-at-end=%d parked-live=%d listener-closed-while-parked=%t dial-refused-after=%t",
		e.ConnsAccepted, e.ConnsClosed, e.ConnsPeak, e.ConnsLiveAtEnd, e.ParkedLiveConns,
		e.ListenerClosedWhileParked, e.DialRefusedAfterTeardown)
	fmt.Fprintf(&b, "\n  teardown: bodies=%d while-parked=%d", len(e.CloseBodies), e.CloseBodiesWhileParked)
	for i := range e.CloseBodies {
		body := &e.CloseBodies[i]
		fmt.Fprintf(&b, "\n    body[%d] by=%-34s accepted=%d closed=%d live=%d after-shutdown-returned=%t",
			i, e.renderAttribution(body.Attribution), body.AcceptedAt, body.ClosedAt, body.LiveAt,
			body.AfterShutdownReturned)
	}
	fmt.Fprintf(&b, "\n  shutdown: calls=%d distinct-errs=%d first-nil=%t drain-timeout=%t ctx=%t close-fault=%t budget=%s by-cancel=%t",
		e.ShutdownCalls, e.DistinctShutdownErrs, e.ShutdownFirstNil, e.ShutdownErrIsDrainTimeout,
		e.ShutdownErrIsCtx, e.ShutdownErrIsCloseFault, e.ShutdownExpiryBudget, e.ShutdownExpiryByCancel)
	fmt.Fprintf(&b, "\n    errs=%q", e.ShutdownErrs)
	fmt.Fprintf(&b, "\n  serve exit: err=%q close-fault=%t", e.ServeExitErr, e.ServeExitErrIsCloseFault)
	b.WriteString("\n  commits:")
	for i := range e.Commits {
		c := &e.Commits[i]
		fmt.Fprintf(&b, "\n    %-10s %-34s run=%-7s pull=%-7s run-code=%-44s pull-code=%-44s transport=%t",
			c.Phase, c.Name, boltDrainReplyVerdict(c.RunAcked, c.RunIgnored),
			boltDrainReplyVerdict(c.PullAcked, c.PullIgnored),
			quoteOrDash(c.RunCode), quoteOrDash(c.PullCode), c.Transport)
	}
	fmt.Fprintf(&b, "\n  shutdown-interrupted replies: transient=%d client-terminated=%d ignored=%d",
		e.InterruptedRuns, e.TerminatedRuns, e.IgnoredReplies)
	fmt.Fprintf(&b, "\n  store witness: post-close-commit=%q refused-writer-closed=%t",
		e.PostCloseCommitErr, e.PostCloseCommitRefused)
	fmt.Fprintf(&b, "\n  checkpoint loop: alive-before=%t stopped-after=%t",
		e.LoopAliveBeforeTeardown, e.LoopStoppedAfterTeardown)
	fmt.Fprintf(&b, "\n  durability: issued=%d acked=%d recovered=%d missing=%v phantom=%v partial=%v",
		len(e.IssuedNames), len(e.AckedNames), len(e.RecoveredNames), e.MissingAcked, e.PhantomNames, e.PartialNames)
	fmt.Fprintf(&b, "\n    wal-image-nonempty=%t wal-ops-replayed=%d snapshot=%t reopen-clean=%t",
		e.WALBytes > 0, e.RecoveredWALOps, e.SnapshotPublished, e.ReopenClean)
	return b.String()
}

// renderAttribution renders a close body's caller for the evidence.
//
// On the drain-SUCCESS path the caller is a genuine RACE: Shutdown cancels the
// accept context before it drains, so Serve's deferred exit path and Shutdown's
// drain-success branch end up waiting on the same WaitGroup and either may reach
// closeOwned first (measured over 25 successful drains: 22 Serve, 3 Shutdown).
// Rendering the winner would make the evidence vary run to run and break the
// bit-reproducibility the deterministic arms claim, so both legal winners collapse
// to ONE token there.
//
// The value is rendered verbatim where the path is determined — on an expiry arm,
// whose failure branches cannot close the store at all, and on the control arm,
// which closes it itself — and verbatim whenever it is a value this oracle does not
// model, so an unrecognised caller is never hidden by the collapse.
func (e *BoltDrainEvidence) renderAttribution(a string) string {
	if e.ExpiryExpected || e.OutOfOrderArm {
		return a
	}
	if a == boltDrainClosedByServeExit || a == boltDrainClosedByShutdown {
		return "drained-path(serve-exit|shutdown)"
	}
	return a
}

// boltDrainReplyVerdict renders one reply's classification.
func boltDrainReplyVerdict(acked, ignored bool) string {
	switch {
	case acked:
		return "SUCCESS"
	case ignored:
		return "IGNORED"
	default:
		return "-"
	}
}

// quoteOrDash renders a code for the evidence, mapping the empty string to a dash
// so a column of codes stays legible.
func quoteOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// -----------------------------------------------------------------------------
// The arm
// -----------------------------------------------------------------------------

// BoltDrainConfig parameterises one arm. The zero value is not usable:
// [RunBoltShutdownDrain] normalises Arm and derives the rest of the geometry from
// it, and the flags below are what a control varies.
type BoltDrainConfig struct {
	// Seed is the master seed. Every node name and the SimDisk sub-stream derive
	// from it.
	Seed uint64
	// Arm names the variant (see the ArmBoltDrain* constants).
	Arm string
	// ParkCommit arms a [SyncGate] on the next fsync and holds one commit inside
	// it, so the teardown genuinely races an in-flight writer.
	ParkCommit bool
	// ShutdownBudget bounds the first Shutdown call. Zero passes
	// [context.Background], so Shutdown drains to completion.
	ShutdownBudget time.Duration
	// ExpiryByCancel bounds Shutdown with an explicit cancel on a context that has
	// NO deadline, instead of with a deadline. It is the only way to reach
	// Shutdown's `<-ctx.Done()` branch: with a deadline, Shutdown's own clamped
	// time.After wins (measured 12/12; see the file comment).
	ExpiryByCancel bool
	// ShutdownCalls is how many times Shutdown is called (minimum 1). Two is what
	// makes the published-identity clause a statement about the server's sync.Once.
	ShutdownCalls int
	// FailClose arms a one-shot fsync fault on the WAL close, so the store's
	// teardown FAILS and the value the server caches is non-nil.
	FailClose bool
	// CloseOutOfOrder closes the store DIRECTLY, with a live client mid-session and
	// no drain at all. It is the sensitivity seam: it reproduces what the ordering
	// exists to prevent, so the ordering and wire clauses are proved to fire.
	CloseOutOfOrder bool
	// Fleet drives several concurrent committers instead of the deterministic
	// single-commit geometry. NOT bit-reproducible.
	Fleet bool
	// SkipCheckpointer stands the store up with NO checkpointer at all: no loop, no
	// published snapshot, and recovery through WAL replay alone. It is the A/B seam
	// for attributing a lost commit to the checkpoint interaction rather than to the
	// drain, and it is deliberately a configuration rather than a separate code path
	// so the two sides differ in exactly one thing.
	//
	// It is not a shipped arm: with no snapshot and no loop to join, the coverage gate
	// fires on nonvacuity-snapshot and nonvacuity-loop by design.
	SkipCheckpointer bool
}

// boltDrainArmConfig returns the canonical configuration for a named arm, so the
// scenario, the tests and a control all build the same geometry from one place.
// An unknown arm returns false rather than a silently defaulted config.
func boltDrainArmConfig(arm string, seed uint64) (BoltDrainConfig, bool) {
	cfg := BoltDrainConfig{Seed: seed, Arm: arm, ShutdownCalls: 2}
	switch arm {
	case ArmBoltDrainOrdered:
		cfg.ParkCommit = true
	case ArmBoltDrainExpiryDrainTimeout:
		cfg.ParkCommit = true
		cfg.ShutdownBudget = boltDrainExpiryBudget
	case ArmBoltDrainExpiryCtxCancel:
		cfg.ParkCommit = true
		cfg.ShutdownBudget = boltDrainExpiryBudget
		cfg.ExpiryByCancel = true
	case ArmBoltDrainOnce:
		cfg.FailClose = true
	case ArmBoltDrainUnordered:
		cfg.CloseOutOfOrder = true
		cfg.ShutdownCalls = 0 // the control never calls Shutdown: that is the point
	case ArmBoltDrainFleet:
		cfg.Fleet = true
		cfg.ShutdownCalls = 1
	default:
		return BoltDrainConfig{}, false
	}
	return cfg, true
}

// boltDrainArms is the deterministic roster [runBoltShutdownDrainScenario] drives,
// in order. The control and the fleet arm are deliberately absent: the control is
// expected to FAIL these clauses (it is driven from the test, which asserts it
// does), and the fleet arm is not bit-reproducible and has its own scenario.
var boltDrainArms = []string{
	ArmBoltDrainOrdered,
	ArmBoltDrainExpiryDrainTimeout,
	ArmBoltDrainExpiryCtxCancel,
	ArmBoltDrainOnce,
}

// boltDrainRunner owns one arm's durable stack, its server, and the evidence it
// is filling in.
type boltDrainRunner struct {
	cfg      BoltDrainConfig
	scfg     simStoreConfig
	disk     *SimDisk
	st       *SimStore
	cp       *checkpoint.Checkpointer[string, float64]
	cpCancel context.CancelFunc
	db       *store.DB
	probe    *boltDrainCloser
	conns    *boltDrainConnTracker
	srv      *SimServer
	seed     *Seed
	ev       *BoltDrainEvidence
	idle     []*WireClient
	gate     *SyncGate
	// shutdownErrs are the errors every Shutdown call returned, in call order. They
	// are kept as VALUES (not rendered text) because the identity clause compares
	// them by pointer.
	shutdownErrs []error
	// fleetMu guards the evidence's issued/acked slices, which several concurrent
	// committers append to in the fleet arm. Every deterministic arm appends from
	// one goroutine and never takes it.
	fleetMu sync.Mutex
}

// RunBoltShutdownDrain drives one arm end to end: stand up a WAL-backed store
// with a running checkpointer, hand it to a real Bolt server as its
// [server.Options.Closer], acknowledge a durable prefix over the genuine wire,
// tear the server down as the arm's configuration dictates, and reopen the
// SimDisk image through real recovery.
//
// It returns evidence and no verdict; adjudicate it with [checkBoltShutdownDrain]
// and [checkBoltShutdownDrainNonVacuity]. An error means the arm could not be
// DRIVEN, which is a harness failure and not a report.
func RunBoltShutdownDrain(ctx context.Context, cfg BoltDrainConfig) (*BoltDrainEvidence, error) {
	if cfg.Arm == "" {
		cfg.Arm = ArmBoltDrainOrdered
	}
	if cfg.ShutdownCalls < 1 {
		cfg.ShutdownCalls = 1
	}
	r, err := openBoltDrainRunner(cfg)
	if err != nil {
		return nil, err
	}
	defer r.release()
	if err := r.drive(ctx); err != nil {
		return nil, err
	}
	return r.ev, nil
}

// openBoltDrainRunner stands up the durable stack: a full-stack store on a fresh
// SimDisk, a checkpointer started over it with the same seams
// [SimStore.Checkpoint] uses, the composed [store.DB] that owns their teardown,
// and a server that owns the DB.
//
// The checkpointer's Config carries no MaxAge and no Interval, so the loop fires
// only on an explicit Trigger. That is what makes the fsync ordinals exact: no
// background publish can consume the Sync a gate or a fault is waiting on.
func openBoltDrainRunner(cfg BoltDrainConfig) (*boltDrainRunner, error) {
	scfg := fullStackStoreConfig()
	disk := NewSimDisk(NewSeed(cfg.Seed^boltDrainDiskSeedMix), 0) // faultRate 0: only an armed fault fires
	st, err := OpenSimStore(disk, scfg)
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-drain[%s] open store: %w", cfg.Arm, err)
	}

	var cp *checkpoint.Checkpointer[string, float64]
	var cpCancel context.CancelFunc
	if !cfg.SkipCheckpointer {
		var unusedMu sync.Mutex
		cp = checkpoint.New[string, float64](
			checkpoint.Config{Dir: scfg.dir}, st.graph, st.wlog, &unusedMu,
			checkpoint.WithCommitSerialiser[string, float64](st.store.RunUnderCommitLock),
			checkpoint.WithMapperCodec[string, float64](st.store.Codec()),
			checkpoint.WithWeightCodec[string, float64](st.store.WeightCodec()),
			checkpoint.WithSnapshotFS[string, float64](simCheckpointBackend[string, float64]{disk: disk}),
			checkpoint.WithConstraintSpecs[string, float64](st.engine.ConstraintSpecsForSnapshot),
			checkpoint.WithIndexSpecs[string, float64](st.engine.IndexSpecsForSnapshot),
		)
		var cpCtx context.Context
		cpCtx, cpCancel = context.WithCancel(context.Background())
		cp.Start(cpCtx)
	}

	// The crash-safe teardown owner, wired exactly as an embedder would: stop the
	// checkpoint goroutine, then close the WAL under the store's commit-lock
	// quiesce. No WithFinalCheckpoint: this arm's subject is the WAL close, and a
	// final checkpoint would fold the whole WAL suffix into a fresh snapshot and
	// leave the durability clause answered by the snapshot instead (the same
	// measurement db_teardown.go records for its own arms).
	dbOpts := []store.Option{store.WithQuiesce(st.store.RunUnderCommitLock)}
	if cp != nil {
		dbOpts = append(dbOpts, store.WithCheckpointer(cp))
	}
	db := store.New(st.wlog, dbOpts...)

	conns := &boltDrainConnTracker{}
	probe := &boltDrainCloser{inner: db, conns: conns, failCloses: cfg.FailClose}
	srv, err := NewSimServerOwnedCloser(st.Engine(), clock.Real(), probe, conns.wrap)
	if err != nil {
		if cpCancel != nil {
			cpCancel()
		}
		_ = st.Close()
		return nil, fmt.Errorf("sim: bolt-drain[%s] server: %w", cfg.Arm, err)
	}

	return &boltDrainRunner{
		cfg: cfg, scfg: scfg, disk: disk, st: st, cp: cp, cpCancel: cpCancel,
		db: db, probe: probe, conns: conns, srv: srv,
		seed: NewSeed(cfg.Seed),
		ev: &BoltDrainEvidence{
			Arm: cfg.Arm, Seed: cfg.Seed,
			CloserWired:            true,
			ConnDecorated:          true,
			ShutdownExpiryBudget:   cfg.ShutdownBudget,
			ShutdownExpiryByCancel: cfg.ExpiryByCancel,
			ParkExpected:           cfg.ParkCommit,
			ExpiryExpected:         cfg.ShutdownBudget > 0,
			OutOfOrderArm:          cfg.CloseOutOfOrder,
			FleetArm:               cfg.Fleet,
		},
	}, nil
}

// release tears down everything the arm owns that the teardown under test did not
// already close. Every call is idempotent, so it is safe after a clean run.
func (r *boltDrainRunner) release() {
	for _, c := range r.idle {
		_ = c.Close()
	}
	if r.gate != nil {
		r.gate.Release()
	}
	_ = r.srv.Close()
	if r.cp != nil {
		r.cp.Stop()
	}
	if r.cpCancel != nil {
		r.cpCancel()
	}
	_ = r.db.Close()
	_ = r.st.Close()
}

// name mints one deterministic node name. Every draw comes from the arm's own
// [Seed] in a fixed order, so the whole name set is a pure function of the run
// seed.
func (r *boltDrainRunner) name(phase string, i int) string {
	return fmt.Sprintf("%s-%s-%02d-%08x", r.cfg.Arm, phase, i, r.seed.Uint64N(1<<32))
}

// drive runs the arm.
func (r *boltDrainRunner) drive(ctx context.Context) error {
	if r.cfg.Fleet {
		return r.driveFleet(ctx)
	}
	return r.driveDeterministic(ctx)
}

// driveDeterministic runs the single-committer geometry: a durable prefix, some
// idle connections, optionally one commit parked inside its fsync, then the
// teardown the arm's configuration selects.
//
// The ORDER of the last four steps is what makes each arm terminate. On an arm
// with no expiry budget, Shutdown cannot return until the parked commit is
// released, so the release comes first and the wait second; on an expiry arm
// Shutdown returns on its own WHILE the commit is still parked, which is the whole
// point, so the wait comes first and the release second. Getting this backwards
// does not produce a wrong answer, it produces a 30-second hang — which is why the
// two orders are spelled out rather than unified.
func (r *boltDrainRunner) driveDeterministic(ctx context.Context) error {
	if err := r.commitPrefix(ctx); err != nil {
		return err
	}
	r.probeLoopAlive()
	if err := r.commitWALSuffix(ctx); err != nil {
		return err
	}
	if err := r.openIdleConns(ctx); err != nil {
		return err
	}

	if r.cfg.CloseOutOfOrder {
		return r.driveOutOfOrder(ctx)
	}

	parked, err := r.parkOneCommit(ctx)
	if err != nil {
		return err
	}
	if r.cfg.FailClose {
		// The WAL close's own fsync is the next Sync on this disk: the checkpointer
		// fires only on an explicit Trigger, every prefix commit has been
		// acknowledged, and the idle connections issue no statements, so nothing else
		// can claim the ordinal.
		r.disk.ArmSyncFaultAt(1)
		r.ev.CloseFaultArmed = true
	}

	call := r.startShutdown()
	switch {
	case parked == nil:
		r.awaitShutdown(call)
	case r.cfg.ShutdownBudget > 0:
		r.observeParkedOrdering()
		r.awaitShutdown(call)
		r.releaseParked(parked)
	default:
		r.observeParkedOrdering()
		r.releaseParked(parked)
		r.awaitShutdown(call)
	}

	r.closeIdleConns()
	r.joinServe()
	r.extraShutdowns()
	return r.finish(ctx)
}

// boltDrainShutdownCall is one in-flight Shutdown invocation.
type boltDrainShutdownCall struct {
	out     chan error
	cleanup func()
}

// startShutdown calls Shutdown on its own goroutine, under the bound the arm's
// configuration asks for, and returns before it has finished. It has to be
// asynchronous: on the drain-success arms Shutdown blocks until the parked commit
// is released, and the ordering observation has to be taken while it is blocked.
//
// The bound's parent is [context.Background] and never the scenario's own context.
// That is deliberate: an arm with no expiry budget must drain to completion, and
// inheriting a scenario deadline would silently turn it into an expiry arm the
// moment the run got slow — the arms would still pass, while measuring something
// else.
func (r *boltDrainRunner) startShutdown() *boltDrainShutdownCall {
	sctx, cleanup := r.shutdownBound()
	call := &boltDrainShutdownCall{out: make(chan error, 1), cleanup: cleanup}
	go func() { call.out <- r.srv.Shutdown(sctx) }()
	return call
}

// shutdownBound builds the context for the arm's first Shutdown call, plus its
// cleanup.
//
// With ExpiryByCancel the context carries NO DEADLINE and is cancelled by a timer
// instead. That is the only shape that reaches Shutdown's `<-ctx.Done()` branch: a
// deadline is clamped into Shutdown's own drain timeout
// (bolt/server/serve.go:861-867), whose time.After is armed from a marginally
// earlier reading and therefore wins the select — measured 12/12 (see the file
// comment).
func (r *boltDrainRunner) shutdownBound() (context.Context, func()) {
	switch {
	case r.cfg.ShutdownBudget <= 0:
		return context.Background(), func() {}
	case r.cfg.ExpiryByCancel:
		sctx, cancel := context.WithCancel(context.Background())
		// time.AfterFunc runs on a runtime-managed timer, so the bound costs no
		// goroutine of this package's own for goleak to find.
		timer := time.AfterFunc(r.cfg.ShutdownBudget, cancel)
		return sctx, func() { timer.Stop(); cancel() }
	default:
		return context.WithTimeout(context.Background(), r.cfg.ShutdownBudget)
	}
}

// awaitShutdown joins the call and records what it returned, together with the
// teardown-body count AT THAT INSTANT.
//
// That count is the clause that pins Shutdown's two failure branches: neither
// closes the owned store (closeOwned is called only from the drain-success branch,
// bolt/server/serve.go:874), so on an expiry arm it must read zero and the store
// must still be closed later, by Serve's exit path.
func (r *boltDrainRunner) awaitShutdown(call *boltDrainShutdownCall) {
	defer call.cleanup()
	var first error
	select {
	case first = <-call.out:
	case <-time.After(boltDrainJoinTimeout):
		first = fmt.Errorf("sim: bolt-drain[%s] Shutdown did not return within %s", r.cfg.Arm, boltDrainJoinTimeout)
	}
	r.ev.CloseBodiesAtShutdownReturn = r.probe.Runs()
	r.probe.shutdownReturned.Store(true)

	r.shutdownErrs = append(r.shutdownErrs, first)
	r.ev.ShutdownFirstNil = first == nil
	r.ev.ShutdownErrIsDrainTimeout = boltDrainIsDrainTimeout(first)
	r.ev.ShutdownErrIsCtx = errors.Is(first, context.Canceled) || errors.Is(first, context.DeadlineExceeded)
	r.ev.ShutdownErrIsCloseFault = errors.Is(first, errBoltDrainCloseFault)
}

// extraShutdowns makes the arm's remaining Shutdown calls, after the drain has
// completed and the store has been torn down, and publishes the identity summary.
//
// They run LAST on purpose. A second Shutdown issued while the first is still
// draining would block on the same WaitGroup and turn every arm into a
// thirty-second wait; issued here it exercises the property that actually matters
// — a late or repeated Shutdown observes the SAME cached close result and never
// runs the teardown body again.
func (r *boltDrainRunner) extraShutdowns() {
	for range max(r.cfg.ShutdownCalls-1, 0) {
		r.shutdownErrs = append(r.shutdownErrs, r.srv.Shutdown(context.Background()))
	}
	r.ev.ShutdownCalls = len(r.shutdownErrs)
	r.ev.ShutdownErrs = make([]string, 0, len(r.shutdownErrs))
	for _, err := range r.shutdownErrs {
		r.ev.ShutdownErrs = append(r.ev.ShutdownErrs, errText(err))
	}
	r.ev.DistinctShutdownErrs = countDistinctErrors(r.shutdownErrs)
	r.ev.LastShutdownNil = len(r.shutdownErrs) > 0 && r.shutdownErrs[len(r.shutdownErrs)-1] == nil
}

// driveOutOfOrder is the sensitivity control: close the store with a client live
// and no drain at all, then ask that client to commit. It is what proves the
// ordering clause and the wire clause can fail.
func (r *boltDrainRunner) driveOutOfOrder(ctx context.Context) error {
	c, err := r.dialReady(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if cerr := boltDrainCloseOutOfOrder(r.probe); cerr != nil {
		r.ev.ShutdownErrs = append(r.ev.ShutdownErrs, errText(cerr))
	}
	r.ev.Commits = append(r.ev.Commits, r.writeOverWire(c, boltDrainPhaseUndrained, r.name("undrained", 0)))
	r.closeIdleConns()
	r.joinServe()
	return r.finish(ctx)
}

// commitPrefix acknowledges [boltDrainPreCommits] auto-commit writes over the
// genuine wire, each on its own connection, so the durability clause has
// something to be answered by.
func (r *boltDrainRunner) commitPrefix(ctx context.Context) error {
	for i := range boltDrainPreCommits {
		c, err := r.dialReady(ctx)
		if err != nil {
			return err
		}
		row := r.writeOverWire(c, boltDrainPhasePre, r.name("pre", i))
		if !row.PullAcked {
			_ = c.Close()
			return fmt.Errorf("sim: bolt-drain[%s] prefix commit %d was not acknowledged: "+
				"run-acked=%t run-ignored=%t run-code=%q pull-code=%q transport=%t "+
				"(the acknowledgement is the TERMINAL reply, not the RUN reply)",
				r.cfg.Arm, i, row.RunAcked, row.RunIgnored, row.RunCode, row.PullCode, row.Transport)
		}
		r.ev.Commits = append(r.ev.Commits, row)
		_ = c.Goodbye()
		if err := c.Close(); err != nil {
			return fmt.Errorf("sim: bolt-drain[%s] prefix close %d: %w", r.cfg.Arm, i, err)
		}
	}
	return nil
}

// commitWALSuffix acknowledges one write AFTER the arm's checkpoint, so at least
// one acknowledged name lives only in the WAL suffix that the teardown's WAL close
// is responsible for.
//
// It matters because [boltDrainRunner.probeLoopAlive] triggers a REAL checkpoint:
// [checkpoint.Checkpointer.Trigger] blocks until the checkpoint completes
// (store/checkpoint/checkpoint.go:494-501, TriggerCtx's contract), publishing a
// snapshot and truncating the WAL prefix. Without this commit every prefix name
// would come back from that snapshot and the durability clause would say nothing
// about the WAL at all.
func (r *boltDrainRunner) commitWALSuffix(ctx context.Context) error {
	c, err := r.dialReady(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	row := r.writeOverWire(c, boltDrainPhaseWALSuffix, r.name("walsuffix", 0))
	if !row.PullAcked {
		return fmt.Errorf("sim: bolt-drain[%s] post-checkpoint commit was not acknowledged: "+
			"run-acked=%t run-code=%q pull-code=%q transport=%t",
			r.cfg.Arm, row.RunAcked, row.RunCode, row.PullCode, row.Transport)
	}
	r.ev.Commits = append(r.ev.Commits, row)
	r.ev.AckedAfterCheckpoint++
	_ = c.Goodbye()
	return nil
}

// openIdleConns opens connections that authenticate and then go silent. Each is a
// live handler goroutine registered in the server's drain WaitGroup, so the
// teardown has something to drain even in an arm with no in-flight statement.
func (r *boltDrainRunner) openIdleConns(ctx context.Context) error {
	for i := range boltDrainIdleConns {
		c, err := r.dialReady(ctx)
		if err != nil {
			return fmt.Errorf("sim: bolt-drain[%s] idle conn %d: %w", r.cfg.Arm, i, err)
		}
		r.idle = append(r.idle, c)
	}
	r.ev.IdleConnsOpened = len(r.idle)
	return nil
}

// closeIdleConns closes the idle client ends. It is a no-op on a server that has
// already torn them down.
func (r *boltDrainRunner) closeIdleConns() {
	for _, c := range r.idle {
		_ = c.Close()
	}
	r.idle = nil
}

// dialReady opens one connection and drives the handshake, HELLO and (on Bolt
// 5.1+) LOGON, leaving it ready for RUN.
func (r *boltDrainRunner) dialReady(ctx context.Context) (*WireClient, error) {
	c, err := r.srv.Dial()
	if err != nil {
		return nil, fmt.Errorf("sim: bolt-drain[%s] dial: %w", r.cfg.Arm, err)
	}
	if err := c.Connect(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("sim: bolt-drain[%s] connect: %w", r.cfg.Arm, err)
	}
	return c, nil
}

// boltDrainCreate is the auto-commit write every arm issues.
func boltDrainCreate(name string) string {
	return fmt.Sprintf("CREATE (:%s {name: %q, age: 1})", boltDrainLabel, name)
}

// writeOverWire issues one auto-commit write and records exactly what the client
// was told. It never fails the arm: what the client is told IS the observation.
func (r *boltDrainRunner) writeOverWire(c *WireClient, phase, name string) BoltDrainCommit {
	row := BoltDrainCommit{Name: name, Phase: phase}
	r.ev.IssuedNames = append(r.ev.IssuedNames, name)
	resp, err := c.Run(boltDrainCreate(name), nil)
	if err != nil {
		row.Transport = true
		return row
	}
	boltDrainClassifyRun(&row, resp)
	if !row.RunAcked {
		return row
	}
	_, term, err := c.PullAll()
	if err != nil {
		row.Transport = true
		return row
	}
	boltDrainClassifyPull(&row, term)
	if row.PullAcked {
		r.ev.AckedNames = append(r.ev.AckedNames, name)
	}
	return row
}

// boltDrainClassifyRun records what the RUN reply WAS, positively.
//
// The acknowledgement is an explicit [proto.Success] and nothing else. Treating
// "not a FAILURE" as an acknowledgement counts a [proto.Ignored] — the reply a
// session in FAILED gives every request-phase message until it is RESET — as a
// durable commit, which is exactly how this file's first oracle manufactured a
// durability violation (see [BoltDrainCommit.RunIgnored]).
func boltDrainClassifyRun(row *BoltDrainCommit, resp any) {
	switch {
	case isSuccess(resp):
		row.RunAcked = true
	case isIgnored(resp):
		row.RunIgnored = true
	default:
		row.RunCode = failureCode(resp)
	}
}

// boltDrainClassifyPull records what the PULL terminal WAS, on the same principle,
// and it is the reply the durability oracle is answered against.
//
// # A RUN SUCCESS is NOT a durability acknowledgement
//
// It is necessary but not sufficient, and the distinction is not cosmetic — it was
// the second of two harness defects this file produced (see the file comment).
// VERIFIED in bolt/server/session.go:
//
//   - handleRun replies SUCCESS whenever the engine call returned no error and
//     NEVER consults the result's own error (session.go:1185-1229). Its metadata is
//     "fields", "qid" and "db" — a statement-ACCEPTED reply, not an outcome.
//   - the BOOKMARK, which is the causal-consistency token a driver uses to
//     establish that a write landed, is delivered only on the TERMINAL reply:
//     under `if !hasMore` on PULL (session.go:1393-1397), the same on DISCARD
//     (:1500), and on COMMIT (:1696). It is absent from the RUN reply.
//   - the engine records a commit failure on the Result rather than returning it:
//     commitUnderBarrier has no return value and its second statement is an early
//     return on `r.rs.Err() != nil || r.rowsErr != nil` (cypher/api.go:5639-5641),
//     so a statement whose materialise failed appends no WAL frame and makes
//     nothing visible.
//
// Those three together are exactly what was measured: a fleet committer cut short
// by the shutdown got a RUN SUCCESS, its terminal never arrived, and its name was
// in neither the live engine nor anywhere in the raw WAL image. Nothing was made
// durable, so nothing acknowledged was lost — the harness was reading a
// non-terminal reply as an acknowledgement.
//
// RunAcked and RunIgnored are kept as recorded WITNESSES, because they are what
// lets a report tell "never dispatched" (IGNORED) from "dispatched, outcome unknown
// to the client" (RUN SUCCESS, no terminal).
func boltDrainClassifyPull(row *BoltDrainCommit, term any) {
	switch {
	case isSuccess(term):
		row.PullAcked = true
	case isIgnored(term):
		row.PullIgnored = true
	default:
		row.PullCode = failureCode(term)
	}
}

// boltDrainParked is one commit held inside its WAL fsync, plus the client that
// issued it and the channel its outcome arrives on.
type boltDrainParked struct {
	client *WireClient
	gate   *SyncGate
	out    chan BoltDrainCommit
}

// parkOneCommit arms a fsync rendezvous and issues one auto-commit write that
// parks inside it, returning once the gate has been ENTERED. The write runs on
// its own goroutine because the client is blocked in a round trip while parked;
// the gate — not a sleep and not the goroutine's scheduling — is the barrier, so
// the arm stays deterministic.
//
// It returns (nil, nil) when the arm asked for no parked commit.
func (r *boltDrainRunner) parkOneCommit(ctx context.Context) (*boltDrainParked, error) {
	if !r.cfg.ParkCommit {
		return nil, nil
	}
	c, err := r.dialReady(ctx)
	if err != nil {
		return nil, err
	}
	// Arm on the NEXT Sync. Nothing else can claim it: the checkpointer is
	// trigger-only, the prefix commits have all been acknowledged, and the idle
	// connections issue no statements.
	gate := r.disk.ArmSyncGateAt(1)
	if gate == nil {
		_ = c.Close()
		return nil, fmt.Errorf("sim: bolt-drain[%s] ArmSyncGateAt returned no gate", r.cfg.Arm)
	}
	r.gate = gate
	r.ev.GateArmed = true

	name := r.name("inflight", 0)
	out := make(chan BoltDrainCommit, 1)
	go func() { out <- r.writeOverWire(c, boltDrainPhaseInFlight, name) }()

	select {
	case <-gate.Reached():
	case <-time.After(boltDrainJoinTimeout):
		gate.Release()
		<-out
		_ = c.Close()
		return nil, fmt.Errorf("sim: bolt-drain[%s] fsync gate never reached (sync count %d)",
			r.cfg.Arm, r.disk.SyncCount())
	}
	r.ev.GateFired = gate.Fired()
	r.ev.ParkedLiveConns = r.conns.live.Load()
	return &boltDrainParked{client: c, gate: gate, out: out}, nil
}

// releaseParked unblocks the parked fsync, joins the committer and records its
// outcome.
func (r *boltDrainRunner) releaseParked(p *boltDrainParked) {
	p.gate.Release()
	select {
	case row := <-p.out:
		r.ev.Commits = append(r.ev.Commits, row)
	case <-time.After(boltDrainJoinTimeout):
		r.ev.Commits = append(r.ev.Commits, BoltDrainCommit{
			Phase: boltDrainPhaseInFlight, Name: "<never-returned>", Transport: true,
		})
	}
	_ = p.client.Close()
}

// observeParkedOrdering takes the negative observation: across a bounded window in
// which Shutdown has demonstrably closed the listener, the store's teardown has not
// been entered even once.
//
// The listener state is read directly ([SimListener.Closed]) rather than probed
// with a Dial. A probe Dial that arrives before Shutdown has closed the listener
// SUCCEEDS, and the connection it creates is counted live by the very tracker the
// ordering clause reads — so the probe would both perturb the oracle and make the
// connection totals depend on a race, destroying the arm's bit-reproducibility. The
// client's-eye version of the same observation is taken later, once the teardown is
// over and a Dial can create nothing ([boltDrainRunner.probeDialAfterTeardown]).
func (r *boltDrainRunner) observeParkedOrdering() {
	deadline := time.Now().Add(boltDrainJoinTimeout)
	for !r.srv.ln.Closed() && time.Now().Before(deadline) {
		time.Sleep(boltDrainDialPollStep)
	}
	r.ev.ListenerClosedWhileParked = r.srv.ln.Closed()
	time.Sleep(boltDrainBlockWindow)
	r.ev.CloseBodiesWhileParked = r.probe.Runs()
}

// probeDialAfterTeardown records that the server no longer accepts work. It runs
// after the teardown, where a Dial can only be refused, so it adds a
// production-shaped observation without adding a connection.
func (r *boltDrainRunner) probeDialAfterTeardown() {
	c, err := r.srv.Dial()
	if err != nil {
		r.ev.DialRefusedAfterTeardown = true
		return
	}
	_ = c.Close()
}

// boltDrainIsDrainTimeout reports whether err is Shutdown's drain-timeout error.
// It is matched by TEXT because Shutdown builds it with errors.New at the call
// site (bolt/server/serve.go:876) and exports no sentinel for it — which is
// itself worth knowing, and is why the arm records the rendered error too.
func boltDrainIsDrainTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "shutdown: drain timeout exceeded")
}

// joinServe joins the serve goroutine and records what [server.Server.Serve]
// returned. On the two expiry arms Serve is still blocked on the same drain when
// Shutdown returns, and its exit path is what performs the post-drain close, so
// this is also the point at which the store is finally torn down.
func (r *boltDrainRunner) joinServe() {
	err := r.srv.Close()
	r.ev.ServeExitErr = errText(err)
	r.ev.ServeExitErrIsCloseFault = errors.Is(err, errBoltDrainCloseFault)
}

// probeLoopAlive records that the checkpoint goroutine was running BEFORE the
// teardown, so "the loop was joined" is not a statement about a loop that never
// started.
func (r *boltDrainRunner) probeLoopAlive() {
	if r.cp == nil {
		return // no checkpointer wired: nothing to probe and nothing to join
	}
	r.ev.LoopAliveBeforeTeardown = r.cp.Trigger() == nil
}

// finish records everything observable after the teardown: the checkpoint loop's
// join, the store-level writer-closed witness, and the durability verdict read
// from a reopen through real recovery.
func (r *boltDrainRunner) finish(ctx context.Context) error {
	r.ev.CloseBodies = r.probe.Bodies()
	r.ev.ConnsAccepted = r.conns.accepted.Load()
	r.ev.ConnsClosed = r.conns.closed.Load()
	r.ev.ConnsPeak = r.conns.peak.Load()
	r.ev.ConnsLiveAtEnd = r.conns.live.Load()

	r.probeDialAfterTeardown()
	if r.cp != nil {
		r.ev.LoopStoppedAfterTeardown = errors.Is(r.cp.Trigger(), checkpoint.ErrCheckpointerStopped)
	}

	postErr := r.postCloseCommit(ctx)
	r.ev.PostCloseCommitErr = errText(postErr)
	r.ev.PostCloseCommitRefused = errors.Is(postErr, wal.ErrWriterClosed)

	for i := range r.ev.Commits {
		c := &r.ev.Commits[i]
		switch c.RunCode {
		case boltDrainInterruptedCode:
			r.ev.InterruptedRuns++
		case boltDrainTerminatedCode:
			r.ev.TerminatedRuns++
		}
		if c.RunIgnored {
			r.ev.IgnoredReplies++
		}
		if c.PullIgnored {
			r.ev.IgnoredReplies++
		}
	}
	sort.Strings(r.ev.AckedNames)
	sort.Strings(r.ev.IssuedNames)
	return r.readRecovered(ctx)
}

// postCloseCommit attempts one commit through the transaction layer after the
// teardown. It goes to the STORE, not over the wire, because the wire cannot
// carry the answer: [wal.ErrWriterClosed] is sanitised into a generic
// internal-error message before a client sees it (bolt/server/session.go:1945).
// Here the error's identity is checkable, which is what makes the writer-closed
// detector demonstrably able to see one.
func (r *boltDrainRunner) postCloseCommit(ctx context.Context) error {
	tx, err := r.st.store.BeginCtx(ctx)
	if err != nil {
		return err
	}
	if aerr := tx.AddNode("post-teardown-probe"); aerr != nil {
		_ = tx.Rollback()
		return aerr
	}
	return tx.Commit()
}

// readRecovered reopens the SimDisk image through real recovery (snapshot + WAL
// suffix, or WAL-only when no snapshot was published) and records what came back.
// The reference for the durability clause is the harness's OWN acked set, never
// the engine's view of itself.
func (r *boltDrainRunner) readRecovered(ctx context.Context) error {
	walPath := walPathFor(r.scfg.dir)
	if r.disk.Exists(walPath) {
		image, rerr := r.disk.ReadFile(walPath)
		if rerr != nil {
			return fmt.Errorf("sim: bolt-drain[%s] read WAL image: %w", r.cfg.Arm, rerr)
		}
		r.ev.WALBytes = len(image)
	}
	r.ev.SnapshotPublished = hasDurableSnapshot(r.disk, r.scfg.dir)

	st2, err := OpenSimStore(r.disk, r.scfg)
	if err != nil {
		return fmt.Errorf("sim: bolt-drain[%s] reopen: %w", r.cfg.Arm, err)
	}
	defer func() { _ = st2.Close() }()
	r.ev.RecoveredWALOps = st2.WALOps()
	r.ev.ReopenClean = st2.Clean()

	names, partial, err := boltDrainRecoveredNames(ctx, st2)
	if err != nil {
		return fmt.Errorf("sim: bolt-drain[%s] read recovered graph: %w", r.cfg.Arm, err)
	}
	r.ev.RecoveredNames = names
	r.ev.PartialNames = partial
	r.ev.MissingAcked = setMinus(toSet(r.ev.AckedNames), toSet(names))
	r.ev.PhantomNames = setMinus(toSet(names), toSet(r.ev.IssuedNames))
	sort.Strings(r.ev.MissingAcked)
	sort.Strings(r.ev.PhantomNames)
	sort.Strings(r.ev.PartialNames)
	return nil
}

// boltDrainRecoveredNames reads every recovered node's name and reports any whose
// age property is absent (a torn CREATE resurrected by recovery).
func boltDrainRecoveredNames(ctx context.Context, st *SimStore) (names, partial []string, err error) {
	res, err := st.Engine().Run(ctx, fmt.Sprintf("MATCH (n:%s) RETURN n.name, n.age", boltDrainLabel), nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
		s, ok := res.ValueAt(0).(expr.StringValue)
		if !ok {
			continue // a node without a name is off-model
		}
		name := string(s)
		names = append(names, name)
		if _, ok := res.ValueAt(1).(expr.IntegerValue); !ok {
			partial = append(partial, name)
		}
	}
	sort.Strings(names)
	return names, partial, res.Err()
}

// -----------------------------------------------------------------------------
// The concurrent arm
// -----------------------------------------------------------------------------

// driveFleet runs several committers concurrently and fires Shutdown while they
// are in flight, which is how a production driver fleet meets a graceful
// shutdown. It is NOT bit-reproducible: which commit is mid-fsync when the drain
// starts depends on the scheduler, so the acked set varies run to run. It is
// guarded on the invariants that hold under ANY interleaving — every
// acknowledged commit recovered, no phantom, no torn write, no
// DatabaseError-class reply to a statement issued before the teardown, exactly
// one teardown body, and a drained connection set at the close instant.
func (r *boltDrainRunner) driveFleet(ctx context.Context) error {
	if err := r.commitPrefix(ctx); err != nil {
		return err
	}
	r.probeLoopAlive()
	if err := r.commitWALSuffix(ctx); err != nil {
		return err
	}

	// Every connection is dialed BEFORE the teardown starts. That is not
	// incidental: the connection tracker increments just before the accept loop's
	// s.wg.Add, so a connection dialed into a teardown's drain window would be
	// live-but-unwaited-for and could read as an ordering breach that is really a
	// harness artefact.
	clients := make([]*WireClient, 0, boltDrainFleetConns)
	for i := range boltDrainFleetConns {
		c, err := r.dialReady(ctx)
		if err != nil {
			return fmt.Errorf("sim: bolt-drain[%s] fleet dial %d: %w", r.cfg.Arm, i, err)
		}
		clients = append(clients, c)
	}
	// Names are drawn from the seed BEFORE the goroutines start, so the issued set
	// is reproducible even though the acked set is not.
	names := make([][]string, len(clients))
	for i := range clients {
		names[i] = make([]string, boltDrainFleetOps)
		for j := range boltDrainFleetOps {
			names[i][j] = r.name(fmt.Sprintf("f%d", i), j)
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	rows := make([]BoltDrainCommit, 0, len(clients)*boltDrainFleetOps)
	for i := range clients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for _, name := range names[i] {
				row := r.fleetWrite(clients[i], name)
				mu.Lock()
				rows = append(rows, row)
				mu.Unlock()
				if boltDrainCommitterDone(&row) {
					return
				}
			}
		}(i)
	}

	// Wait for durable progress so the teardown races genuinely in-flight commits
	// rather than starting on an almost-empty store. Condition-bounded, never a
	// sleep.
	waitForSyncProgress(r.disk, r.disk.SyncCount()+2, time.Now().Add(boltDrainJoinTimeout))

	// Shutdown runs asynchronously and is awaited only after the committers have
	// finished: it cannot return until they have, because they are exactly what it
	// is draining.
	call := r.startShutdown()
	if !waitWGTimeout(&wg, boltDrainJoinTimeout) {
		return fmt.Errorf("sim: bolt-drain[%s] fleet committers did not finish within %s", r.cfg.Arm, boltDrainJoinTimeout)
	}
	r.awaitShutdown(call)
	for _, c := range clients {
		_ = c.Close()
	}
	r.joinServe()
	r.extraShutdowns()

	// Sorted so the evidence's commit block is stable for a given SET of outcomes,
	// which is all a non-reproducible arm can offer.
	sort.Slice(rows, func(a, b int) bool { return rows[a].Name < rows[b].Name })
	r.ev.Commits = append(r.ev.Commits, rows...)
	return r.finish(ctx)
}

// fleetWrite issues one fleet commit. It is [boltDrainRunner.writeOverWire] with
// the shared-slice appends taken under a lock, because several committers run at
// once.
func (r *boltDrainRunner) fleetWrite(c *WireClient, name string) BoltDrainCommit {
	row := BoltDrainCommit{Name: name, Phase: boltDrainPhasePre}
	r.fleetMu.Lock()
	r.ev.IssuedNames = append(r.ev.IssuedNames, name)
	r.fleetMu.Unlock()

	resp, err := c.Run(boltDrainCreate(name), nil)
	if err != nil {
		row.Transport = true
		return row
	}
	boltDrainClassifyRun(&row, resp)
	if !row.RunAcked {
		return row
	}
	_, term, err := c.PullAll()
	if err != nil {
		row.Transport = true
		return row
	}
	boltDrainClassifyPull(&row, term)
	if row.PullAcked {
		r.fleetMu.Lock()
		r.ev.AckedNames = append(r.ev.AckedNames, name)
		r.fleetMu.Unlock()
	}
	return row
}

// boltDrainCommitterDone reports whether a fleet committer should stop after this
// row.
//
// It stops on ANY outcome that is not a clean round trip, which is what a real
// driver does: a Bolt session answered a FAILURE is in FAILED and answers every
// subsequent request-phase message with IGNORED until it is RESET, so continuing
// would only produce replies that say nothing about the shutdown.
func boltDrainCommitterDone(row *BoltDrainCommit) bool {
	return row.Transport || row.RunIgnored || row.PullIgnored || row.RunCode != "" || row.PullCode != ""
}

// -----------------------------------------------------------------------------
// The adjudicators
// -----------------------------------------------------------------------------

// boltDrainOp labels a violation with the arm and the clause that produced it.
func boltDrainOp(arm, clause string) string {
	return "<bolt-shutdown-drain:" + arm + ":" + clause + ">"
}

// checkBoltShutdownDrain adjudicates the evidence against the CONTRACT: the
// ordering of the store's close against the connection drain, what the server
// publishes to repeated teardown callers, what a client is told, and what survives
// recovery.
//
// It is split from [checkBoltShutdownDrainNonVacuity] so an uninformative run
// cannot read as a faulty one (rmp #2470): every violation here names a property
// of the SERVER, never a property of the run's own coverage.
//
// The receiver is a pointer because the value is far over gocritic's hugeParam
// threshold; it mutates nothing.
func checkBoltShutdownDrain(e *BoltDrainEvidence) []Violation {
	// slices.Concat rather than repeated appends onto a nil slice: it sizes the
	// result once and returns nil — not an empty non-nil slice — when every clause
	// is satisfied, which is the shape a caller testing len(v) == 0 expects.
	return slices.Concat(
		checkBoltDrainOrdering(e),
		checkBoltDrainPublication(e),
		checkBoltDrainWire(e),
		checkBoltDrainInFlightCompleted(e),
		checkBoltDrainDurability(e),
		checkBoltDrainResidue(e),
	)
}

// checkBoltDrainOrdering adjudicates the one property this file exists for: the
// store's teardown runs exactly once, and not until the last connection has
// finished.
func checkBoltDrainOrdering(e *BoltDrainEvidence) []Violation {
	var v []Violation
	if len(e.CloseBodies) != 1 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "close-once"),
			Message: fmt.Sprintf("the store's teardown body ran %d time(s), want exactly 1: the server's closeOnce "+
				"must make the owned closer's Close run once however many stop paths reach it "+
				"(bolt/server/serve.go closeOwned)", len(e.CloseBodies)),
		})
	}
	for i := range e.CloseBodies {
		body := &e.CloseBodies[i]
		if body.LiveAt != 0 {
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: boltDrainOp(e.Arm, "drain-before-close"),
				Message: fmt.Sprintf("teardown body %d began with %d connection(s) still open "+
					"(accepted=%d closed=%d): the store must not be closed until the drain completes, or a "+
					"still-executing write can race the WAL close",
					i, body.LiveAt, body.AcceptedAt, body.ClosedAt),
			})
		}
		if body.Attribution == boltDrainClosedByUnknown {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "close-attribution"),
				Message: fmt.Sprintf("teardown body %d was entered from a call path this oracle does not model: "+
					"neither Server.Serve's exit, Server.Shutdown, nor the harness's own out-of-order close", i),
			})
		}
	}
	if e.ParkExpected && e.CloseBodiesWhileParked != 0 {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: boltDrainOp(e.Arm, "close-while-parked"),
			Message: fmt.Sprintf("%d teardown body/bodies had been entered while a commit was still parked inside "+
				"its WAL fsync and the listener was already closed: the drain did not wait for the in-flight write",
				e.CloseBodiesWhileParked),
		})
	}
	if e.ConnsLiveAtEnd != 0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "conn-residue"),
			Message: fmt.Sprintf("%d server-side connection(s) were still open after the teardown "+
				"(accepted=%d closed=%d): every connection handler must have exited", e.ConnsLiveAtEnd,
				e.ConnsAccepted, e.ConnsClosed),
		})
	}
	if !e.DialRefusedAfterTeardown {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "no-new-work"),
			Message: "a fresh connection was ACCEPTED after the teardown: the listener must be closed, or a client " +
				"can still reach a server whose store is gone",
		})
	}
	return append(v, checkBoltDrainExpiryAttribution(e)...)
}

// checkBoltDrainExpiryAttribution adjudicates WHO closed the store, on the two
// arms where the answer is determined rather than raced.
//
// On the success path it is a genuine race and therefore NOT adjudicated: Shutdown
// cancels the accept context before it drains, so Serve's deferred exit path and
// Shutdown's drain-success branch end up waiting on the same WaitGroup and either
// may reach closeOwned first (measured over 25 successful drains: 22 Serve, 3
// Shutdown). The once-guard makes the outcome identical, so asserting a winner
// would be asserting the scheduler.
//
// On an expiry arm it is determined: neither failure branch calls closeOwned
// (bolt/server/serve.go:874-879), so the store must still be open when Shutdown
// returns and must be closed afterwards, by Serve's exit path.
func checkBoltDrainExpiryAttribution(e *BoltDrainEvidence) []Violation {
	if !e.ExpiryExpected {
		return nil
	}
	var v []Violation
	if e.CloseBodiesAtShutdownReturn != 0 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "expiry-closed-early"),
			Message: fmt.Sprintf("%d teardown body/bodies had run by the time the expiring Shutdown returned: "+
				"an undrained connection may still hold a transaction, so neither Shutdown failure branch may "+
				"tear the store down (closeOwned is documented as reachable only from fully-drained stop paths)",
				e.CloseBodiesAtShutdownReturn),
		})
	}
	for i := range e.CloseBodies {
		body := &e.CloseBodies[i]
		if body.Attribution != boltDrainClosedByServeExit {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "expiry-close-attribution"),
				Message: fmt.Sprintf("teardown body %d was entered from %q, want %q: after an expired Shutdown the "+
					"only path left to the post-drain close is Serve's own exit cleanup",
					i, body.Attribution, boltDrainClosedByServeExit),
			})
		}
		if !body.AfterShutdownReturned {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "expiry-close-order"),
				Message: fmt.Sprintf("teardown body %d ran BEFORE the expiring Shutdown returned: on this path the "+
					"store is closed only once the abandoned connections finish, which is necessarily later", i),
			})
		}
	}
	return v
}

// checkBoltDrainPublication adjudicates what the server tells its teardown
// callers: which branch an expiring Shutdown reports, and that repeated callers
// observe ONE value rather than N re-derived ones.
func checkBoltDrainPublication(e *BoltDrainEvidence) []Violation {
	var v []Violation
	switch e.Arm {
	case ArmBoltDrainExpiryDrainTimeout:
		// WHICH of the two expiry errors a DEADLINE-bounded Shutdown reports is a
		// RACE, and this clause used to assert one of them. It must not.
		//
		// Shutdown clamps its drain timeout to time.Until(deadline) and then selects
		// over both that clamped time.After and ctx.Done() (bolt/server/serve.go).
		// The two therefore come due at very nearly the same instant, and when both
		// are ready Go's select chooses UNIFORMLY at random — so the winner is a coin
		// flip weighted only by which timer the runtime happens to fire first.
		//
		// An earlier version of this arm asserted the drain-timeout error on the
		// strength of 12 consecutive observations of it. That was luck, not a
		// property: under -race the bias moves, and a DEADLINE-bounded Shutdown was
		// measured returning "context deadline exceeded" — which turned this clause
		// into an intermittently red gate (5 of 6 -race runs failed across the arm's
		// tests). The precedent for the correct treatment is checkpoint_cadence.go's
		// TriggerCtx fold, which REPORTS Go's uniform choice rather than pinning it.
		//
		// So both errors are legal here and the arm adjudicates only what the
		// contract actually promises: the call reported FAILURE rather than a
		// spurious success (checked by the caller via ShutdownFirstNil), it did not
		// close the owned store, and nothing acknowledged was lost. Which branch
		// won is a witness, carried in the evidence and rendered as a class.
		if !e.ShutdownErrIsDrainTimeout && !e.ShutdownErrIsCtx {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "expiry-branch"),
				Message: fmt.Sprintf("an expiring Shutdown with a DEADLINE reported %q, want either the drain-timeout "+
					"error or a context error: those are the only two branches its select can take once the drain "+
					"has not finished", e.firstShutdownErr()),
			})
		}
	case ArmBoltDrainExpiryCtxCancel:
		if !e.ShutdownErrIsCtx {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "expiry-branch"),
				Message: fmt.Sprintf("an expiring Shutdown cancelled on a DEADLINE-FREE context reported %q, want the "+
					"context error: an explicit cancel is the only shape that reaches Shutdown's <-ctx.Done() branch",
					e.firstShutdownErr()),
			})
		}
	}
	if e.ExpiryExpected && e.ShutdownCalls > 1 && !e.LastShutdownNil {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "expiry-eventual-success"),
			Message: fmt.Sprintf("the last Shutdown call reported %q after the drain had completed, want success: "+
				"an expiry abandons the connections, it does not poison the server's teardown",
				e.ShutdownErrs[len(e.ShutdownErrs)-1]),
		})
	}
	if !e.ExpiryExpected && !e.CloseFaultArmed && !e.OutOfOrderArm && !e.ShutdownFirstNil {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "drain-success"),
			Message: fmt.Sprintf("Shutdown reported %q on an unbounded call over a healthy store, want success",
				e.firstShutdownErr()),
		})
	}
	if e.CloseFaultArmed {
		if !e.ShutdownErrIsCloseFault {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "close-failure-surfaced"),
				Message: fmt.Sprintf("Shutdown reported %q with the store's own close deliberately failing, want the "+
					"close failure: a failed WAL close must be surfaced rather than swallowed", e.firstShutdownErr()),
			})
		}
		if !e.ServeExitErrIsCloseFault {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "close-failure-joined"),
				Message: fmt.Sprintf("Serve returned %q with the store's own close deliberately failing, want the close "+
					"failure joined into it: Serve's exit path joins closeOwned's error into its own return",
					e.ServeExitErr),
			})
		}
	}
	if !e.ExpiryExpected && e.ShutdownCalls > 1 && e.DistinctShutdownErrs != 1 {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "published-identity"),
			Message: fmt.Sprintf("%d Shutdown call(s) observed %d DISTINCT error values (%q), want exactly 1: the "+
				"server caches one close result under its sync.Once, so every caller must observe the SAME value — "+
				"compared by identity here, because a class check cannot tell one published value from N re-derived ones",
				e.ShutdownCalls, e.DistinctShutdownErrs, e.ShutdownErrs),
		})
	}
	return v
}

// firstShutdownErr renders the first Shutdown call's error, or "<nil>".
func (e *BoltDrainEvidence) firstShutdownErr() string {
	if len(e.ShutdownErrs) == 0 || e.ShutdownErrs[0] == "" {
		return "<nil>"
	}
	return e.ShutdownErrs[0]
}

// checkBoltDrainWire adjudicates what a client was told.
//
// Every statement in these arms is issued on a connection the server had NOT
// drained, so the drain contract says each must complete before the store is torn
// down — and therefore that NONE of them may be answered with a storage failure.
// The wire signature of a storage failure is the DatabaseError catch-all: the
// message a closed-writer commit produces is sanitised into a generic
// internal-error text (bolt/server/session.go:1945), so the CODE is the only
// observable, and it is checked on both the RUN and the PULL reply.
func checkBoltDrainWire(e *BoltDrainEvidence) []Violation {
	var v []Violation
	for i := range e.Commits {
		c := &e.Commits[i]
		switch {
		case c.RunCode == boltDrainDatabaseErrorCode:
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: boltDrainOp(e.Arm, "wire-storage-failure"),
				Message: fmt.Sprintf("the %s write %q was answered with %s at RUN: a client on an undrained "+
					"connection was told its write failed, which is the wire signature of a teardown that raced it",
					c.Phase, c.Name, c.RunCode),
			})
		case e.FleetArm && (c.RunCode == boltDrainInterruptedCode || c.RunCode == boltDrainTerminatedCode):
			// The shutdown cut the statement short. Accepted on the CONCURRENT arm only:
			// there, a committer may genuinely be mid-statement when the accept context
			// is cancelled. On a deterministic arm every write is either completed
			// before the teardown or is the parked one, which
			// [checkBoltDrainInFlightAck] requires to be ACKNOWLEDGED — so neither code
			// can serve as an escape hatch. See [boltDrainTerminatedCode].
		case c.RunCode != "":
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "wire-unexpected-code"),
				Message: fmt.Sprintf("the %s write %q was refused at RUN with %s, which this arm provokes no cause for",
					c.Phase, c.Name, c.RunCode),
			})
		}
		if !e.FleetArm && (c.RunIgnored || c.PullIgnored) {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "wire-ignored"),
				Message: fmt.Sprintf("the %s write %q was IGNORED (run=%t pull=%t): a deterministic arm drives no "+
					"statement that should put a session into FAILED, so an ignored reply means the arm measured "+
					"traffic it did not intend", c.Phase, c.Name, c.RunIgnored, c.PullIgnored),
			})
		}
		if c.PullCode == boltDrainDatabaseErrorCode {
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: boltDrainOp(e.Arm, "wire-storage-failure"),
				Message: fmt.Sprintf("the %s write %q was answered with %s at PULL: the commit reached its durability "+
					"point and failed there", c.Phase, c.Name, c.PullCode),
			})
		}
	}
	return v
}

// checkBoltDrainInFlightCompleted adjudicates the positive half of the drain
// contract on a parked arm: the commit the teardown found in flight must be
// DISPATCHED and its write must SURVIVE recovery.
//
// It cannot be phrased as "acknowledged", and that is the point. A graceful
// Shutdown flushes the in-flight statement's reply and then closes the connection
// (bolt/server/serve.go:1194 returns on the first pass through the select after
// connCtx is cancelled), so the parked committer gets its RUN SUCCESS and its
// TERMINAL never arrives — meaning the client never receives the bookmark and the
// commit is, correctly, not in the acknowledged set. What the drain owes it is that
// the write it was in the middle of nonetheless COMPLETED and is durable, which is
// read from a reopen through real recovery.
//
// This is what stops [boltDrainInterruptedCode] and [boltDrainTerminatedCode]
// becoming escape hatches. A drain that abandoned in-flight writes would satisfy
// every clause phrased as an absence — nothing acknowledged would be lost, because
// nothing would be acknowledged — and would fail here.
func checkBoltDrainInFlightCompleted(e *BoltDrainEvidence) []Violation {
	if !e.ParkExpected {
		return nil
	}
	var v []Violation
	for i := range e.Commits {
		c := &e.Commits[i]
		if c.Phase != boltDrainPhaseInFlight {
			continue
		}
		if !c.RunAcked {
			v = append(v, Violation{
				Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "inflight-dispatched"),
				Message: fmt.Sprintf("the in-flight write %q was never dispatched (run-code %q, ignored %t, "+
					"transport-error %t): a graceful drain must let the statement it found executing run",
					c.Name, c.RunCode, c.RunIgnored, c.Transport),
			})
			continue
		}
		if !slices.Contains(e.RecoveredNames, c.Name) {
			v = append(v, Violation{
				Kind: ViolationACIDDurability, Op: boltDrainOp(e.Arm, "inflight-durable"),
				Message: fmt.Sprintf("the in-flight write %q was dispatched but is absent after a reopen through "+
					"real recovery: the drain waited for the statement and then discarded its effect", c.Name),
			})
		}
	}
	return v
}

// checkBoltDrainDurability adjudicates what survived, against the harness's OWN
// record of what the clients were told — never against the engine's view of
// itself.
func checkBoltDrainDurability(e *BoltDrainEvidence) []Violation {
	var v []Violation
	for _, missing := range e.MissingAcked {
		v = append(v, Violation{
			Kind: ViolationACIDDurability, Op: boltDrainOp(e.Arm, "acked-lost"),
			Message: fmt.Sprintf("acknowledged commit %q is absent after the teardown and a reopen through real "+
				"recovery (acked=%d recovered=%d, %d WAL ops replayed)",
				missing, len(e.AckedNames), len(e.RecoveredNames), e.RecoveredWALOps),
		})
	}
	for _, phantom := range e.PhantomNames {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: boltDrainOp(e.Arm, "phantom"),
			Message: fmt.Sprintf("recovered node %q was never issued by any client (phantom write)", phantom),
		})
	}
	for _, torn := range e.PartialNames {
		v = append(v, Violation{
			Kind: ViolationACIDAtomicity, Op: boltDrainOp(e.Arm, "torn-create"),
			Message: fmt.Sprintf("recovered node %q lacks its age property: a torn transaction was resurrected", torn),
		})
	}
	if !e.ReopenClean {
		v = append(v, Violation{
			Kind: ViolationACIDConsistency, Op: boltDrainOp(e.Arm, "reopen-unclean"),
			Message: fmt.Sprintf("recovery reported a NON-CLEAN durable image after the teardown "+
				"(%d WAL ops replayed, snapshot=%t)", e.RecoveredWALOps, e.SnapshotPublished),
		})
	}
	return v
}

// checkBoltDrainResidue adjudicates what the teardown left behind: the checkpoint
// goroutine joined, and the WAL genuinely closed.
func checkBoltDrainResidue(e *BoltDrainEvidence) []Violation {
	var v []Violation
	if !e.LoopStoppedAfterTeardown {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "checkpointer-join"),
			Message: "a checkpoint request after the teardown did not report checkpoint.ErrCheckpointerStopped: " +
				"the checkpoint goroutine outlived the store it writes through, which is the leak the composed " +
				"teardown exists to prevent",
		})
	}
	if !e.PostCloseCommitRefused {
		v = append(v, Violation{
			Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, "wal-closed"),
			Message: fmt.Sprintf("a commit attempted through the transaction layer after the teardown returned %q, "+
				"want wal.ErrWriterClosed: without it the teardown did not actually close the WAL, and the "+
				"writer-closed detector every other clause relies on is never shown to see one",
				e.PostCloseCommitErr),
		})
	}
	return v
}

// checkBoltShutdownDrainNonVacuity is the SEPARATE coverage precondition, kept
// apart from [checkBoltShutdownDrain] for the reason rmp #2470 established: an
// uninformative run must not read as a faulty one. It asserts the run had the
// SHAPE this adjudication needs — a closer the server actually owned, connections
// it actually had to drain, commits that actually reached the WAL suffix, and (on
// a parked arm) a rendezvous that was actually reached while Shutdown was
// demonstrably already draining.
//
// A violation here means the RUN proved nothing, not that the server is broken.
func checkBoltShutdownDrainNonVacuity(e *BoltDrainEvidence) []Violation {
	var v []Violation
	shortfall := func(clause, msg string) {
		v = append(v, Violation{Kind: ViolationOracleDeviation, Op: boltDrainOp(e.Arm, clause), Message: msg})
	}
	if !e.CloserWired {
		shortfall("nonvacuity-closer", "the server was given no Options.Closer, so it owned no store and every "+
			"ordering clause is a statement about a teardown that never happened")
	}
	if !e.ConnDecorated {
		shortfall("nonvacuity-observable", "the connection tracker was not installed, so \"the drain was complete when "+
			"the close began\" is read off an instrument that counts nothing")
	}
	if e.ConnsPeak < 2 {
		shortfall("nonvacuity-conns", fmt.Sprintf("the server never held more than %d connection(s) at once: a drain of "+
			"at most one connection says little about draining a fleet", e.ConnsPeak))
	}
	if e.FleetArm && e.ConnsPeak < boltDrainFleetConns {
		shortfall("nonvacuity-fleet", fmt.Sprintf("the fleet arm held at most %d connection(s) at once, want %d: the "+
			"drain had fewer committers to wait for than the arm claims", e.ConnsPeak, boltDrainFleetConns))
	}
	if !e.FleetArm && e.IdleConnsOpened != boltDrainIdleConns {
		shortfall("nonvacuity-idle", fmt.Sprintf("the arm left %d idle connection(s) open, want %d: they are the live "+
			"handler goroutines the drain has to join", e.IdleConnsOpened, boltDrainIdleConns))
	}
	if len(e.AckedNames) == 0 {
		shortfall("nonvacuity-acked", "no commit was acknowledged at all, so the durability clause has nothing to be "+
			"answered by")
	}
	if e.AckedAfterCheckpoint == 0 {
		shortfall("nonvacuity-wal-suffix", "no commit was acknowledged AFTER the arm's checkpoint: every acked name "+
			"could then come back from the published snapshot, and a WAL close that flushed nothing would satisfy "+
			"the durability clause")
	}
	if e.RecoveredWALOps == 0 {
		shortfall("nonvacuity-wal-replay", fmt.Sprintf("recovery replayed %d WAL op(s): the durability clause was "+
			"answered by the snapshot, not by the WAL the teardown closed", e.RecoveredWALOps))
	}
	if !e.SnapshotPublished {
		shortfall("nonvacuity-snapshot", "no snapshot manifest was published, so the reopen did not exercise the "+
			"full snapshot+WAL recovery path this arm's store is laid out for")
	}
	if !e.LoopAliveBeforeTeardown {
		shortfall("nonvacuity-loop", "no checkpoint request succeeded before the teardown, so \"the checkpoint "+
			"goroutine was joined\" is satisfied by a loop that was never running")
	}
	if len(e.Commits) == 0 {
		shortfall("nonvacuity-commits", "the arm issued no write at all, so the wire clause has nothing to adjudicate")
	}
	v = append(v, checkBoltDrainParkShape(e, shortfall)...)
	if !e.OutOfOrderArm && !e.FleetArm && e.ShutdownCalls < 2 {
		shortfall("nonvacuity-shutdown-calls", fmt.Sprintf("the arm made %d Shutdown call(s): with fewer than two, "+
			"\"every caller observed the same published value\" is a statement about a single observation",
			e.ShutdownCalls))
	}
	if e.ExpiryExpected && e.ShutdownExpiryBudget <= 0 {
		shortfall("nonvacuity-expiry", "the arm claims an expiring Shutdown but gave it no bound, so it could not have "+
			"expired")
	}
	return v
}

// checkBoltDrainParkShape adjudicates the SHAPE of the in-flight rendezvous: it
// must have been armed, entered, and observed while Shutdown was demonstrably
// already draining, or the negative observation ("the close had not begun") is a
// statement about the harness rather than about the server.
func checkBoltDrainParkShape(e *BoltDrainEvidence, shortfall func(clause, msg string)) []Violation {
	if !e.ParkExpected {
		return nil
	}
	if !e.GateArmed {
		shortfall("nonvacuity-gate", "the arm claims a parked commit but armed no fsync rendezvous")
		return nil
	}
	if !e.GateFired {
		shortfall("nonvacuity-gate", "the armed fsync rendezvous was never ENTERED, so no commit was ever in flight "+
			"and \"the close waited for it\" is a statement about a gate that matched no ordinal")
	}
	if !e.ListenerClosedWhileParked {
		shortfall("nonvacuity-shutdown-progress", "the listener was not observed closed while the commit was parked, "+
			"so Shutdown was never shown to have entered its drain wait: \"the store's teardown had not begun\" is "+
			"then equally explained by a Shutdown that had not begun either")
	}
	if e.ParkedLiveConns < 2 {
		shortfall("nonvacuity-parked-conns", fmt.Sprintf("only %d connection(s) were live while the commit was parked: "+
			"the drain had at most the parked one to wait for", e.ParkedLiveConns))
	}
	inflight := 0
	for i := range e.Commits {
		if e.Commits[i].Phase == boltDrainPhaseInFlight {
			inflight++
		}
	}
	if inflight != 1 {
		shortfall("nonvacuity-inflight-row", fmt.Sprintf("the arm recorded %d in-flight write(s), want exactly 1", inflight))
	}
	return nil
}

// -----------------------------------------------------------------------------
// The scenarios
// -----------------------------------------------------------------------------

// The catalogue defaults for the two shutdown scenarios.
const (
	boltShutdownDrainDefaultSeed = 0x2483_D5A1
	boltShutdownFleetDefaultSeed = 0x2483_F1EE
)

// boltShutdownDrainScenario drives the four deterministic arms in order: the
// successful drain with a commit parked inside its fsync, the two expiry branches,
// and the publication arm over a deliberately failing store close.
func boltShutdownDrainScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltShutdownDrain,
		Description: "Bolt Server.Shutdown drain and Options.Closer ordering: the owned store is closed exactly " +
			"once and never before the last connection has finished, both Shutdown failure branches leave the " +
			"store for Serve's exit path, and a failed close is published identically to every caller",
		Mode:        ModeDeterministic,
		DefaultSeed: boltShutdownDrainDefaultSeed,
		run:         runBoltShutdownDrainScenario,
	}
}

// boltShutdownFleetScenario drives the concurrent arm: several committers in
// flight when Shutdown fires, as a production driver fleet would be.
//
// It is registered separately from [boltShutdownDrainScenario] because it is NOT
// bit-reproducible — which commit is mid-fsync when the drain starts depends on
// the scheduler — and folding it into a deterministic scenario would make that
// scenario's report vary run to run while still claiming determinism.
func boltShutdownFleetScenario() Scenario {
	return Scenario{
		Name: ScenarioBoltShutdownFleet,
		Description: "Bolt Server.Shutdown against a fleet of concurrent committers: every acknowledged commit " +
			"survives the drain and real recovery, no client on an undrained connection is told its write failed, " +
			"and the owned store is closed exactly once, after the last connection",
		Mode:        ModeConcurrent,
		DefaultSeed: boltShutdownFleetDefaultSeed,
		Connections: boltDrainFleetConns,
		OpsPerConn:  boltDrainFleetOps,
		Mix:         &ConcurrentMix{WriterWeight: 1.0},
		run:         runBoltShutdownFleetScenario,
	}
}

// runBoltShutdownDrainScenario drives every deterministic arm and concatenates
// their violations.
//
// All four run, because they measure different halves of one surface and a caller
// selecting the scenario by name expects the whole of it. An arm that cannot be
// DRIVEN is a harness error and aborts the scenario; a violation is a report.
func runBoltShutdownDrainScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	var v []Violation
	for _, arm := range boltDrainArms {
		cfg, ok := boltDrainArmConfig(arm, seed)
		if !ok {
			return nil, fmt.Errorf("sim: bolt-shutdown-drain: no configuration for arm %q", arm)
		}
		ev, err := RunBoltShutdownDrain(ctx, cfg)
		if err != nil {
			return nil, err
		}
		v = append(v, checkBoltShutdownDrain(ev)...)
		v = append(v, checkBoltShutdownDrainNonVacuity(ev)...)
	}
	if len(v) == 0 {
		return nil, nil
	}
	return boltDrainReport(ScenarioBoltShutdownDrain, ModeDeterministic, seed, v), nil
}

// runBoltShutdownFleetScenario drives the concurrent arm.
func runBoltShutdownFleetScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	cfg, ok := boltDrainArmConfig(ArmBoltDrainFleet, seed)
	if !ok {
		return nil, fmt.Errorf("sim: bolt-shutdown-fleet: no configuration for arm %q", ArmBoltDrainFleet)
	}
	ev, err := RunBoltShutdownDrain(ctx, cfg)
	if err != nil {
		return nil, err
	}
	v := slices.Concat(checkBoltShutdownDrain(ev), checkBoltShutdownDrainNonVacuity(ev))
	if len(v) == 0 {
		return nil, nil
	}
	return boltDrainReport(ScenarioBoltShutdownFleet, ModeConcurrent, seed, v), nil
}

// boltDrainReport wraps violations in a scenario report.
func boltDrainReport(name string, mode ExecMode, seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Scenario:   name,
		Mode:       mode,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<bolt server shutdown drain>"},
		Violations: v,
	}
}
