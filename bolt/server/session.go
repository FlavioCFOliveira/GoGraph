package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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

	// txActive indicates that an explicit transaction is open (BEGIN called).
	txActive bool

	// tx is the active explicit transaction, non-nil when txActive is true.
	tx *Tx

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
	s.txActive = false
}

// Close tears the session down on connection teardown: it drains any open cursor
// and rolls back any open explicit transaction so the engine writer serialisation
// is released immediately rather than lingering until the GC finalises the
// leaked Result/transaction (#1309). It is safe to call exactly once from the
// connection handler's deferred cleanup on every exit path (clean close, read or
// write error, panic). Idempotent: a second call, or a call on a session with no
// open transaction, is a no-op.
func (s *Session) Close() {
	s.abortTx()
}

// ─────────────────────────────────────────────────────────────────────────────
// Individual handlers
// ─────────────────────────────────────────────────────────────────────────────

func (s *Session) handleHello(m *proto.Hello) ([]any, error) {
	if s.state != StateNegotiation {
		return s.failTransition(&proto.Hello{})
	}

	scheme, _ := extractString(m.Extra, "scheme")
	principal, _ := extractString(m.Extra, "principal")
	credentials, _ := extractString(m.Extra, "credentials")

	id, err := s.auth.Authenticate(scheme, principal, credentials)
	if err != nil {
		// HELLO is legal only in NEGOTIATION, before any BEGIN, so no transaction
		// or cursor can be open here; a raw state set is correct (no enterFailed
		// reclaim is needed, unlike the FAILED entries reachable post-BEGIN).
		s.state = StateFailed
		s.log.Error("bolt: authentication failed", slog.String("session", s.id), slog.String("err", err.Error()))
		return []any{&proto.Failure{Code: authErrorCode(err), Message: s.sanitiseErr(err)}}, nil
	}
	s.identity = id

	next, transErr := Transition(s.state, m, true)
	if transErr != nil {
		return s.failTransition(m)
	}
	s.state = next

	return []any{&proto.Success{
		Metadata: map[string]packstream.Value{
			"server":        serverAgent,
			"connection_id": s.id,
			"hints":         map[string]packstream.Value{},
			"bolt_agent":    map[string]packstream.Value{"product": serverAgent},
		},
	}}, nil
}

func (s *Session) handleLogon(m *proto.Logon) ([]any, error) {
	if s.state != StateReady && s.state != StateTxReady {
		return s.failTransition(m)
	}

	scheme, _ := extractString(m.Auth, "scheme")
	principal, _ := extractString(m.Auth, "principal")
	credentials, _ := extractString(m.Auth, "credentials")

	id, err := s.auth.Authenticate(scheme, principal, credentials)
	if err != nil {
		// LOGON re-authentication is legal in TX_READY, so a failed auth here can
		// leave an explicit transaction open; enterFailed reclaims it (#1312).
		s.enterFailed()
		s.log.Error("bolt: authentication failed", slog.String("session", s.id), slog.String("err", err.Error()))
		return []any{&proto.Failure{Code: authErrorCode(err), Message: s.sanitiseErr(err)}}, nil
	}
	s.identity = id

	next, transErr := Transition(s.state, m, true)
	if transErr != nil {
		return s.failTransition(m)
	}
	s.state = next

	return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
}

func (s *Session) handleLogoff() ([]any, error) {
	m := &proto.Logoff{}
	if s.state != StateReady && s.state != StateTxReady {
		return s.failTransition(m)
	}
	next, err := Transition(s.state, m, true)
	if err != nil {
		return s.failTransition(m)
	}
	s.state = next
	s.identity = Identity{}
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
	}

	next, err := Transition(s.state, &proto.Reset{}, true)
	if err != nil {
		return s.failTransition(&proto.Reset{})
	}
	s.state = next
	s.txActive = false

	return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
}

func (s *Session) handleGoodbye() ([]any, error) {
	s.drainResult()
	if s.tx != nil {
		_ = s.tx.Rollback() //nolint:errcheck // best-effort cleanup on goodbye
		s.tx = nil
	}
	s.state = StateDefunct
	// No response is sent for GOODBYE.
	return nil, nil
}

func (s *Session) handleRun(ctx context.Context, m *proto.Run) ([]any, error) {
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
		if ms, ok := v.(int64); ok && ms > 0 {
			s.stmtTimeout = time.Duration(ms) * time.Millisecond
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

	// Convert proto params to map[string]any for RunAny / tx.Run.
	params := make(map[string]any, len(m.Parameters))
	for k, v := range m.Parameters {
		params[k] = v
	}

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
		// false in StateTxReady): route through RunInTxAny so that write queries
		// (CREATE, MERGE, SET, DELETE) are handled by the write-aware planner.
		// Read-only queries pass through the same code path without side-effects.
		result, runErr = s.eng.RunInTxAny(runCtx, m.Query, params)
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

	n := m.N
	if n == 0 {
		n = -1 // treat 0 as "pull all" for safety
	}

	var responses []any
	fetched := int64(0)

	// Emit the pre-fetched row from a previous partial PULL, if any.
	if s.peeked != nil {
		responses = append(responses, &proto.Record{Data: *s.peeked})
		s.peeked = nil
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
		rec := s.result.Record()
		row := make([]packstream.Value, len(s.columns))
		for i, col := range s.columns {
			row[i] = exprToPackstream(rec[col])
		}
		responses = append(responses, &proto.Record{Data: row})
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
			rec := s.result.Record()
			row := make([]packstream.Value, len(s.columns))
			for i, col := range s.columns {
				row[i] = exprToPackstream(rec[col])
			}
			s.peeked = &row
			hasMore = true
		}
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
	}
	responses = append(responses, &proto.Success{Metadata: meta})
	return responses, nil
}

func (s *Session) handleDiscard(m *proto.Discard) ([]any, error) {
	if s.state != StateStreaming && s.state != StateTxStreaming {
		return s.failTransition(m)
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
		if ms, ok := v.(int64); ok && ms > 0 {
			effective = time.Duration(ms) * time.Millisecond
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
	if s.state != StateReady && s.state != StateTxReady {
		return s.failTransition(m)
	}
	rt := RoutingTable(s.localAddr)
	return []any{&proto.Success{Metadata: rt}}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

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
// and returns a FAILURE response for an illegal state transition.
func (s *Session) failTransition(msg any) ([]any, error) {
	s.enterFailed()
	return []any{&proto.Failure{
		Code:    "Neo.ClientError.Request.Invalid",
		Message: fmt.Sprintf("illegal message %T in state %s", msg, s.state),
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
//   - Auth errors → "Authentication failed."
//   - Cypher syntax/sema errors → the error text (already a user-facing message).
//   - All other errors → a generic message with a session ID for log correlation.
func (s *Session) sanitiseErr(err error) string {
	if err == nil {
		return ""
	}
	// Auth errors: never reveal the underlying cause to the client.
	if errors.Is(err, ErrSchemeUnknown) {
		return "Authentication failed."
	}
	// Syntax and semantic errors are already composed as user-facing messages.
	if isCypherUserError(err) {
		return err.Error()
	}
	// Bounded-resource guards (result-row cap, aggregator element budget) carry a
	// message that names the limit and discloses nothing sensitive; forward it so
	// the client learns the query was rejected for exceeding a configured cap
	// rather than seeing the opaque internal-error text.
	if isResourceLimitErr(err) {
		return err.Error()
	}
	// All other errors (internal engine, storage, unexpected): generic + session ID.
	return fmt.Sprintf("An internal error occurred. See server logs for details (session: %s).", s.id)
}

// isCypherUserError reports whether err is a Cypher syntax or semantic error
// that is safe to forward verbatim to the client.
func isCypherUserError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Heuristic: Cypher user errors start with their Bolt error code prefix or
	// contain "SyntaxError" / "SemanticError" in their message. This is safer
	// than a type assertion because the cypher package is not imported here.
	for _, prefix := range []string{
		"Neo.ClientError.Statement.",
		"Neo.ClientError.Schema.",
		"SyntaxError",
		"SemanticError",
		"TypeError",
		"ArgumentError",
	} {
		if len(msg) >= len(prefix) && msg[:len(prefix)] == prefix {
			return true
		}
	}
	return false
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

// exprToPackstream converts an expr.Value (or any interface{} from exec.Record)
// to a packstream.Value suitable for inclusion in a RECORD message.
//
// Handles: nil, expr.Null, expr.IntegerValue, expr.FloatValue, expr.BoolValue,
// expr.StringValue, expr.NodeValue, expr.RelationshipValue, expr.ListValue,
// expr.MapValue. Unknown types are converted to their string representation.
func exprToPackstream(v any) packstream.Value {
	if v == nil {
		return nil
	}

	switch x := v.(type) {
	case expr.Value:
		return exprValueToPackstream(x)
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
//
//nolint:gocyclo,cyclop // dispatch over all expr.Value kinds; complexity is irreducible
func exprValueToPackstream(v expr.Value) packstream.Value {
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
			props[k] = exprValueToPackstream(pv)
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
			props[k] = exprValueToPackstream(pv)
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
			nodes[i] = exprValueToPackstream(n)
		}
		rels := make([]packstream.Value, len(x.Relationships))
		for i, r := range x.Relationships {
			rels[i] = exprValueToPackstream(r)
		}
		return map[string]packstream.Value{
			"nodes":         nodes,
			"relationships": rels,
		}
	case expr.MapValue:
		m := make(map[string]packstream.Value, len(x))
		for k, mv := range x {
			m[k] = exprValueToPackstream(mv)
		}
		return m
	default:
		// expr.Null, expr.nullValue, or any unknown value kind.
		if x == nil || x == expr.Null {
			return nil
		}
		return x.String()
	}
}
