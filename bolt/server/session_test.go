package server

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newTestEngine creates a minimal, populated Cypher engine for use in
// session tests.
func newTestEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g)
}

// helloMsg returns a HELLO proto message with no-auth credentials.
func helloMsg() *proto.Hello {
	return &proto.Hello{
		Extra: map[string]interface{}{
			"scheme":      "none",
			"principal":   "user",
			"credentials": "",
			"agent":       "test/1.0",
		},
	}
}

// TestSession_HelloReady verifies that HELLO in NEGOTIATION state transitions
// the session to READY and returns a SUCCESS with server metadata.
func TestSession_HelloReady(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	sess := newSession(eng, NoAuthHandler{}, "")

	if sess.state != StateNegotiation {
		t.Fatalf("initial state: got %v, want NEGOTIATION", sess.state)
	}

	msgs, err := sess.HandleMessage(context.Background(), helloMsg())
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("response count: got %d, want 1", len(msgs))
	}
	s, ok := msgs[0].(*proto.Success)
	if !ok {
		t.Fatalf("response type: got %T, want *proto.Success", msgs[0])
	}
	if s.Metadata == nil {
		t.Fatal("SUCCESS metadata is nil")
	}
	if _, ok := s.Metadata["server"]; !ok {
		t.Error("SUCCESS metadata missing 'server' field")
	}
	if sess.state != StateReady {
		t.Fatalf("state after HELLO: got %v, want READY", sess.state)
	}
}

// TestSession_RunPullReady verifies the Run→Pull→Ready cycle with a real
// engine that returns zero or more rows.
func TestSession_RunPullReady(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	sess := newSession(eng, NoAuthHandler{}, "")

	// HELLO → READY
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if sess.state != StateReady {
		t.Fatalf("state after HELLO: %v", sess.state)
	}

	// RUN — use MATCH which the engine supports on an empty graph (returns 0 rows).
	run := &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}
	runMsgs, err := sess.HandleMessage(context.Background(), run)
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if len(runMsgs) != 1 {
		t.Fatalf("RUN response count: %d", len(runMsgs))
	}
	runSuccess, ok := runMsgs[0].(*proto.Success)
	if !ok {
		t.Fatalf("RUN response type: %T", runMsgs[0])
	}
	fields, ok := runSuccess.Metadata["fields"]
	if !ok {
		t.Fatal("RUN SUCCESS missing 'fields'")
	}
	_ = fields

	if sess.state != StateStreaming {
		t.Fatalf("state after RUN: got %v, want STREAMING", sess.state)
	}

	// PULL all (-1)
	pull := &proto.Pull{N: -1, QID: -1}
	pullMsgs, err := sess.HandleMessage(context.Background(), pull)
	if err != nil {
		t.Fatalf("PULL: %v", err)
	}
	if len(pullMsgs) < 1 {
		t.Fatal("PULL returned no messages")
	}
	// Last message must be SUCCESS.
	last := pullMsgs[len(pullMsgs)-1]
	if _, ok := last.(*proto.Success); !ok {
		t.Fatalf("last PULL message: got %T, want *proto.Success", last)
	}

	if sess.state != StateReady {
		t.Fatalf("state after PULL all: got %v, want READY", sess.state)
	}
}

// TestSession_ResetDrainsStreaming verifies that RESET from STREAMING state
// drains the pending cursor and returns to READY.
func TestSession_ResetDrainsStreaming(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	sess := newSession(eng, NoAuthHandler{}, "")

	// HELLO → READY
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}

	// RUN → STREAMING — MATCH on empty graph: zero rows, cursor still open.
	run := &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}
	if _, err := sess.HandleMessage(context.Background(), run); err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if sess.state != StateStreaming {
		t.Fatalf("state after RUN: %v", sess.state)
	}

	// RESET — must drain cursor and return to READY
	resetMsgs, err := sess.HandleMessage(context.Background(), &proto.Reset{})
	if err != nil {
		t.Fatalf("RESET: %v", err)
	}
	if len(resetMsgs) != 1 {
		t.Fatalf("RESET response count: %d", len(resetMsgs))
	}
	if _, ok := resetMsgs[0].(*proto.Success); !ok {
		t.Fatalf("RESET response: %T", resetMsgs[0])
	}
	if sess.state != StateReady {
		t.Fatalf("state after RESET: got %v, want READY", sess.state)
	}
	if sess.result != nil {
		t.Fatal("result cursor not nil after RESET")
	}
}

// TestSession_StatementTimeout verifies that a statement timeout context
// deadline is respected. We simulate this by passing a context that is
// already cancelled.
func TestSession_StatementTimeout(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	sess := newSession(eng, NoAuthHandler{}, "")

	// HELLO → READY
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}

	// Use an already-cancelled context to simulate timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// RUN with a cancelled context — should return FAILURE.
	run := &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}
	msgs, err := sess.HandleMessage(ctx, run)
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("response count: %d", len(msgs))
	}
	// With a cancelled context, HandleMessage should return a FAILURE.
	if _, ok := msgs[0].(*proto.Failure); !ok {
		t.Fatalf("expected *proto.Failure, got %T", msgs[0])
	}
}

// TestSession_AuthFailure verifies that a HELLO with wrong credentials returns
// FAILURE and terminates the connection (DEFUNCT), so a credential-less client
// cannot reuse it (task #1345).
func TestSession_AuthFailure(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	auth := BasicAuthHandler{
		Validate: func(_, _ string) error { return ErrAuthFailed },
	}
	sess := newSession(eng, auth, "")

	msgs, err := sess.HandleMessage(context.Background(), helloMsg())
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("response count: %d", len(msgs))
	}
	if _, ok := msgs[0].(*proto.Failure); !ok {
		t.Fatalf("expected *proto.Failure, got %T", msgs[0])
	}
	if sess.state != StateDefunct {
		t.Fatalf("state after auth failure: got %v, want DEFUNCT", sess.state)
	}
}

// TestSession_IllegalTransition verifies that sending an illegal message in
// the wrong state returns FAILURE and moves to FAILED.
func TestSession_IllegalTransition(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	sess := newSession(eng, NoAuthHandler{}, "")
	// State is NEGOTIATION; sending PULL is illegal.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("response count: %d", len(msgs))
	}
	if _, ok := msgs[0].(*proto.Failure); !ok {
		t.Fatalf("expected *proto.Failure, got %T", msgs[0])
	}
	if sess.state != StateFailed {
		t.Fatalf("state after illegal message: got %v, want FAILED", sess.state)
	}
}

// newReadySession is a test helper that creates a Session in READY state by
// sending a HELLO message.
func newReadySession(t *testing.T) *Session {
	t.Helper()
	sess := newSession(newTestEngine(t), NoAuthHandler{}, "")
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if sess.state != StateReady {
		t.Fatalf("state after HELLO: got %v, want READY", sess.state)
	}
	return sess
}

// ─────────────────────────────────────────────────────────────────────────────
// Task 311: RUN / PULL / DISCARD streaming tests
// ─────────────────────────────────────────────────────────────────────────────

// TestSession_Run_Pull_All verifies a full RUN → PULL(-1) cycle on a non-empty
// graph, checking that all RECORD messages are emitted and the final SUCCESS
// has has_more=false.
func TestSession_Run_Pull_All(t *testing.T) {
	t.Parallel()

	// Build a graph with two nodes so MATCH (n) RETURN n yields 2 rows.
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	eng := cypher.NewEngine(g)
	sess := newSession(eng, NoAuthHandler{}, "")

	// HELLO → READY.
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}

	// RUN.
	runMsgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if len(runMsgs) != 1 {
		t.Fatalf("RUN response count: %d", len(runMsgs))
	}
	runSuccess, ok := runMsgs[0].(*proto.Success)
	if !ok {
		t.Fatalf("RUN response type: %T", runMsgs[0])
	}
	fields, ok := runSuccess.Metadata["fields"]
	if !ok {
		t.Fatal("RUN SUCCESS missing 'fields'")
	}
	fieldList, ok := fields.([]packstream.Value)
	if !ok {
		t.Fatalf("RUN SUCCESS 'fields' type: %T", fields)
	}
	if len(fieldList) == 0 {
		t.Fatal("RUN SUCCESS 'fields' is empty")
	}

	// PULL all.
	pullMsgs, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("PULL: %v", err)
	}
	if len(pullMsgs) < 1 {
		t.Fatal("PULL returned no messages")
	}

	// Count RECORDs.
	var records int
	for _, msg := range pullMsgs[:len(pullMsgs)-1] {
		if _, ok := msg.(*proto.Record); ok {
			records++
		}
	}
	if records != 2 {
		t.Fatalf("expected 2 RECORD messages, got %d", records)
	}

	// Last message must be SUCCESS with has_more=false.
	last, ok := pullMsgs[len(pullMsgs)-1].(*proto.Success)
	if !ok {
		t.Fatalf("last PULL message: got %T, want *proto.Success", pullMsgs[len(pullMsgs)-1])
	}
	hasMore, ok := last.Metadata["has_more"]
	if !ok {
		t.Fatal("PULL SUCCESS missing 'has_more'")
	}
	if hasMore != false {
		t.Fatalf("has_more: got %v, want false", hasMore)
	}

	if sess.state != StateReady {
		t.Fatalf("state after PULL all: got %v, want READY", sess.state)
	}
}

// TestSession_Pull_Paginated verifies that PULL with n=1 on a 2-row result
// produces has_more=true on the first pull and has_more=false on the second.
func TestSession_Pull_Paginated(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	eng := cypher.NewEngine(g)
	sess := newSession(eng, NoAuthHandler{}, "")

	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}

	// RUN.
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}); err != nil {
		t.Fatalf("RUN: %v", err)
	}

	// First PULL: n=1 → should yield 1 RECORD and has_more=true.
	msgs1, err := sess.HandleMessage(context.Background(), &proto.Pull{N: 1, QID: -1})
	if err != nil {
		t.Fatalf("PULL 1: %v", err)
	}
	last1, ok := msgs1[len(msgs1)-1].(*proto.Success)
	if !ok {
		t.Fatalf("first PULL last message: %T", msgs1[len(msgs1)-1])
	}
	hasMore1, ok := last1.Metadata["has_more"]
	if !ok {
		t.Fatal("first PULL SUCCESS missing 'has_more'")
	}
	if hasMore1 != true {
		t.Fatalf("first PULL has_more: got %v, want true", hasMore1)
	}
	if sess.state != StateStreaming {
		t.Fatalf("state after partial PULL: got %v, want STREAMING", sess.state)
	}

	// Second PULL: n=1 → last row, has_more=false.
	msgs2, err := sess.HandleMessage(context.Background(), &proto.Pull{N: 1, QID: -1})
	if err != nil {
		t.Fatalf("PULL 2: %v", err)
	}
	last2, ok := msgs2[len(msgs2)-1].(*proto.Success)
	if !ok {
		t.Fatalf("second PULL last message: %T", msgs2[len(msgs2)-1])
	}
	hasMore2, ok := last2.Metadata["has_more"]
	if !ok {
		t.Fatal("second PULL SUCCESS missing 'has_more'")
	}
	if hasMore2 != false {
		t.Fatalf("second PULL has_more: got %v, want false", hasMore2)
	}
	if sess.state != StateReady {
		t.Fatalf("state after final PULL: got %v, want READY", sess.state)
	}
}

// TestSession_Discard verifies that DISCARD produces no RECORD messages and
// returns SUCCESS with has_more=false.
func TestSession_Discard(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	eng := cypher.NewEngine(g)
	sess := newSession(eng, NoAuthHandler{}, "")

	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}

	// RUN.
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}); err != nil {
		t.Fatalf("RUN: %v", err)
	}

	// DISCARD.
	discardMsgs, err := sess.HandleMessage(context.Background(), &proto.Discard{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("DISCARD: %v", err)
	}

	// Must have exactly one message (SUCCESS) and no RECORDs.
	for _, msg := range discardMsgs {
		if _, ok := msg.(*proto.Record); ok {
			t.Fatal("DISCARD emitted a RECORD message")
		}
	}
	if len(discardMsgs) != 1 {
		t.Fatalf("DISCARD response count: %d, want 1", len(discardMsgs))
	}
	success, ok := discardMsgs[0].(*proto.Success)
	if !ok {
		t.Fatalf("DISCARD response: %T, want *proto.Success", discardMsgs[0])
	}
	hasMore, ok := success.Metadata["has_more"]
	if !ok {
		t.Fatal("DISCARD SUCCESS missing 'has_more'")
	}
	if hasMore != false {
		t.Fatalf("DISCARD has_more: got %v, want false", hasMore)
	}
	if sess.state != StateReady {
		t.Fatalf("state after DISCARD: got %v, want READY", sess.state)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleLogon / handleLogoff
// ─────────────────────────────────────────────────────────────────────────────

func TestSession_Logon_Success(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	logon := &proto.Logon{
		Auth: map[string]packstream.Value{
			"scheme":      "none",
			"principal":   "user",
			"credentials": "",
		},
	}
	msgs, err := sess.HandleMessage(context.Background(), logon)
	if err != nil {
		t.Fatalf("Logon: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Logon response count: %d", len(msgs))
	}
	if _, ok := msgs[0].(*proto.Success); !ok {
		t.Fatalf("Logon response: %T, want *proto.Success", msgs[0])
	}
	if sess.state != StateReady {
		t.Fatalf("state after Logon: %v, want READY", sess.state)
	}
}

func TestSession_Logon_WrongState(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	sess := newSession(eng, NoAuthHandler{}, "")
	// NEGOTIATION state — LOGON is illegal here.
	logon := &proto.Logon{
		Auth: map[string]packstream.Value{"scheme": "none"},
	}
	msgs, err := sess.HandleMessage(context.Background(), logon)
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if _, ok := msgs[0].(*proto.Failure); !ok {
		t.Fatalf("expected FAILURE, got %T", msgs[0])
	}
	if sess.state != StateFailed {
		t.Fatalf("state: got %v, want FAILED", sess.state)
	}
}

func TestSession_Logoff_Success(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	msgs, err := sess.HandleMessage(context.Background(), &proto.Logoff{})
	if err != nil {
		t.Fatalf("Logoff: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Logoff response count: %d", len(msgs))
	}
	if _, ok := msgs[0].(*proto.Success); !ok {
		t.Fatalf("Logoff response: %T, want *proto.Success", msgs[0])
	}
	if sess.state != StateReady {
		t.Fatalf("state after Logoff: %v, want READY", sess.state)
	}
}

func TestSession_Logoff_WrongState(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	sess := newSession(eng, NoAuthHandler{}, "")
	// NEGOTIATION — LOGOFF is illegal.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Logoff{})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if _, ok := msgs[0].(*proto.Failure); !ok {
		t.Fatalf("expected FAILURE, got %T", msgs[0])
	}
	if sess.state != StateFailed {
		t.Fatalf("state: got %v, want FAILED", sess.state)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleGoodbye with active transaction
// ─────────────────────────────────────────────────────────────────────────────

func TestSession_GoodbyeWithActiveTx(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	// BEGIN to open a transaction.
	if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if sess.state != StateTxReady {
		t.Fatalf("state after BEGIN: %v", sess.state)
	}

	// GOODBYE — must rollback the tx and move to DEFUNCT.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Goodbye{})
	if err != nil {
		t.Fatalf("GOODBYE: %v", err)
	}
	if msgs != nil {
		t.Fatalf("GOODBYE should return nil, got %v", msgs)
	}
	if sess.state != StateDefunct {
		t.Fatalf("state after GOODBYE: %v, want DEFUNCT", sess.state)
	}
	if sess.tx != nil {
		t.Fatal("tx should be nil after GOODBYE")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleReset from DEFUNCT state
// ─────────────────────────────────────────────────────────────────────────────

func TestSession_ResetFromDefunct(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	sess := newSession(eng, NoAuthHandler{}, "")
	sess.state = StateDefunct

	msgs, err := sess.HandleMessage(context.Background(), &proto.Reset{})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if _, ok := msgs[0].(*proto.Failure); !ok {
		t.Fatalf("expected FAILURE, got %T", msgs[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleRun from illegal state
// ─────────────────────────────────────────────────────────────────────────────

func TestSession_RunFromStreaming(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	// RUN → STREAMING
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query: "MATCH (n) RETURN n",
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("first RUN: %v", err)
	}
	if sess.state != StateStreaming {
		t.Fatalf("state after RUN: %v", sess.state)
	}

	// Second RUN from STREAMING — illegal.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query: "MATCH (n) RETURN n",
		Extra: map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("second RUN: %v", err)
	}
	if _, ok := msgs[0].(*proto.Failure); !ok {
		t.Fatalf("expected FAILURE, got %T", msgs[0])
	}
	if sess.state != StateFailed {
		t.Fatalf("state: got %v, want FAILED", sess.state)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleRun with statement timeout metadata
// ─────────────────────────────────────────────────────────────────────────────

func TestSession_RunWithTimeout(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	// Set a 10-second timeout in RUN Extra metadata.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query: "MATCH (n) RETURN n",
		Extra: map[string]interface{}{"timeout": int64(10000)},
	})
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("RUN response count: %d", len(msgs))
	}
	if _, ok := msgs[0].(*proto.Success); !ok {
		t.Fatalf("expected SUCCESS, got %T", msgs[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleBegin from non-READY state
// ─────────────────────────────────────────────────────────────────────────────

func TestSession_BeginFromStreaming(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	// RUN → STREAMING
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query: "MATCH (n) RETURN n",
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("RUN: %v", err)
	}

	// BEGIN from STREAMING — illegal.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, ok := msgs[0].(*proto.Failure); !ok {
		t.Fatalf("expected FAILURE, got %T", msgs[0])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// authErrorCode
// ─────────────────────────────────────────────────────────────────────────────

func TestAuthErrorCode_SchemeUnknown(t *testing.T) {
	code := authErrorCode(ErrSchemeUnknown)
	if code != "Neo.ClientError.Security.AuthProviderFailed" {
		t.Errorf("got %q, want Neo.ClientError.Security.AuthProviderFailed", code)
	}
}

func TestAuthErrorCode_Default(t *testing.T) {
	code := authErrorCode(errors.New("some auth error"))
	if code != "Neo.ClientError.Security.Unauthorized" {
		t.Errorf("got %q, want Neo.ClientError.Security.Unauthorized", code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// exprToPackstream and exprValueToPackstream
// ─────────────────────────────────────────────────────────────────────────────

func TestExprToPackstream_Primitives(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want packstream.Value
	}{
		{"nil", nil, nil},
		{"int64", int64(42), int64(42)},
		{"float64", float64(3.14), float64(3.14)},
		{"bool", true, true},
		{"string", "hello", "hello"},
		{"default", struct{}{}, "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := exprToPackstream(tc.in, 5)
			if tc.name == "default" {
				// Default case converts to fmt.Sprintf("%v", v).
				if _, ok := got.(string); !ok {
					t.Fatalf("expected string, got %T", got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestExprValueToPackstream_NodeValue(t *testing.T) {
	nv := expr.NodeValue{
		ID:     42,
		Labels: []string{"Person"},
		Properties: expr.MapValue{
			"name": expr.StringValue("Alice"),
		},
	}
	n := decodeNode(t, exprValueToPackstream(nv, 5))
	if n.ID != 42 {
		t.Errorf("id: got %v, want 42", n.ID)
	}
	if len(n.Labels) != 1 || n.Labels[0] != "Person" {
		t.Errorf("labels: got %v, want [Person]", n.Labels)
	}
	if n.Props["name"] != "Alice" {
		t.Errorf("properties[name]: got %v, want Alice", n.Props["name"])
	}
	if n.ElementID != "42" {
		t.Errorf("element_id: got %q, want \"42\" (the durable id in decimal, as elementId() returns)", n.ElementID)
	}
}

func TestExprValueToPackstream_RelationshipValue(t *testing.T) {
	rv := expr.RelationshipValue{
		ID:      10,
		StartID: 1,
		EndID:   2,
		Type:    "KNOWS",
		Properties: expr.MapValue{
			"since": expr.IntegerValue(2020),
		},
	}
	r := decodeRel(t, exprValueToPackstream(rv, 5))
	if r.ID != 10 {
		t.Errorf("id: got %v, want 10", r.ID)
	}
	if r.Type != "KNOWS" {
		t.Errorf("type: got %v, want KNOWS", r.Type)
	}
	if r.StartID != 1 || r.EndID != 2 {
		t.Errorf("endpoints: got (%d, %d), want (1, 2)", r.StartID, r.EndID)
	}
	if r.Props["since"] != int64(2020) {
		t.Errorf("properties[since]: got %v, want 2020", r.Props["since"])
	}
	if r.ElementID != "10" || r.StartEID != "1" || r.EndEID != "2" {
		t.Errorf("element ids: got (%q, %q, %q), want (\"10\", \"1\", \"2\")",
			r.ElementID, r.StartEID, r.EndEID)
	}
}

func TestExprValueToPackstream_PathValue(t *testing.T) {
	n1 := expr.NodeValue{ID: 1, Labels: []string{"A"}, Properties: expr.MapValue{}}
	n2 := expr.NodeValue{ID: 2, Labels: []string{"B"}, Properties: expr.MapValue{}}
	r1 := expr.RelationshipValue{ID: 5, StartID: 1, EndID: 2, Type: "REL", Properties: expr.MapValue{}}
	pv := expr.PathValue{
		Nodes:         []expr.NodeValue{n1, n2},
		Relationships: []expr.RelationshipValue{r1},
	}
	p := decodePath(t, exprValueToPackstream(pv, 5))
	if len(p.Nodes) != 2 {
		t.Fatalf("nodes: got %d, want 2", len(p.Nodes))
	}
	if len(p.Rels) != 1 {
		t.Fatalf("relationships: got %d, want 1", len(p.Rels))
	}
	// A path carries UNBOUND relationships: the endpoints come from the indices.
	id, typ, _, eid := decodeUnboundRel(t, p.Rels[0])
	if id != 5 || typ != "REL" || eid != "5" {
		t.Errorf("unbound relationship: got (%d, %q, %q), want (5, \"REL\", \"5\")", id, typ, eid)
	}
	// One hop, traversed forward (r1 starts at n1), so the pair is (+1, 1).
	if len(p.Indices) != 2 {
		t.Fatalf("indices: got %v, want one (relationship, node) pair", p.Indices)
	}
	if p.Indices[0] != int64(1) {
		t.Errorf("relationship index: got %v, want +1 (one-based, forward)", p.Indices[0])
	}
	if p.Indices[1] != int64(1) {
		t.Errorf("node index: got %v, want 1 (zero-based, the hop's end node)", p.Indices[1])
	}
}

// TestExprValueToPackstream_PathValue_ReverseHop pins the sign rule that makes a
// reverse-traversed hop round-trip with its endpoints the right way round: the
// relationship index is NEGATIVE when the relationship does not start at the hop's
// source node, and the driver then resolves it as relNodes[(-index)-1].
func TestExprValueToPackstream_PathValue_ReverseHop(t *testing.T) {
	n1 := expr.NodeValue{ID: 1, Labels: []string{"A"}, Properties: expr.MapValue{}}
	n2 := expr.NodeValue{ID: 2, Labels: []string{"B"}, Properties: expr.MapValue{}}
	// The relationship runs 2 -> 1, but the path walks n1 -> n2, so the hop is REVERSE.
	rev := expr.RelationshipValue{ID: 5, StartID: 2, EndID: 1, Type: "REL", Properties: expr.MapValue{}}
	pv := expr.PathValue{Nodes: []expr.NodeValue{n1, n2}, Relationships: []expr.RelationshipValue{rev}}

	p := decodePath(t, exprValueToPackstream(pv, 5))
	if len(p.Indices) != 2 {
		t.Fatalf("indices: got %v, want one pair", p.Indices)
	}
	if p.Indices[0] != int64(-1) {
		t.Errorf("relationship index: got %v, want -1 (reverse traversal)", p.Indices[0])
	}
	if p.Indices[1] != int64(1) {
		t.Errorf("node index: got %v, want 1", p.Indices[1])
	}
}

// TestExprValueToPackstream_PathValue_SingleNode pins the zero-length path: openCypher
// produces a single disconnected node, and the driver's buildPath treats an EMPTY index
// list as exactly that rather than as malformed.
func TestExprValueToPackstream_PathValue_SingleNode(t *testing.T) {
	only := expr.NodeValue{ID: 7, Labels: []string{"A"}, Properties: expr.MapValue{}}
	pv := expr.PathValue{Nodes: []expr.NodeValue{only}}

	p := decodePath(t, exprValueToPackstream(pv, 5))
	if len(p.Nodes) != 1 {
		t.Fatalf("nodes: got %d, want 1", len(p.Nodes))
	}
	if len(p.Rels) != 0 {
		t.Fatalf("relationships: got %d, want 0", len(p.Rels))
	}
	if len(p.Indices) != 0 {
		t.Fatalf("indices: got %v, want empty", p.Indices)
	}
}

func TestExprValueToPackstream_MapValue(t *testing.T) {
	mv := expr.MapValue{
		"x": expr.IntegerValue(99),
		"y": expr.StringValue("z"),
	}
	got := exprValueToPackstream(mv, 5)
	m, ok := got.(map[string]packstream.Value)
	if !ok {
		t.Fatalf("expected map, got %T", got)
	}
	if len(m) != 2 {
		t.Errorf("map len: got %d, want 2", len(m))
	}
}

func TestExprValueToPackstream_Null(t *testing.T) {
	// Passing the typed nil expr.Null value should return nil.
	got := exprValueToPackstream(nil, 5)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}
