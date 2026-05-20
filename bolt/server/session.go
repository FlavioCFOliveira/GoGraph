package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gograph/bolt/packstream"
	"gograph/bolt/proto"
	"gograph/cypher"
	"gograph/cypher/expr"
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

	// txActive indicates that an explicit transaction is open (BEGIN called).
	// For this sprint only auto-commit is implemented; explicit TX support is
	// task 312. This field is reserved for that extension.
	txActive bool

	// stmtTimeout is extracted from RUN extra metadata ("timeout" key, ms).
	stmtTimeout time.Duration

	// bookmark holds the last committed transaction bookmark (server-generated
	// placeholder for this sprint).
	bookmark string
}

// newSession constructs an idle Session backed by eng, starting in
// StateNegotiation (version negotiation has already succeeded by the time
// newSession is called).
func newSession(eng *cypher.Engine, auth AuthHandler) *Session {
	return &Session{
		id:    randomID(),
		eng:   eng,
		auth:  auth,
		state: StateNegotiation,
	}
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
	// Propagate context cancellation before doing any work.
	if err := ctx.Err(); err != nil {
		return s.failWith("Neo.TransientError.General.RequestInterrupted", err.Error()), nil
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
		return s.handleBegin(m)
	case *proto.Commit:
		return s.handleCommit()
	case *proto.Rollback:
		return s.handleRollback()
	default:
		return s.failWith("Neo.ClientError.Request.Invalid",
			fmt.Sprintf("unrecognised message type %T", msg)), nil
	}
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
		s.state = StateFailed
		code := authErrorCode(err)
		return []any{&proto.Failure{Code: code, Message: err.Error()}}, nil
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
		s.state = StateFailed
		return []any{&proto.Failure{Code: authErrorCode(err), Message: err.Error()}}, nil
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
	s.state = StateDefunct
	// No response is sent for GOODBYE.
	return nil, nil
}

func (s *Session) handleRun(ctx context.Context, m *proto.Run) ([]any, error) {
	if s.state != StateReady && s.state != StateTxReady {
		return s.failTransition(m)
	}

	// Extract optional statement timeout from extra metadata.
	if v, ok := m.Extra["timeout"]; ok {
		if ms, ok := v.(int64); ok && ms > 0 {
			s.stmtTimeout = time.Duration(ms) * time.Millisecond
		}
	}

	// Build execution context with optional deadline.
	runCtx := ctx
	if s.stmtTimeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, s.stmtTimeout)
		defer cancel()
	}

	// Convert proto params to map[string]any for RunAny.
	params := make(map[string]any, len(m.Parameters))
	for k, v := range m.Parameters {
		params[k] = v
	}

	var (
		result *cypher.Result
		runErr error
	)
	if s.state == StateTxReady {
		result, runErr = s.eng.RunInTxAny(runCtx, m.Query, params)
	} else {
		result, runErr = s.eng.RunAny(runCtx, m.Query, params)
	}

	next, transErr := Transition(s.state, m, runErr == nil)
	if transErr != nil {
		if result != nil {
			_ = result.Close() //nolint:errcheck // best-effort close on unexpected path
		}
		return s.failTransition(m)
	}
	s.state = next

	if runErr != nil {
		return []any{&proto.Failure{
			Code:    "Neo.DatabaseError.General.UnknownError",
			Message: runErr.Error(),
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

//nolint:gocyclo // pull loop has context cancellation, cursor error, has_more, and state transition branches; complexity is irreducible.
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

	for n <= 0 || fetched < n {
		if ctx.Err() != nil {
			s.drainResult()
			s.state = StateFailed
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
		s.drainResult()
		s.state = StateFailed
		return []any{&proto.Failure{
			Code:    "Neo.DatabaseError.General.UnknownError",
			Message: err.Error(),
		}}, nil
	}

	// Determine has_more: true only when n > 0 and we hit the n-row limit.
	// If n ≤ 0 (pull-all) or we fetched fewer rows than requested, the cursor
	// is exhausted.
	hasMore := n > 0 && fetched == n && fetched > 0

	// Transition state based on has_more.
	next, transErr := StreamingTransition(s.state, hasMore)
	if transErr != nil {
		return s.failTransition(m)
	}
	if !hasMore {
		s.drainResult() // close and nil the cursor
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

func (s *Session) handleBegin(m *proto.Begin) ([]any, error) {
	if s.state != StateReady {
		return s.failTransition(m)
	}
	next, err := Transition(s.state, m, true)
	if err != nil {
		return s.failTransition(m)
	}
	s.state = next
	s.txActive = true
	return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
}

func (s *Session) handleCommit() ([]any, error) {
	m := &proto.Commit{}
	if s.state != StateTxReady {
		return s.failTransition(m)
	}
	next, err := Transition(s.state, m, true)
	if err != nil {
		return s.failTransition(m)
	}
	s.state = next
	s.txActive = false
	return []any{&proto.Success{Metadata: map[string]packstream.Value{
		"bookmark": s.bookmark,
	}}}, nil
}

func (s *Session) handleRollback() ([]any, error) {
	m := &proto.Rollback{}
	if s.state != StateTxReady {
		return s.failTransition(m)
	}
	next, err := Transition(s.state, m, true)
	if err != nil {
		return s.failTransition(m)
	}
	s.state = next
	s.txActive = false
	return []any{&proto.Success{Metadata: map[string]packstream.Value{}}}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// drainResult closes and nils the current result cursor if one is open.
func (s *Session) drainResult() {
	if s.result != nil {
		_ = s.result.Close() //nolint:errcheck // best-effort drain; error is not actionable here
		s.result = nil
		s.columns = nil
	}
}

// failTransition moves the session to FAILED and returns a FAILURE response
// for an illegal state transition.
func (s *Session) failTransition(msg any) ([]any, error) {
	s.state = StateFailed
	return []any{&proto.Failure{
		Code:    "Neo.ClientError.Request.Invalid",
		Message: fmt.Sprintf("illegal message %T in state %s", msg, s.state),
	}}, nil
}

// failWith moves the session to FAILED and returns a FAILURE response.
func (s *Session) failWith(code, message string) []any {
	s.state = StateFailed
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
