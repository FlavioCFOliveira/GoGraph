package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// serverAgent is the agent string advertised in SUCCESS metadata after HELLO.
const serverAgent = "GoGraph/1.0"

// Session holds all per-connection state for a single Bolt v5 client
// connection.
//
// Session is NOT safe for concurrent use. Each accepted TCP connection owns
// exactly one Session, and the message loop is single-threaded per connection.
type Session struct {
	// id is a random hex string used in log messages to identify the connection.
	id string

	// identity is populated after a successful HELLO/LOGON.
	identity Identity

	// authenticated reports whether this connection has completed a successful
	// HELLO or LOGON and has not since been de-authorized by LOGOFF. It is the
	// authoritative authentication gate, set true only on a successful
	// Authenticate and cleared on LOGOFF; query-bearing handlers (RUN, BEGIN,
	// ROUTE) and RESET consult it so that no credential-less connection can
	// reach a query-executing state. It is tracked as an explicit flag rather
	// than inferred from identity, because a NoAuthHandler legitimately yields a
	// zero-value Identity for a client that sent no principal — which must still
	// count as authenticated. (task #1345)
	authenticated bool

	// eng is the Cypher engine that executes queries.
	eng *cypher.Engine

	// auth is the auth handler used during HELLO/LOGON.
	auth AuthHandler

	// state is the current Bolt protocol state machine state.
	state State

	// result is the open streaming cursor, non-nil only in STREAMING or
	// TX_STREAMING states.
	result *cypher.Result

	// columns holds the ordered column names of the current result, matching
	// result.Columns() at the time RUN was processed.
	columns []string

	// peeked, when non-nil, holds a pre-fetched row from the cursor that has
	// been read ahead to determine has_more. It must be emitted before the next
	// result.Next() call.
	peeked *[]packstream.Value

	// records, when non-nil, is the per-connection sink that [Session.handlePull]
	// streams RECORD messages to as it iterates the result cursor, instead of
	// accumulating them in the returned response slice. The serve loop installs
	// a sink that encodes and writes each record to the client connection
	// immediately under the per-message write deadline, so a large PULL is
	// never duplicated into a second in-memory copy and a slow reader exerts
	// TCP backpressure on the producing loop (task #1350). When nil (a Session
	// driven directly through [Session.HandleMessage], without a serve loop),
	// handlePull falls back to buffering the records in the returned slice.
	// The sink is invoked only from the session's single message-handling
	// goroutine, so it needs no synchronisation.
	records recordSink

	// txActive indicates that an explicit transaction is open (BEGIN called).
	txActive bool

	// txDeadline is the absolute wall-clock deadline of the open explicit
	// transaction, set by [Session.handleBegin] from the effective transaction
	// timeout (zero when no finite timeout applies, or when no transaction is
	// open). The serve loop arms a reaper that closes the connection at this
	// instant so an idle-but-open transaction cannot hold the engine's global
	// writer lock indefinitely while the client keeps the connection alive with
	// no-op messages (task #1346). It is read only by the serve loop after
	// [Session.HandleMessage] returns, on the same goroutine, so it needs no
	// synchronisation.
	txDeadline time.Time

	// tx is the active explicit transaction, non-nil when txActive is true.
	tx *Tx

	// txAccounted records whether the currently-open explicit transaction has
	// been counted by [metricTxOpened] but not yet counted closed by
	// [metricTxClosed]. It guards the open-transaction gauge derivation
	// (opened − closed) so that exactly one tx.closed is emitted per tx.opened,
	// regardless of which of the several teardown paths (COMMIT, ROLLBACK,
	// RESET, GOODBYE, FAILED-reclaim, connection teardown) ends the transaction.
	// It is set by [Session.txOpened] and cleared by the first [Session.txClosed]
	// for that transaction; a second close on the same transaction (e.g. the
	// idempotent teardown rollback after a prior COMMIT) finds it already false
	// and is a no-op. The Session is single-threaded per connection, so the flag
	// needs no synchronisation.
	txAccounted bool

	// stmtTimeout is extracted from RUN extra metadata ("timeout" key, ms).
	stmtTimeout time.Duration

	// maxStmtTimeout is the server-side cap applied to client-supplied timeouts.
	// Zero means no cap.
	maxStmtTimeout time.Duration

	// bookmark holds the last committed transaction bookmark (server-generated
	// placeholder for this sprint).
	bookmark string

	// localAddr is the listener address of the server that accepted this
	// connection; used to populate the routing table in ROUTE responses.
	localAddr string

	// log is the session-scoped structured logger.
	log *slog.Logger

	// maxInFlight bounds the total number of Result cursors that may be
	// registered against an explicit transaction before it must be
	// committed or rolled back. Defaults to
	// [DefaultMaxInFlightPerConnection]. RUN is rejected with
	// "Neo.ClientError.General.LimitExceeded" once the count reaches
	// this value. See [Options.MaxInFlightPerConnection] for the
	// rationale.
	maxInFlight int

	// defaultTxTimeout is the bounded transaction timeout applied to an explicit
	// transaction (BEGIN) when the client supplies no tx_timeout. A finite value
	// guarantees the engine writer serialisation an explicit transaction holds
	// can never be retained indefinitely (a client that BEGINs and then stalls
	// would otherwise block every other writer forever). Seeded from
	// [Options.DefaultTxTimeout] / [DefaultTxTimeout]. Zero means no default
	// bound (a client BEGIN with no tx_timeout is then unbounded — not used by
	// the production server, which always installs a finite default).
	defaultTxTimeout time.Duration

	// boltVersion is the Bolt protocol version negotiated during the handshake.
	// It is set once by setBoltVersion immediately after proto.Negotiate and is
	// read-only thereafter. Used by exprToPackstream to select the correct
	// PackStream encoding for temporal types (e.g. DateTime tag 0x46 for v4.4,
	// 0x49 for v5.0+).
	boltVersion proto.Version

	// clk is the wall-clock source for computing the explicit-transaction
	// deadline (txDeadline). It defaults to [clock.Real] in [newSession] and is
	// set by the server to the same clock its serve-loop reaper uses, so the
	// deadline and the reaper's countdown are coherent. A test may inject a
	// [clock.Fake] so a session timeout is driven by virtual time.
	clk clock.Clock
}

// newSession constructs an idle Session backed by eng, starting in
// StateNegotiation (version negotiation has already succeeded by the time
// newSession is called). localAddr is the listener address reported in ROUTE
// responses; it may be empty for sessions created without a listening address
// (e.g. unit tests).
func newSession(eng *cypher.Engine, auth AuthHandler, localAddr string) *Session {
	return &Session{
		id:               randomID(),
		eng:              eng,
		auth:             auth,
		state:            StateNegotiation,
		localAddr:        localAddr,
		log:              slog.Default(),
		maxInFlight:      DefaultMaxInFlightPerConnection,
		defaultTxTimeout: DefaultTxTimeout,
		clk:              clock.Real(),
	}
}

// setClock overrides the session's wall-clock source for the
// explicit-transaction deadline computation. A nil clock is ignored, leaving
// the default real clock in place. Intended for the server bootstrap (so the
// session and the serve-loop reaper share one clock) and for tests.
func (s *Session) setClock(clk clock.Clock) {
	if clk != nil {
		s.clk = clk
	}
}

// setMaxStmtTimeout sets the server-side statement timeout cap. Non-positive
// values are ignored.
func (s *Session) setMaxStmtTimeout(d time.Duration) {
	if d > 0 {
		s.maxStmtTimeout = d
	}
}

// setMaxInFlight overrides the session's per-connection in-flight
// cursor cap. Intended for use by the server bootstrap path so the
// operator-configured [Options.MaxInFlightPerConnection] takes
// effect; tests may use it to exercise alternative caps. A
// non-positive value is rejected and the cap is left unchanged.
func (s *Session) setMaxInFlight(n int) {
	if n > 0 {
		s.maxInFlight = n
	}
}

// setDefaultTxTimeout sets the bounded transaction timeout applied to an
// explicit transaction when the client supplies no tx_timeout. Non-positive
// values are ignored, leaving the default in place. Intended for the server
// bootstrap path so the operator-configured [Options.DefaultTxTimeout] takes
// effect.
func (s *Session) setDefaultTxTimeout(d time.Duration) {
	if d > 0 {
		s.defaultTxTimeout = d
	}
}

// maxClientTimeoutMillis is the largest client-supplied timeout, in
// milliseconds, that converts to a positive [time.Duration] without overflowing
// the underlying int64 nanosecond count. time.Duration is an int64 of
// nanoseconds, so ms * int64(time.Millisecond) overflows once ms exceeds
// math.MaxInt64 / int64(time.Millisecond) (≈9.22e12 ms ≈ 2562047h). A hostile
// client can otherwise pick a value that wraps the product to zero (e.g.
// 1<<62 ms) or negative (e.g. math.MaxInt64 ms), defeating the timeout guard.
const maxClientTimeoutMillis = int64(math.MaxInt64) / int64(time.Millisecond)

// clientMillisToDuration converts a client-supplied timeout in milliseconds to
// a [time.Duration], overflow-safely. It returns (d, true) only when ms is
// strictly positive AND small enough that the nanosecond product does not
// overflow int64. A non-positive or overflowing ms returns (0, false), so the
// caller treats it as "unset" and falls back to the server default — never to a
// wrapped, non-positive duration (CWE-190). See #1484.
func clientMillisToDuration(ms int64) (time.Duration, bool) {
	if ms <= 0 || ms > maxClientTimeoutMillis {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// setBoltVersion records the Bolt protocol version agreed during handshake.
// Must be called once, immediately after proto.Negotiate, before any messages
// are processed. The version governs temporal-value encoding in RECORD messages
// (task #1434).
func (s *Session) setBoltVersion(v proto.Version) {
	s.boltVersion = v
}

// recordSink consumes one RECORD message produced by [Session.handlePull],
// encoding and writing it to the client connection immediately. A non-nil
// error reports a failed or timed-out write; the failure is terminal for the
// connection — a partially written chunked message cannot be resumed — so the
// caller stops streaming and surfaces [errRecordWrite].
//
// Contract: a sink MUST consume the *proto.Record (and its Data slice)
// synchronously and retain no reference to either past its return. handlePull
// reuses one Record and one row buffer across the rows of a PULL (#1520), so a
// sink that buffered or deferred the record would observe it overwritten by the
// next row. The built-in sink (encode-and-write-now) honours this.
type recordSink func(*proto.Record) error

// errRecordWrite is the sentinel error returned by [Session.HandleMessage]
// when the installed record sink failed mid-stream. After a partial chunked
// write the connection framing is unrecoverable, so the serve loop must tear
// the connection down without attempting to write any further message.
var errRecordWrite = errors.New("bolt: record stream write failed")

// setRecordSink installs the per-connection record sink that
// [Session.handlePull] streams RECORD messages to (task #1350). It must be
// set before the session processes its first message and never changed
// afterwards; the session invokes the sink only from its single
// message-handling goroutine.
func (s *Session) setRecordSink(sink recordSink) {
	s.records = sink
}

// inFlightCount returns the number of Result cursors registered against this
// session that count toward the per-connection cap.
//
// Inside an explicit transaction the count is the total number of Result
// cursors appended to tx.results since BEGIN, regardless of whether they have
// been fully pulled (closed). Each RUN appends one cursor; the slice is only
// cleared on COMMIT or ROLLBACK. This bounds the total heap footprint of a
// transaction: a client that issues RUN+PULL in a tight loop without
// committing would otherwise accumulate an unbounded number of (closed but
// still referenced) Result objects.
//
// Outside a transaction there is at most one auto-commit cursor (s.result).
// The Bolt state machine already prevents a second auto-commit RUN before the
// first cursor is consumed, so the auto-commit path is not bounded by this
// counter; instead it returns 0 once s.result has been cleared by drainResult.
func (s *Session) inFlightCount() int {
	if s.txActive && s.tx != nil {
		return len(s.tx.results)
	}
	if s.result != nil {
		return 1
	}
	return 0
}

// randomID returns a 16-byte random hex string suitable for use as a session
// identifier in log messages.
func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// HandleMessage dispatches msg to the correct per-state handler and returns
// the response messages to send to the client.
//
// On an illegal state transition or internal error the session moves to
// FAILED and HandleMessage returns a single *proto.Failure response. The
// caller is responsible for encoding and sending all returned messages.
//
// When a record sink is installed (see [Session.setRecordSink]), the RECORD
// messages of a PULL are written through the sink as the cursor is iterated
// and are NOT part of the returned slice, which then carries only the
// trailing SUCCESS or FAILURE. A sink write failure is surfaced as an error
// wrapping [errRecordWrite]: the connection framing is unrecoverable and the
// caller must tear the connection down without writing anything further.
func (s *Session) HandleMessage(ctx context.Context, msg any) ([]any, error) {
	// Propagate context cancellation before doing any work. failWith routes
	// through enterFailed, which reclaims any open explicit transaction even on
	// this early-return path that never reaches dispatch (#1312).
	if err := ctx.Err(); err != nil {
		return s.failWith("Neo.TransientError.General.RequestInterrupted", err.Error()), nil
	}

	// Every transition into FAILED reclaims any open explicit transaction at the
	// transition itself (see enterFailed): no further RUN/COMMIT can run in
	// FAILED, and RESET would discard the transaction anyway, so its writes — and
	// the engine writer serialisation it holds — must be released NOW rather than
	// lingering for the whole FAILED→RESET window (#1312). Reclaiming is funnelled
	// through enterFailed so it holds for every FAILED entry, including paths that
	// do not pass through here (the context-cancellation early return above) or
	// that set FAILED inline in a handler (the in-flight cap, a PULL cursor error,
	// a failed COMMIT). RESET/GOODBYE and the connection-teardown Close roll the
	// transaction back on their own idempotent paths.
	return s.dispatch(ctx, msg)
}

// dispatch routes msg to the correct per-state handler. It is the inner switch
// of [Session.HandleMessage].
func (s *Session) dispatch(ctx context.Context, msg any) ([]any, error) {
	// Bolt v5: once an AUTHENTICATED connection is FAILED, a query/transaction
	// message is answered with IGNORED — not executed, not re-failed — until the
	// client RESETs (#1781). Scoped deliberately:
	//   - Only the request-phase messages (RUN/PULL/DISCARD/BEGIN/COMMIT/
	//     ROLLBACK/ROUTE) are ignored. The auth messages HELLO/LOGON/LOGOFF keep
	//     their dedicated handlers, which enforce the pre-auth/re-auth security
	//     gates (a failed HELLO / illegal LOGON in FAILED must FAIL, not be
	//     IGNORED — see the auth-bypass regression tests). RESET/GOODBYE escape
	//     FAILED (handled below and in Transition).
	//   - Only when s.authenticated: an UNAUTHENTICATED FAILED connection (e.g.
	//     a failed HELLO) keeps its hard FAILURE/terminal behaviour so a query
	//     can never be softly ignored into an auth bypass.
	// It also guarantees no write/DDL runs while an authenticated session is FAILED.
	if s.state == StateFailed && s.authenticated {
		switch msg.(type) {
		case *proto.Run, *proto.Pull, *proto.Discard, *proto.Begin,
			*proto.Commit, *proto.Rollback, *proto.Route:
			return []any{&proto.Ignored{}}, nil
		}
	}
	switch m := msg.(type) {
	case *proto.Hello:
		return s.handleHello(m)
	case *proto.Logon:
		return s.handleLogon(m)
	case *proto.Logoff:
		return s.handleLogoff()
	case *proto.Reset:
		return s.handleReset()
	case *proto.Goodbye:
		return s.handleGoodbye()
	case *proto.Run:
		return s.handleRun(ctx, m)
	case *proto.Pull:
		return s.handlePull(ctx, m)
	case *proto.Discard:
		return s.handleDiscard(m)
	case *proto.Begin:
		return s.handleBegin(ctx, m)
	case *proto.Commit:
		return s.handleCommit()
	case *proto.Rollback:
		return s.handleRollback()
	case *proto.Route:
		return s.handleRoute(m)
	default:
		return s.failWith("Neo.ClientError.Request.Invalid",
			fmt.Sprintf("unrecognised message type %T", msg)), nil
	}
}

// txOpened records that an explicit transaction has just been opened. It
// increments the [metricTxOpened] counter (the opened side of the
// open-transaction gauge derivation opened − closed) and arms [Session.txClosed]
// via the txAccounted flag so the transaction is counted closed exactly once.
// It is called once from [Session.handleBegin] at the single point a BEGIN
// successfully acquires the transaction.
func (s *Session) txOpened() {
	s.txAccounted = true
	incCounter(metricTxOpened)
}

// txClosed records that the currently-open explicit transaction has ended,
// incrementing the [metricTxClosed] counter (the closed side of the
// open-transaction gauge derivation) exactly once per [Session.txOpened]. It is
// idempotent: it increments only when txAccounted is set, then clears the flag,
// so the several teardown paths that may run for one transaction (a handler
// closing it followed by the idempotent connection-teardown rollback) emit a
// single tx.closed. It is called from every path that ends an explicit
// transaction: COMMIT, ROLLBACK, RESET, GOODBYE, the FAILED-reclaim
// ([Session.abortTx]), and the connection teardown ([Session.Close]).
func (s *Session) txClosed() {
	if !s.txAccounted {
		return
	}
	s.txAccounted = false
	incCounter(metricTxClosed)
}

// abortTx drains any open result cursor and rolls back the session's explicit
// transaction, clearing all transaction state. It is the shared teardown for the
// FAILED-with-open-transaction path (post-dispatch) and the connection-teardown
// path ([Session.Close]). It is best-effort and idempotent: a nil tx is a no-op,
// and rolling back an already-finished engine transaction returns promptly. After
// abortTx the session holds no transaction and the engine writer serialisation is
// released.
func (s *Session) abortTx() {
	s.drainResult()
	if s.tx != nil {
		_ = s.tx.Rollback() //nolint:errcheck // best-effort rollback on failure/teardown; error not actionable
		s.tx = nil
	}
	// Count the transaction closed (idempotent: a no-op when a prior COMMIT/
	// ROLLBACK/RESET/GOODBYE already counted it). This keeps the open-transaction
	// gauge balanced when abortTx is the path that ends the transaction (a FAILED
	// reclaim or a connection teardown that found the transaction still open).
	s.txClosed()
	s.txActive = false
}

// Close tears the session down on connection teardown: it drains any open cursor
// and rolls back any open explicit transaction so the engine writer serialisation
// is released immediately rather than lingering until the GC finalises the
// leaked Result/transaction (#1309). It is safe to call exactly once from the
// connection handler's deferred cleanup on every exit path (clean close, read or
// write error, panic). Idempotent: a second call, or a call on a session with no
// open transaction, is a no-op.
//
// An explicit transaction still open at this point is an abnormal disconnect —
// the client dropped the connection (or hit an idle timeout, or the handler
// panicked) without sending COMMIT, ROLLBACK, or RESET. Close counts it as
// [metricTxAbandoned] before the rollback so an operator can distinguish a
// leaked transaction reclaimed here from one ended in an orderly way. This is
// the only site that emits tx.abandoned: a FAILED-transition reclaim (#1312)
// goes through [Session.abortTx] directly and is an in-session state change, not
// a disconnect, so it is not counted abandoned.
func (s *Session) Close() {
	if s.tx != nil {
		incCounter(metricTxAbandoned)
	}
	s.abortTx()
}

// ─────────────────────────────────────────────────────────────────────────────
// Individual handlers
// ─────────────────────────────────────────────────────────────────────────────

func (s *Session) handleHello(m *proto.Hello) ([]any, error) {
	if s.state != StateNegotiation {
		return s.failTransition(&proto.Hello{})
	}

	// Bolt >= 5.1 split authentication out of HELLO into a dedicated LOGON
	// message: a 5.1+ driver sends a credential-less HELLO carrying only driver
	// metadata, then a LOGON carrying scheme/principal/credentials. Authenticating
	// the (empty) HELLO token against a credentialed AuthHandler would reject the
	// correctly-configured client before its LOGON is ever read. So on >= 5.1 the
	// server does NOT authenticate at HELLO and does NOT set authenticated;
	// HELLO(success) advances to the pre-LOGON StateAuthentication, from which only
	// LOGON/LOGOFF/RESET/GOODBYE are legal and a successful LOGON reaches READY.
	// (task #1470)
	if authDeferredToLogon(s.boltVersion) {
		next, transErr := HelloTransition(s.state, s.boltVersion, true)
		if transErr != nil {
			return s.failTransition(m)
		}
		s.state = next
		return []any{&proto.Success{Metadata: s.helloSuccessMetadata()}}, nil
	}

	// Bolt <= 5.0 (and the white-box tests, which run at the zero-value version):
	// HELLO carries the credentials and authenticates inline, advancing to READY.
	scheme, _ := extractString(m.Extra, "scheme")
	principal, _ := extractString(m.Extra, "principal")
	credentials, _ := extractString(m.Extra, "credentials")

	id, err := s.auth.Authenticate(scheme, principal, credentials)
	if err != nil {
		// A failed HELLO terminates the connection: the client never reached an
		// authenticated state, so there is nothing to recover and no reason to
		// keep the socket open for a second attempt on the same connection. The
		// FAILURE is written by the serve loop before it observes DEFUNCT and
		// closes, so the client still learns why. Making the connection DEFUNCT
		// (rather than FAILED) also removes the historical "failed HELLO → RESET
		// → READY" bypass at its root. HELLO is legal only in NEGOTIATION before
		// any BEGIN, so no transaction or cursor can be open here; a raw state
		// set is correct (no enterFailed reclaim is needed). (task #1345)
		s.state = StateDefunct
		s.log.Error("bolt: authentication failed", slog.String("session", s.id), slog.String("err", err.Error()))
		return []any{&proto.Failure{Code: authErrorCode(err), Message: s.sanitiseErr(err)}}, nil
	}
	s.identity = id
	s.authenticated = true

	next, transErr := HelloTransition(s.state, s.boltVersion, true)
	if transErr != nil {
		return s.failTransition(m)
	}
	s.state = next

	return []any{&proto.Success{Metadata: s.helloSuccessMetadata()}}, nil
}

// helloSuccessMetadata builds the SUCCESS metadata returned for a successful
// HELLO, identical for the inline-auth (<= 5.0) and deferred-auth (>= 5.1) paths.
func (s *Session) helloSuccessMetadata() map[string]packstream.Value {
	return map[string]packstream.Value{
		"server":        serverAgent,
		"connection_id": s.id,
		"hints":         map[string]packstream.Value{},
		"bolt_agent":    map[string]packstream.Value{"product": serverAgent},
	}
}

func (s *Session) handleLogon(m *proto.Logon) ([]any, error) {
	// LOGON is legal as the first authentication from the pre-LOGON
	// StateAuthentication (Bolt >= 5.1, after a credential-less HELLO) and as a
	// re-authentication from READY/TX_READY. (tasks #1345, #1470)
	if s.state != StateAuthentication && s.state != StateReady && s.state != StateTxReady {
		return s.failTransition(m)
	}

	// firstAuth is true when this LOGON is the connection's first authentication
	// (Bolt >= 5.1 deferred-auth flow). A FIRST authentication that fails must
	// terminate the connection per the Bolt 5.1 spec: respond FAILURE then close,
	// exactly like a failed <= 5.0 HELLO — the client never reached an
	// authenticated state, so there is nothing to recover. A RE-authentication
	// failure (from READY/TX_READY) stays recoverable via RESET, unchanged. (task #1470)
	firstAuth := s.state == StateAuthentication

	scheme, _ := extractString(m.Auth, "scheme")
	principal, _ := extractString(m.Auth, "principal")
	credentials, _ := extractString(m.Auth, "credentials")

	id, err := s.auth.Authenticate(scheme, principal, credentials)
	if err != nil {
		if firstAuth {
			// First authentication failed: terminate the connection (DEFUNCT). No
			// transaction or cursor can be open in StateAuthentication, so a raw
			// state set is correct (no enterFailed reclaim is needed). The FAILURE
			// is written by the serve loop before it observes DEFUNCT and closes,
			// so the client still learns why it was rejected. (task #1470)
			s.state = StateDefunct
			s.log.Error("bolt: authentication failed", slog.String("session", s.id), slog.String("err", err.Error()))
			return []any{&proto.Failure{Code: authErrorCode(err), Message: s.sanitiseErr(err)}}, nil
		}
		// LOGON re-authentication is legal in TX_READY, so a failed auth here can
		// leave an explicit transaction open; enterFailed reclaims it (#1312).
		s.enterFailed()
		s.log.Error("bolt: authentication failed", slog.String("session", s.id), slog.String("err", err.Error()))
		return []any{&proto.Failure{Code: authErrorCode(err), Message: s.sanitiseErr(err)}}, nil
	}
	s.identity = id
	s.authenticated = true

	next, transErr := Transition(s.state, m, true)
	if transErr != nil {
		return s.failTransition(m)
	}
	s.state = next

	return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
}

func (s *Session) handleLogoff() ([]any, error) {
	m := &proto.Logoff{}
	// LOGOFF is legal from READY/TX_READY (de-authorising an authenticated
	// connection) and from the pre-LOGON StateAuthentication (Bolt >= 5.1), where
	// it is a no-op de-authorisation that leaves the connection awaiting a fresh
	// LOGON. (tasks #1345, #1470)
	if s.state != StateAuthentication && s.state != StateReady && s.state != StateTxReady {
		return s.failTransition(m)
	}
	next, err := Transition(s.state, m, true)
	if err != nil {
		return s.failTransition(m)
	}
	s.state = next
	s.identity = Identity{}
	// LOGOFF de-authorizes the connection: a subsequent RUN/BEGIN/ROUTE must be
	// rejected until a fresh LOGON (or RESET+HELLO) re-authenticates. (task #1345)
	s.authenticated = false
	return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
}

func (s *Session) handleReset() ([]any, error) {
	// RESET is valid from any non-DEFUNCT state.
	if s.state == StateDefunct {
		return s.failTransition(&proto.Reset{})
	}

	// Drain any open result cursor.
	s.drainResult()

	// Roll back and discard any active explicit transaction.
	if s.tx != nil {
		_ = s.tx.Rollback() //nolint:errcheck // best-effort cleanup on reset
		s.tx = nil
		// Orderly end: count the transaction closed. Idempotent via txAccounted.
		s.txClosed()
	}

	s.txActive = false

	// RESET must never grant access that authentication has not. A connection
	// that has not completed a successful HELLO/LOGON (RESET sent as the first
	// message, or RESET after LOGOFF or a failed re-auth) returns to NEGOTIATION
	// — it awaits a successful HELLO and does NOT reach READY. Only an
	// authenticated connection clears, via the state machine, to READY. This
	// closes the pre-auth RESET authentication-bypass. (task #1345)
	if !s.authenticated {
		s.state = StateNegotiation
		return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
	}

	next, err := Transition(s.state, &proto.Reset{}, true)
	if err != nil {
		return s.failTransition(&proto.Reset{})
	}
	s.state = next

	return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
}

func (s *Session) handleGoodbye() ([]any, error) {
	s.drainResult()
	if s.tx != nil {
		_ = s.tx.Rollback() //nolint:errcheck // best-effort cleanup on goodbye
		s.tx = nil
		// Orderly end: count the transaction closed. Idempotent via txAccounted.
		s.txClosed()
	}
	s.state = StateDefunct
	// No response is sent for GOODBYE.
	return nil, nil
}

func (s *Session) handleRun(ctx context.Context, m *proto.Run) ([]any, error) {
	// Authentication gate: a connection that has not completed a successful
	// HELLO/LOGON (or was de-authorized by LOGOFF) must never execute a query,
	// even if it has somehow reached a READY/TX_READY state. This is the
	// defence-in-depth complement to the state-machine and RESET fixes for the
	// pre-auth bypass (task #1345). failTransition moves the session to FAILED
	// and emits a FAILURE, so the client is told the message was rejected.
	if !s.authenticated {
		return s.failTransition(m)
	}
	if s.state != StateReady && s.state != StateTxReady {
		return s.failTransition(m)
	}

	// Per-connection in-flight cursor cap. The Bolt v5 state machine
	// already prevents two auto-commit RUNs from co-existing (a
	// second RUN is illegal in StateStreaming), but inside an
	// explicit transaction every RUN appends a cursor to tx.results
	// that is not closed until COMMIT/ROLLBACK. Without the cap a
	// long-running transaction can accumulate an unbounded number
	// of cursors, each holding operator state.
	if n := s.inFlightCount(); n >= s.maxInFlight {
		// The cap is only reachable inside an explicit transaction (it counts
		// cursors appended to tx.results); enterFailed reclaims that transaction
		// so the writer serialisation is not held until RESET (#1312).
		s.enterFailed()
		return []any{&proto.Failure{
			Code:    "Neo.ClientError.General.LimitExceeded",
			Message: fmt.Sprintf("bolt: per-connection in-flight cursor cap reached (cap=%d, open=%d); commit/rollback or pull/discard before issuing more queries", s.maxInFlight, n),
		}}, nil
	}

	// Log any incoming bookmarks for observability; single-host server ignores
	// them for causal consistency but they should not be silently dropped.
	if bms := ExtractBookmarks(m.Extra); len(bms) > 0 {
		s.log.Debug("bolt: RUN bookmarks received",
			slog.String("session", s.id),
			slog.Any("bookmarks", bms))
	}

	// Extract optional statement timeout from extra metadata and apply the
	// server-side cap (maxStmtTimeout). When the client supplies no timeout
	// but maxStmtTimeout is set, the cap is applied unconditionally.
	if v, ok := m.Extra["timeout"]; ok {
		if ms, ok := v.(int64); ok {
			// Convert overflow-safely (#1484); a non-positive or overflowing
			// per-statement timeout is ignored rather than wrapping to a
			// non-positive duration.
			if d, ok := clientMillisToDuration(ms); ok {
				s.stmtTimeout = d
			}
		}
	}
	effective := s.stmtTimeout
	if s.maxStmtTimeout > 0 {
		switch {
		case effective <= 0:
			effective = s.maxStmtTimeout
		case effective > s.maxStmtTimeout:
			effective = s.maxStmtTimeout
		}
	}

	// Build execution context with optional deadline.
	runCtx := ctx
	if effective > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, effective)
		defer cancel()
	}

	// m.Parameters is map[string]packstream.Value, and packstream.Value is an
	// alias of any, so it already IS a map[string]any — pass it straight to the
	// engine instead of copying it per RUN (#1521). RunAny and tx.Run consume it
	// read-only: BindParams converts it once into the engine's internal form and
	// retains no reference to the map, and no engine path writes to it. The map
	// is freshly decoded per message and used only on this single message-loop
	// goroutine, so there is no aliasing or concurrency hazard. The explicit
	// conversion fails to compile (flagging the maintainer) if Value ever stops
	// being an alias.
	params := map[string]any(m.Parameters)

	var (
		result *cypher.Result
		runErr error
	)
	if s.txActive && s.tx != nil {
		// Run inside the explicit transaction so that index-buffer writes are
		// scoped to the transaction lifecycle (commit/rollback in tx.go).
		result, runErr = s.tx.Run(m.Query, params)
	} else {
		// Autocommit mode (or defensive fallback when txActive is unexpectedly
		// false in StateTxReady): route through RunAny so that reads take the
		// lock-free Engine.Run path and only writes acquire the single-writer
		// lock via Engine.RunInTx. Routing all autocommit queries through
		// RunInTxAny would block a read-only session behind any open explicit
		// write transaction, violating the 'readers do not block writers where
		// avoidable' mandate (task #1432).
		result, runErr = s.eng.RunAny(runCtx, m.Query, params)
	}

	next, transErr := Transition(s.state, m, runErr == nil)
	if transErr != nil {
		if result != nil {
			_ = result.Close() //nolint:errcheck // best-effort close on unexpected path
		}
		return s.failTransition(m)
	}
	// A statement that failed to execute inside an explicit transaction computes
	// next == StateFailed; transitionTo routes that through enterFailed so the
	// transaction is rolled back and the writer serialisation released NOW rather
	// than held until the client's RESET (#1312).
	s.transitionTo(next)

	if runErr != nil {
		s.log.Error("bolt: query execution failed", slog.String("session", s.id), slog.String("err", runErr.Error()))
		return []any{&proto.Failure{
			Code:    FailureCode(runErr),
			Message: s.sanitiseErr(runErr),
		}}, nil
	}

	s.result = result
	s.columns = result.Columns()

	return []any{&proto.Success{
		Metadata: map[string]packstream.Value{
			"fields": stringsToValues(s.columns),
			"qid":    int64(-1),
		},
	}}, nil
}

//nolint:gocyclo // pull loop has context cancellation, cursor error, has_more peek, and state transition branches; complexity is irreducible.
func (s *Session) handlePull(ctx context.Context, m *proto.Pull) ([]any, error) {
	if s.state != StateStreaming && s.state != StateTxStreaming {
		return s.failTransition(m)
	}
	// The single open stream always has qid -1 (RUN reports qid=-1, and a new RUN
	// is rejected while streaming). An explicit qid >= 0 names a stream that does
	// not exist and must FAIL rather than be served against the current one (#1783).
	if m.QID >= 0 {
		return s.failWith("Neo.ClientError.Request.Invalid",
			fmt.Sprintf("no such query: qid %d", m.QID)), nil
	}

	n := m.N
	if n == 0 {
		n = -1 // treat 0 as "pull all" for safety
	}

	var responses []any
	fetched := int64(0)

	// On the streaming-sink path the sink encodes-and-writes each RECORD
	// synchronously before handlePull advances to the next row (the serve loop's
	// task #1350 path: setRecordSink -> writeResponse -> sendResponse consumes
	// rec.Data on the calling goroutine and retains nothing). So a single row
	// buffer and Record can be reused across the whole PULL instead of
	// allocating both per row (#1520). The buffered path (no sink — sessions
	// driven directly through HandleMessage) retains every RECORD in responses,
	// so it must keep allocating a fresh row per record.
	streaming := s.records != nil
	var (
		rowBuf    []packstream.Value
		streamRec proto.Record
	)
	if streaming {
		rowBuf = make([]packstream.Value, len(s.columns))
	}

	// emit hands one RECORD to the streaming sink when one is installed, reusing
	// streamRec/rowBuf; otherwise it buffers a freshly-allocated RECORD.
	// Streaming per row adds no write amplification — the buffered path also
	// issued one chunked write per RECORD after the handler returned; only the
	// timing changes — and the blocking write is the backpressure that keeps a
	// slow reader from forcing the whole page into memory. A sink error is
	// terminal: the wire may hold a partial chunk.
	emit := func(row []packstream.Value) error {
		if streaming {
			streamRec.Data = row
			return s.records(&streamRec)
		}
		responses = append(responses, &proto.Record{Data: row})
		return nil
	}

	// newRow returns the buffer to fill for the next streamed row: the reused
	// rowBuf when streaming, or a fresh slice when buffering (a buffered RECORD
	// is retained in responses, so it must not alias rowBuf). The has_more peek
	// below always allocates fresh because the peeked row outlives this PULL.
	newRow := func() []packstream.Value {
		if streaming {
			return rowBuf
		}
		return make([]packstream.Value, len(s.columns))
	}

	// Emit the pre-fetched row from a previous partial PULL, if any.
	if s.peeked != nil {
		row := *s.peeked
		s.peeked = nil
		if err := emit(row); err != nil {
			return s.abortStream(err)
		}
		fetched++
	}

	for n <= 0 || fetched < n {
		if ctx.Err() != nil {
			// enterFailed drains the cursor and rolls back any open explicit
			// transaction (TX_STREAMING), releasing the writer serialisation (#1312).
			s.enterFailed()
			return []any{&proto.Failure{
				Code:    "Neo.TransientError.General.RequestInterrupted",
				Message: ctx.Err().Error(),
			}}, nil
		}
		if !s.result.Next() {
			break
		}
		// Read the row positionally (#1499): the engine result is always
		// materialised, so ValueAt reads the column-oriented backing store
		// directly and never builds the per-row map that Record() would. row is
		// the reused streaming buffer or a fresh slice (#1520, see newRow).
		row := newRow()
		for i := range s.columns {
			row[i] = exprToPackstream(s.result.ValueAt(i), s.boltVersion.Major)
		}
		if err := emit(row); err != nil {
			return s.abortStream(err)
		}
		fetched++
	}

	if err := s.result.Err(); err != nil {
		// enterFailed drains the cursor and rolls back any open explicit
		// transaction (TX_STREAMING), releasing the writer serialisation (#1312).
		s.enterFailed()
		s.log.Error("bolt: result stream error", slog.String("session", s.id), slog.String("err", err.Error()))
		return []any{&proto.Failure{
			Code:    FailureCode(err),
			Message: s.sanitiseErr(err),
		}}, nil
	}

	// Peek ahead to determine has_more: attempt to read one more row from the
	// cursor. If the peek succeeds, we store it for the next PULL call and
	// report has_more=true. This is the approach specified in the Bolt v5 spec.
	var hasMore bool
	if n > 0 && fetched == n {
		// Only peek when we might have hit the n-row limit; pull-all (n≤0) or
		// early-termination (fetched < n) are always exhausted.
		if s.result.Next() {
			row := make([]packstream.Value, len(s.columns))
			for i := range s.columns {
				row[i] = exprToPackstream(s.result.ValueAt(i), s.boltVersion.Major)
			}
			s.peeked = &row
			hasMore = true
		}
	}

	// Capture any plan-time notifications (e.g. a Cartesian-product warning,
	// #1483) before draining the cursor; they are reported in the terminal PULL
	// SUCCESS metadata, as Neo4j does.
	var notifications []packstream.Value
	if !hasMore && s.result != nil {
		notifications = notificationsToValues(s.result.Notifications())
	}

	// Transition state based on has_more.
	next, transErr := StreamingTransition(s.state, hasMore)
	if transErr != nil {
		return s.failTransition(m)
	}
	if !hasMore {
		s.drainResult() // close and nil the cursor
		s.peeked = nil
	}
	s.state = next

	meta := map[string]packstream.Value{
		"has_more": hasMore,
	}
	if !hasMore {
		meta["bookmark"] = s.bookmark
		if len(notifications) > 0 {
			meta["notifications"] = notifications
		}
	}
	responses = append(responses, &proto.Success{Metadata: meta})
	return responses, nil
}

func (s *Session) handleDiscard(m *proto.Discard) ([]any, error) {
	if s.state != StateStreaming && s.state != StateTxStreaming {
		return s.failTransition(m)
	}
	// As in handlePull: an explicit qid >= 0 names a non-existent stream (#1783).
	if m.QID >= 0 {
		return s.failWith("Neo.ClientError.Request.Invalid",
			fmt.Sprintf("no such query: qid %d", m.QID)), nil
	}

	// Defense-in-depth: if the open cursor already carries a statement error,
	// treat DISCARD like a failure so the client is notified rather than
	// receiving a spurious SUCCESS. enterFailed drains the cursor and reclaims
	// any open explicit transaction.
	if s.result != nil {
		if stmtErr := s.result.Err(); stmtErr != nil {
			s.drainResult()
			s.enterFailed()
			return []any{&proto.Failure{
				Code:    FailureCode(stmtErr),
				Message: s.sanitiseErr(stmtErr),
			}}, nil
		}
	}

	s.drainResult()

	next, err := Transition(s.state, m, true)
	if err != nil {
		return s.failTransition(m)
	}
	s.state = next

	return []any{&proto.Success{Metadata: map[string]packstream.Value{
		"has_more": false,
		"bookmark": s.bookmark,
	}}}, nil
}

func (s *Session) handleBegin(ctx context.Context, m *proto.Begin) ([]any, error) {
	// Authentication gate (see handleRun): an unauthenticated connection must
	// not open a transaction. (task #1345)
	if !s.authenticated {
		return s.failTransition(m)
	}
	if s.state != StateReady {
		return s.failTransition(m)
	}
	// Nested BEGIN (txActive already true) must be rejected.
	if s.txActive {
		return []any{&proto.Failure{
			Code:    "Neo.ClientError.Statement.SemanticError",
			Message: "nested transactions are not supported",
		}}, nil
	}

	// Log incoming bookmarks for observability.
	if bms := ExtractBookmarks(m.Extra); len(bms) > 0 {
		s.log.Debug("bolt: BEGIN bookmarks received",
			slog.String("session", s.id),
			slog.Any("bookmarks", bms))
	}

	// Determine the effective transaction timeout. A finite bound is mandatory:
	// the explicit transaction holds the engine's single-writer serialisation
	// from BEGIN until COMMIT/ROLLBACK, so a client that BEGINs and then stalls
	// would otherwise block every other writer indefinitely (#1302). Precedence:
	// the client-supplied tx_timeout if present, else the server default
	// (defaultTxTimeout). The server-side statement cap (maxStmtTimeout), when
	// set, clamps the result so a client can never request a longer hold than the
	// operator permits.
	effective := s.defaultTxTimeout
	if v, ok := m.Extra["tx_timeout"]; ok {
		if ms, ok := v.(int64); ok {
			// Convert overflow-safely (#1484). A non-positive or overflowing
			// tx_timeout (e.g. 1<<62 → product 0, or math.MaxInt64 → negative)
			// is treated as "unset" and leaves the server default in place,
			// rather than wrapping to a non-positive duration that would leave
			// txDeadline zero and disarm the writer-lock reaper.
			if d, ok := clientMillisToDuration(ms); ok {
				effective = d
			}
		}
	}
	if s.maxStmtTimeout > 0 && (effective <= 0 || effective > s.maxStmtTimeout) {
		effective = s.maxStmtTimeout
	}

	// Determine transaction mode (default: "w").
	mode := "w"
	if v, ok := m.Extra["mode"]; ok {
		if modeStr, ok := v.(string); ok && modeStr == "r" {
			mode = "r"
		}
	}

	// Open the engine transaction rooted at the CONNECTION context (so a dropped
	// connection or a server shutdown cancels an in-flight statement) bounded by
	// the effective timeout. BeginTx acquires the engine writer serialisation; a
	// failure here (e.g. an already-cancelled context) leaves the session in
	// READY with no open transaction.
	tx, err := newTx(ctx, s.eng, mode, effective)
	if err != nil {
		// newTx failed before acquiring any resources (s.tx is still nil), so
		// enterFailed has no transaction to reclaim here; it is used for the single
		// FAILED-entry invariant (#1312).
		s.enterFailed()
		s.log.Error("bolt: begin transaction failed", slog.String("session", s.id), slog.String("err", err.Error()))
		return []any{&proto.Failure{
			Code:    FailureCode(err),
			Message: s.sanitiseErr(err),
		}}, nil
	}

	next, transErr := Transition(s.state, m, true)
	if transErr != nil {
		// Roll back the just-opened transaction so the writer serialisation is not
		// leaked on the (unreachable in practice) illegal-transition path.
		_ = tx.Rollback() //nolint:errcheck // best-effort cleanup; error not actionable
		return s.failTransition(m)
	}
	s.state = next
	s.txActive = true
	s.tx = tx
	// Record the transaction's absolute wall-clock deadline so the serve loop can
	// reap an idle-but-open transaction at the timeout even if the client keeps
	// the connection alive with no-op messages (task #1346). A non-positive
	// effective timeout (never produced by the production server, which installs
	// a finite default) leaves the deadline zero, i.e. no reaper.
	if effective > 0 {
		s.txDeadline = s.clk.Now().Add(effective)
	} else {
		s.txDeadline = time.Time{}
	}
	// Count the transaction opened (the opened side of the open-transaction gauge
	// derivation opened − closed). The matching txClosed runs on whichever path
	// ends it, keeping the derived gauge balanced.
	s.txOpened()
	return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
}

func (s *Session) handleCommit() ([]any, error) {
	m := &proto.Commit{}
	if s.state != StateTxReady {
		return s.failTransition(m)
	}

	// Commit the transaction if one is active.
	if s.tx != nil {
		if err := s.tx.Commit(); err != nil {
			// A failed Commit already released the engine writer serialisation and
			// finished the engine transaction (cypher.ExplicitTx.Commit defers
			// release even on error). enterFailed clears the now-finished s.tx; its
			// abortTx→Rollback is a clean ErrTxFinished no-op, never a double
			// rollback (#1312).
			s.enterFailed()
			s.log.Error("bolt: commit failed", slog.String("session", s.id), slog.String("err", err.Error()))
			return []any{&proto.Failure{
				Code:    FailureCode(err),
				Message: s.sanitiseErr(err),
			}}, nil
		}
		s.tx = nil
		// Orderly end: count the transaction closed (the closed side of the
		// open-transaction gauge derivation). Idempotent via txAccounted.
		s.txClosed()
	}

	next, err := Transition(s.state, m, true)
	if err != nil {
		return s.failTransition(m)
	}
	s.state = next
	s.txActive = false
	s.bookmark = NextBookmark()
	return []any{&proto.Success{Metadata: map[string]packstream.Value{
		"bookmark": s.bookmark,
	}}}, nil
}

func (s *Session) handleRollback() ([]any, error) {
	m := &proto.Rollback{}
	if s.state != StateTxReady {
		return s.failTransition(m)
	}

	// Roll back the transaction if one is active.
	if s.tx != nil {
		_ = s.tx.Rollback() //nolint:errcheck // rollback errors are not actionable; best-effort cleanup
		s.tx = nil
		// Orderly end: count the transaction closed. Idempotent via txAccounted.
		s.txClosed()
	}

	next, err := Transition(s.state, m, true)
	if err != nil {
		return s.failTransition(m)
	}
	s.state = next
	s.txActive = false
	return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
}

func (s *Session) handleRoute(m *proto.Route) ([]any, error) {
	// ROUTE is valid only once the session has completed HELLO (and LOGON on
	// Bolt >= 5.1), i.e. from READY or TX_READY. An unauthenticated client in
	// StateNegotiation must not elicit any server response beyond the
	// handshake/auth exchange; ROUTE in StateNegotiation is rejected as an
	// illegal transition (Neo.ClientError.Request.Invalid via failTransition).
	//
	// This is wire-compatible with the official Neo4j Go driver: in routing
	// mode it completes HELLO (and LOGON for Bolt >= 5.1) before issuing ROUTE.
	// The driver's bolt4/bolt5 GetRoutingTable both assert the Ready state
	// before sending ROUTE, so a legitimate driver is never in StateNegotiation
	// when it sends ROUTE.
	//
	// Authentication gate (see handleRun): an unauthenticated connection must
	// not receive a routing table. Checking authentication first also keeps a
	// pre-HELLO ROUTE rejected with the same failTransition response as before
	// (the connection is in NEGOTIATION, !authenticated). (task #1345)
	if !s.authenticated {
		return s.failTransition(m)
	}
	if s.state != StateReady && s.state != StateTxReady {
		return s.failTransition(m)
	}
	rt := RoutingTable(s.localAddr)
	return []any{&proto.Success{Metadata: rt}}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// abortStream handles a record-sink write failure mid-PULL: the client
// connection can no longer carry a well-formed message (the failed write may
// have left a partial chunk on the wire), so the session enters FAILED —
// draining the cursor and rolling back any open explicit transaction, which
// releases the engine writer serialisation NOW (#1312) — and the error is
// surfaced wrapped in [errRecordWrite] so the serve loop tears the connection
// down without writing anything further.
func (s *Session) abortStream(err error) ([]any, error) {
	s.enterFailed()
	return nil, fmt.Errorf("%w: %w", errRecordWrite, err)
}

// drainResult closes and nils the current result cursor if one is open. It
// also discards any pre-fetched peek row.
func (s *Session) drainResult() {
	s.peeked = nil
	if s.result != nil {
		_ = s.result.Close() //nolint:errcheck // best-effort drain; error is not actionable here
		s.result = nil
		s.columns = nil
	}
}

// enterFailed is the single funnel for every transition into FAILED. It moves
// the session to FAILED and, if an explicit transaction is still open, rolls it
// back NOW via [Session.abortTx] rather than waiting for the client's RESET.
//
// Entering FAILED means no further RUN/COMMIT can run until RESET, and RESET
// itself discards the transaction; a FAILED session can therefore never legally
// resume the open transaction. Holding its writes — and the engine's
// single-writer serialisation the transaction acquired at BEGIN — for the whole
// FAILED→RESET window would block every other writer and keep a partial
// transaction live for that entire window (or forever, if the client never
// sends RESET and the connection is not torn down). Reclaiming at the FAILED
// transition releases the writer serialisation immediately (#1312).
//
// abortTx is idempotent: a subsequent RESET (or the connection-teardown
// [Session.Close]) finds tx already nil and does not double-roll-back. Every
// transition into FAILED is routed through this one helper, so "entering FAILED
// reclaims any open transaction" is an invariant of the state machine rather
// than a property of a single call site. The routes are:
//   - the two failure helpers [Session.failTransition] and [Session.failWith]
//     (illegal messages, the context-cancellation early return, an unrecognised
//     message);
//   - the handler-inline FAILED entries (the in-flight cursor cap, a PULL cursor
//     error or context-cancellation, a failed LOGON re-authentication, a failed
//     COMMIT);
//   - [Session.transitionTo], for a statement that failed to execute and whose
//     [Transition]-computed next state is therefore StateFailed.
//
// In particular the context-cancellation early return in [Session.HandleMessage]
// — which never reaches a handler — still reclaims the transaction.
// reapTimedOutTx rolls back an explicit transaction that has exceeded its
// wall-clock deadline while the connection was kept alive (idle, or by no-op
// pings), releasing the engine's global writer lock, and moves the session to
// FAILED so the client's next message receives a FAILURE and must RESET. It is
// invoked by the serve loop on a read-deadline timeout, on the session's own
// goroutine, so it never races the single-threaded session. enterFailed drains
// any cursor and rolls back the transaction (clearing txActive); txDeadline is
// cleared here so the serve loop stops tightening the read deadline. (task #1346)
func (s *Session) reapTimedOutTx() {
	incCounter(metricTxTimedOut)
	s.enterFailed()
	s.txDeadline = time.Time{}
}

func (s *Session) enterFailed() {
	s.state = StateFailed
	// Entering FAILED invalidates any in-flight cursor: no PULL/DISCARD can
	// continue until RESET. Drain it unconditionally (this also discards a
	// pending peek row) so an auto-commit stream's cursor is closed promptly, not
	// only on RESET/teardown.
	s.drainResult()
	// Roll back any open explicit transaction so the engine writer serialisation
	// it holds is released NOW. abortTx is idempotent (a nil tx is a no-op) and
	// drains the cursor again harmlessly.
	if s.tx != nil {
		s.abortTx()
	}
}

// transitionTo applies the next state computed by [Transition] for a legal
// message. A statement that FAILED to execute (a RUN/PULL/COMMIT/ROLLBACK whose
// operation returned an error) is a legal transition whose computed next state
// is StateFailed; routing that case through [Session.enterFailed] reclaims any
// open explicit transaction, so this — like the failure helpers and the
// handler-inline FAILED entries — preserves the "entering FAILED reclaims the
// open transaction" invariant (#1312). Every other computed state is applied
// verbatim.
func (s *Session) transitionTo(next State) {
	if next == StateFailed {
		s.enterFailed()
		return
	}
	s.state = next
}

// failTransition moves the session to FAILED (reclaiming any open transaction)
// and returns a FAILURE response for an illegal state transition. The FAILURE
// message reports the state the message was illegal IN (captured before the
// FAILED transition), not the post-transition FAILED state: reporting the
// originating state is the actionable diagnostic for a client that sent, say, a
// PULL with no active stream (the message then reads "in state READY", telling
// the client what it should have sent instead, rather than the uninformative
// "in state FAILED" the session lands in for every illegal transition alike).
func (s *Session) failTransition(msg any) ([]any, error) {
	origin := s.state
	s.enterFailed()
	return []any{&proto.Failure{
		Code:    "Neo.ClientError.Request.Invalid",
		Message: fmt.Sprintf("illegal message %T in state %s", msg, origin),
	}}, nil
}

// failWith moves the session to FAILED (reclaiming any open transaction) and
// returns a FAILURE response.
func (s *Session) failWith(code, message string) []any {
	s.enterFailed()
	return []any{&proto.Failure{Code: code, Message: message}}
}

// authErrorCode maps an auth error to a Neo4j-compatible error code string.
func authErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrSchemeUnknown):
		return "Neo.ClientError.Security.AuthProviderFailed"
	default:
		return "Neo.ClientError.Security.Unauthorized"
	}
}

// sanitiseErr returns a safe client-visible error message for err, suppressing
// internal Go type names, file paths, and stack details. The real error is
// logged server-side by the caller using the session ID for correlation.
//
// Mapping:
//   - Auth errors → "Authentication failed." (never reveal the cause).
//   - Client-fault errors (parse/semantic/type errors, constraint violations,
//     resource caps, transaction-too-large, index/procedure misuse — see
//     [isClientFaultErr]) → the error text, which is the client's own
//     diagnostic (task #1353).
//   - All other errors → a generic message with a session ID for log correlation.
func (s *Session) sanitiseErr(err error) string {
	if err == nil {
		return ""
	}
	// Auth errors: never reveal the underlying cause to the client.
	if errors.Is(err, ErrSchemeUnknown) || errors.Is(err, ErrAuthFailed) {
		return "Authentication failed."
	}
	// Client-fault errors carry diagnostics about the client's own request
	// (the parse position, the undefined variable, the violated constraint,
	// the tripped cap) — replacing them with the generic internal-error text
	// would break debuggability without protecting anything internal.
	if isClientFaultErr(err) {
		return err.Error()
	}
	// All other errors (internal engine, storage, unexpected): generic + session ID.
	return fmt.Sprintf("An internal error occurred. See server logs for details (session: %s).", s.id)
}

// extractString retrieves a string value from a packstream map by key.
func extractString(m map[string]packstream.Value, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// stringsToValues converts a []string to []packstream.Value ([]any containing
// strings) for use in SUCCESS metadata.
func stringsToValues(ss []string) []packstream.Value {
	out := make([]packstream.Value, len(ss))
	for i, s := range ss {
		out[i] = packstream.Value(s)
	}
	return out
}

// notificationsToValues encodes the engine's plan-time notifications as the
// Bolt SUCCESS "notifications" metadata: a list of maps with the Neo4j-style
// keys (code, title, description, severity, category). It returns nil when there
// are no notifications so the metadata key is omitted entirely (#1483).
func notificationsToValues(ns []cypher.Notification) []packstream.Value {
	if len(ns) == 0 {
		return nil
	}
	out := make([]packstream.Value, len(ns))
	for i, n := range ns {
		out[i] = map[string]packstream.Value{
			"code":        packstream.Value(n.Code),
			"title":       packstream.Value(n.Title),
			"description": packstream.Value(n.Description),
			"severity":    packstream.Value(n.Severity),
			"category":    packstream.Value(n.Category),
		}
	}
	return out
}

// exprToPackstream converts an expr.Value (or any interface{} from exec.Record)
// to a packstream.Value suitable for inclusion in a RECORD message.
// boltMajor is the negotiated Bolt protocol major version; it governs the
// encoding of temporal types (e.g. DateTime tag selection, task #1434).
//
// Handles: nil, expr.Null, expr.IntegerValue, expr.FloatValue, expr.BoolValue,
// expr.StringValue, expr.NodeValue, expr.RelationshipValue, expr.ListValue,
// expr.MapValue, and all six temporal expr.Value kinds. Unknown types are
// converted to their string representation.
func exprToPackstream(v any, boltMajor uint8) packstream.Value {
	if v == nil {
		return nil
	}

	switch x := v.(type) {
	case expr.Value:
		return exprValueToPackstream(x, boltMajor)
	case int64:
		return x
	case float64:
		return x
	case bool:
		return x
	case string:
		return x
	default:
		return fmt.Sprintf("%v", v)
	}
}

// exprValueToPackstream converts a typed expr.Value to packstream.Value.
// boltMajor is the negotiated Bolt major version; for temporal values the
// correct PackStream Struct tag depends on whether the client speaks Bolt v4.4
// or v5.0+ (task #1434).
//
//nolint:gocyclo,cyclop // dispatch over all expr.Value kinds; complexity is irreducible
func exprValueToPackstream(v expr.Value, boltMajor uint8) packstream.Value {
	if v == nil {
		return nil
	}

	switch x := v.(type) {
	case expr.IntegerValue:
		return int64(x)
	case expr.FloatValue:
		return float64(x)
	case expr.BoolValue:
		return bool(x)
	case expr.StringValue:
		return string(x)
	case expr.NodeValue:
		props := make(map[string]packstream.Value, len(x.Properties))
		for k, pv := range x.Properties {
			props[k] = exprValueToPackstream(pv, boltMajor)
		}
		labels := make([]packstream.Value, len(x.Labels))
		for i, l := range x.Labels {
			labels[i] = l
		}
		return map[string]packstream.Value{
			"id":         int64(x.ID),
			"labels":     labels,
			"properties": props,
		}
	case expr.RelationshipValue:
		props := make(map[string]packstream.Value, len(x.Properties))
		for k, pv := range x.Properties {
			props[k] = exprValueToPackstream(pv, boltMajor)
		}
		return map[string]packstream.Value{
			"id":         int64(x.ID),
			"start":      int64(x.StartID),
			"end":        int64(x.EndID),
			"type":       x.Type,
			"properties": props,
		}
	case expr.PathValue:
		nodes := make([]packstream.Value, len(x.Nodes))
		for i, n := range x.Nodes {
			nodes[i] = exprValueToPackstream(n, boltMajor)
		}
		rels := make([]packstream.Value, len(x.Relationships))
		for i, r := range x.Relationships {
			rels[i] = exprValueToPackstream(r, boltMajor)
		}
		return map[string]packstream.Value{
			"nodes":         nodes,
			"relationships": rels,
		}
	case expr.MapValue:
		m := make(map[string]packstream.Value, len(x))
		for k, mv := range x {
			m[k] = exprValueToPackstream(mv, boltMajor)
		}
		return m

	// ── Temporal types (task #1434) ──────────────────────────────────────────
	// PackStream Struct tags and field layouts per the Bolt protocol and the
	// neo4j-go-driver v5 hydrator (internal/bolt/hydrator.go), which is the
	// authoritative decoder contract:
	//   DateValue          → 'D' 0x44  [epochDay int64]
	//   LocalTimeValue     → 't' 0x74  [nanosOfDay int64]
	//   TimeValue          → 'T' 0x54  [nanosOfDay int64, tzOffsetSec int64]
	//   LocalDateTimeValue → 'd' 0x64  [epochSecond int64, nano int64]
	//   DurationValue      → 'E' 0x45  [months int64, days int64, seconds int64, nanos int64]
	//
	// DateTimeValue depends on the negotiated version (UTC mode for Bolt v5.0+,
	// legacy mode for v4.4) and on whether the zone carries an IANA name:
	//   v5.0+ offset zone  → 'I' 0x49  [utcEpochSec int64, nano int64, tzOffsetSec int64]
	//   v5.0+ named zone   → 'i' 0x69  [utcEpochSec int64, nano int64, tzId string]
	//   v4.4  offset zone  → 'F' 0x46  [localEpochSec int64, nano int64, tzOffsetSec int64]
	//   v4.4  named zone   → 'f' 0x66  [localEpochSec int64, nano int64, tzId string]
	// (legacy "local" seconds are the wall-clock instant expressed as if UTC,
	//  i.e. utcEpochSec + tzOffsetSec — see hydrator.dateTimeOffset).
	case expr.DateValue:
		epochDay := x.ToTime().Unix() / 86400
		return packstream.Struct{Tag: 0x44, Fields: []packstream.Value{epochDay}}
	case expr.LocalTimeValue:
		return packstream.Struct{Tag: 0x74, Fields: []packstream.Value{x.Nanos}}
	case expr.TimeValue:
		return packstream.Struct{Tag: 0x54, Fields: []packstream.Value{x.Nanos, int64(x.OffsetSec)}}
	case expr.LocalDateTimeValue:
		return packstream.Struct{Tag: 0x64, Fields: []packstream.Value{
			x.T.Unix(),
			int64(x.T.Nanosecond()),
		}}
	case expr.DateTimeValue:
		return dateTimeToPackstream(x, boltMajor)
	case expr.DurationValue:
		return packstream.Struct{Tag: 0x45, Fields: []packstream.Value{
			x.Months,
			x.Days,
			x.Seconds,
			int64(x.Nanos),
		}}

	default:
		// expr.Null, expr.nullValue, or any unknown value kind.
		if x == nil || x == expr.Null {
			return nil
		}
		return x.String()
	}
}

// dateTimeToPackstream encodes a [expr.DateTimeValue] as the PackStream Struct
// the negotiated Bolt version expects. Bolt v5.0+ uses the UTC convention (the
// epoch second is the true UTC instant); Bolt v4.4 uses the legacy convention
// (the seconds field is the wall-clock instant expressed as if UTC). A zone
// carrying an IANA name (detected the same way as [expr.DateTimeValue.String],
// i.e. the location name contains "/") is encoded as a named-zone Struct; any
// other zone (UTC or fixed offset) is encoded as an offset Struct (task #1434).
func dateTimeToPackstream(x expr.DateTimeValue, boltMajor uint8) packstream.Value {
	utcEpochSec := x.T.Unix()
	nano := int64(x.T.Nanosecond())
	_, offsetSec := x.T.Zone()
	locName := x.T.Location().String()
	named := strings.Contains(locName, "/")

	if boltMajor >= 5 {
		// UTC mode: the seconds field is the true UTC epoch second.
		if named {
			return packstream.Struct{Tag: 0x69, Fields: []packstream.Value{utcEpochSec, nano, locName}}
		}
		return packstream.Struct{Tag: 0x49, Fields: []packstream.Value{utcEpochSec, nano, int64(offsetSec)}}
	}
	// Legacy mode (v4.4 and earlier without the UTC patch): the seconds field is
	// the local wall-clock instant expressed as if it were UTC.
	localEpochSec := utcEpochSec + int64(offsetSec)
	if named {
		return packstream.Struct{Tag: 0x66, Fields: []packstream.Value{localEpochSec, nano, locName}}
	}
	return packstream.Struct{Tag: 0x46, Fields: []packstream.Value{localEpochSec, nano, int64(offsetSec)}}
}
