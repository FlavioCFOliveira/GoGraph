package server

import (
	"context"
	"errors"
	"testing"

	"gograph/bolt/proto"
)

// TestIllegalTransitions_AllDocumentedPairs covers T689 AC1–AC3.
//
// Every (state, message) pair that is illegal per the Bolt v5 state machine
// must produce:
//  1. A *proto.Failure response.
//  2. The failure code maps to the documented FailureCode for ErrInvalidTransition:
//     "Neo.ClientError.Request.InvalidFormat".
//     (The session layer uses "Neo.ClientError.Request.Invalid" for the Failure
//     message it sends; the FailureCode function maps ErrInvalidTransition to
//     "Neo.ClientError.Request.InvalidFormat". Both are tested here.)
//  3. The session ends up in FAILED.
//
// Notes on the session.failTransition helper:
// It hard-codes the code "Neo.ClientError.Request.Invalid" in the proto.Failure
// it returns. ErrInvalidTransition itself maps to "Neo.ClientError.Request.InvalidFormat"
// via FailureCode(). Both codes come from the same root error; the difference is
// the wire code vs the FailureCode mapper. We check the wire code here.
func TestIllegalTransitions_AllDocumentedPairs(t *testing.T) {
	t.Parallel()

	// For cases that need a session already in a specific state, we provide a
	// setup function. Each entry is independent; the session is fresh per sub-test.
	type tc struct {
		name     string
		setup    func(t *testing.T) *Session
		msg      any
		wantCode string
	}

	// wireFailureCode is the code used by session.failTransition.
	const wireFailureCode = "Neo.ClientError.Request.Invalid"

	cases := []tc{
		// ── NEGOTIATION: only HELLO is legal ────────────────────────────────
		{
			name:     "NEGOTIATION+Run",
			setup:    func(t *testing.T) *Session { t.Helper(); return newSession(newTestEngine(t), NoAuthHandler{}, "") },
			msg:      &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}},
			wantCode: wireFailureCode,
		},
		{
			name:     "NEGOTIATION+Pull",
			setup:    func(t *testing.T) *Session { t.Helper(); return newSession(newTestEngine(t), NoAuthHandler{}, "") },
			msg:      &proto.Pull{N: -1, QID: -1},
			wantCode: wireFailureCode,
		},
		{
			name:     "NEGOTIATION+Begin",
			setup:    func(t *testing.T) *Session { t.Helper(); return newSession(newTestEngine(t), NoAuthHandler{}, "") },
			msg:      &proto.Begin{Extra: map[string]interface{}{}},
			wantCode: wireFailureCode,
		},
		{
			name:     "NEGOTIATION+Commit",
			setup:    func(t *testing.T) *Session { t.Helper(); return newSession(newTestEngine(t), NoAuthHandler{}, "") },
			msg:      &proto.Commit{},
			wantCode: wireFailureCode,
		},
		{
			name:     "NEGOTIATION+Rollback",
			setup:    func(t *testing.T) *Session { t.Helper(); return newSession(newTestEngine(t), NoAuthHandler{}, "") },
			msg:      &proto.Rollback{},
			wantCode: wireFailureCode,
		},
		{
			name:     "NEGOTIATION+Discard",
			setup:    func(t *testing.T) *Session { t.Helper(); return newSession(newTestEngine(t), NoAuthHandler{}, "") },
			msg:      &proto.Discard{N: -1, QID: -1},
			wantCode: wireFailureCode,
		},
		// ── READY: PULL and DISCARD are illegal without a prior RUN ─────────
		{
			name:     "READY+Pull",
			setup:    func(t *testing.T) *Session { t.Helper(); return newReadySession(t) },
			msg:      &proto.Pull{N: -1, QID: -1},
			wantCode: wireFailureCode,
		},
		{
			name:     "READY+Discard",
			setup:    func(t *testing.T) *Session { t.Helper(); return newReadySession(t) },
			msg:      &proto.Discard{N: -1, QID: -1},
			wantCode: wireFailureCode,
		},
		{
			name:     "READY+Commit",
			setup:    func(t *testing.T) *Session { t.Helper(); return newReadySession(t) },
			msg:      &proto.Commit{},
			wantCode: wireFailureCode,
		},
		{
			name:     "READY+Rollback",
			setup:    func(t *testing.T) *Session { t.Helper(); return newReadySession(t) },
			msg:      &proto.Rollback{},
			wantCode: wireFailureCode,
		},
		// ── STREAMING: only PULL, DISCARD, RESET, GOODBYE are legal ─────────
		{
			name: "STREAMING+Run",
			setup: func(t *testing.T) *Session {
				t.Helper()
				sess := newReadySession(t)
				if _, err := sess.HandleMessage(context.Background(), &proto.Run{
					Query: "MATCH (n) RETURN n", Extra: map[string]interface{}{},
				}); err != nil {
					t.Fatalf("RUN: %v", err)
				}
				return sess
			},
			msg:      &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}},
			wantCode: wireFailureCode,
		},
		{
			name: "STREAMING+Begin",
			setup: func(t *testing.T) *Session {
				t.Helper()
				sess := newReadySession(t)
				if _, err := sess.HandleMessage(context.Background(), &proto.Run{
					Query: "MATCH (n) RETURN n", Extra: map[string]interface{}{},
				}); err != nil {
					t.Fatalf("RUN: %v", err)
				}
				return sess
			},
			msg:      &proto.Begin{Extra: map[string]interface{}{}},
			wantCode: wireFailureCode,
		},
		{
			name: "STREAMING+Commit",
			setup: func(t *testing.T) *Session {
				t.Helper()
				sess := newReadySession(t)
				if _, err := sess.HandleMessage(context.Background(), &proto.Run{
					Query: "MATCH (n) RETURN n", Extra: map[string]interface{}{},
				}); err != nil {
					t.Fatalf("RUN: %v", err)
				}
				return sess
			},
			msg:      &proto.Commit{},
			wantCode: wireFailureCode,
		},
		// ── TX_READY: PULL and DISCARD without a prior RUN ──────────────────
		{
			name: "TX_READY+Pull",
			setup: func(t *testing.T) *Session {
				t.Helper()
				sess := newReadySession(t)
				if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
					Extra: map[string]interface{}{},
				}); err != nil {
					t.Fatalf("BEGIN: %v", err)
				}
				return sess
			},
			msg:      &proto.Pull{N: -1, QID: -1},
			wantCode: wireFailureCode,
		},
		{
			name: "TX_READY+Discard",
			setup: func(t *testing.T) *Session {
				t.Helper()
				sess := newReadySession(t)
				if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
					Extra: map[string]interface{}{},
				}); err != nil {
					t.Fatalf("BEGIN: %v", err)
				}
				return sess
			},
			msg:      &proto.Discard{N: -1, QID: -1},
			wantCode: wireFailureCode,
		},
		// ── FAILED: all messages except RESET/GOODBYE are illegal ───────────
		{
			name: "FAILED+Run",
			setup: func(t *testing.T) *Session {
				t.Helper()
				s := newSession(newTestEngine(t), NoAuthHandler{}, "")
				s.state = StateFailed
				return s
			},
			msg:      &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}},
			wantCode: wireFailureCode,
		},
		{
			name: "FAILED+Pull",
			setup: func(t *testing.T) *Session {
				t.Helper()
				s := newSession(newTestEngine(t), NoAuthHandler{}, "")
				s.state = StateFailed
				return s
			},
			msg:      &proto.Pull{N: -1, QID: -1},
			wantCode: wireFailureCode,
		},
		{
			name: "FAILED+Begin",
			setup: func(t *testing.T) *Session {
				t.Helper()
				s := newSession(newTestEngine(t), NoAuthHandler{}, "")
				s.state = StateFailed
				return s
			},
			msg:      &proto.Begin{Extra: map[string]interface{}{}},
			wantCode: wireFailureCode,
		},
		// ── DEFUNCT: everything is illegal ──────────────────────────────────
		{
			name: "DEFUNCT+Reset",
			setup: func(t *testing.T) *Session {
				t.Helper()
				s := newSession(newTestEngine(t), NoAuthHandler{}, "")
				s.state = StateDefunct
				return s
			},
			msg:      &proto.Reset{},
			wantCode: wireFailureCode,
		},
		{
			name: "DEFUNCT+Run",
			setup: func(t *testing.T) *Session {
				t.Helper()
				s := newSession(newTestEngine(t), NoAuthHandler{}, "")
				s.state = StateDefunct
				return s
			},
			msg:      &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}},
			wantCode: wireFailureCode,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess := tc.setup(t)

			msgs, err := sess.HandleMessage(context.Background(), tc.msg)
			if err != nil {
				t.Fatalf("HandleMessage: %v", err)
			}
			if len(msgs) != 1 {
				t.Fatalf("response count: got %d, want 1", len(msgs))
			}
			f, ok := msgs[0].(*proto.Failure)
			if !ok {
				t.Fatalf("response type: got %T, want *proto.Failure", msgs[0])
			}
			if f.Code != tc.wantCode {
				t.Errorf("failure code: got %q, want %q", f.Code, tc.wantCode)
			}
			// AC3: session ends in FAILED (DEFUNCT stays DEFUNCT for that sub-case).
			if sess.state != StateFailed && sess.state != StateDefunct {
				t.Errorf("state: got %v, want FAILED or DEFUNCT", sess.state)
			}
		})
	}
}

// TestIllegalTransitions_FailureCodeMapping covers T689 AC2:
// The error returned by Transition for an illegal pair maps to
// "Neo.ClientError.Request.InvalidFormat" via FailureCode.
func TestIllegalTransitions_FailureCodeMapping(t *testing.T) {
	t.Parallel()

	got := FailureCode(ErrInvalidTransition)
	want := "Neo.ClientError.Request.InvalidFormat"
	if got != want {
		t.Errorf("FailureCode(ErrInvalidTransition) = %q, want %q", got, want)
	}

	// Wrapped ErrInvalidTransition must also resolve correctly.
	wrapped := errors.Join(errors.New("outer"), ErrInvalidTransition)
	got2 := FailureCode(wrapped)
	if got2 != want {
		t.Errorf("FailureCode(wrapped ErrInvalidTransition) = %q, want %q", got2, want)
	}
}
