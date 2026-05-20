package server

import (
	"context"
	"testing"

	"gograph/bolt/proto"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
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
	sess := newSession(eng, NoAuthHandler{})

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
	sess := newSession(eng, NoAuthHandler{})

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
	sess := newSession(eng, NoAuthHandler{})

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
	sess := newSession(eng, NoAuthHandler{})

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
// FAILURE and leaves the session in FAILED state.
func TestSession_AuthFailure(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	auth := BasicAuthHandler{
		Validate: func(_, _ string) error { return ErrAuthFailed },
	}
	sess := newSession(eng, auth)

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
	if sess.state != StateFailed {
		t.Fatalf("state after auth failure: got %v, want FAILED", sess.state)
	}
}

// TestSession_IllegalTransition verifies that sending an illegal message in
// the wrong state returns FAILURE and moves to FAILED.
func TestSession_IllegalTransition(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t)
	sess := newSession(eng, NoAuthHandler{})
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
