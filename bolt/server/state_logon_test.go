package server

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// logonMsg builds a LOGON proto message with the "none" scheme.
func logonMsg() *proto.Logon {
	return &proto.Logon{
		Auth: map[string]packstream.Value{
			"scheme":      "none",
			"principal":   "user",
			"credentials": "",
		},
	}
}

// TestLogon_WrongStartingStates covers T668 AC2:
// "Wrong starting state yields Defunct or a documented illegal-transition error."
//
// The spec allows Logon only from READY and TX_READY. Every other state
// must produce a *proto.Failure and move the session to FAILED.
func TestLogon_WrongStartingStates(t *testing.T) {
	t.Parallel()

	states := []struct {
		name  string
		setup func(t *testing.T) *Session
	}{
		{
			name: "from_negotiation",
			setup: func(t *testing.T) *Session {
				t.Helper()
				return newSession(newTestEngine(t), NoAuthHandler{}, "")
				// starts in NEGOTIATION — no extra setup needed
			},
		},
		{
			name: "from_streaming",
			setup: func(t *testing.T) *Session {
				t.Helper()
				sess := newReadySession(t)
				if _, err := sess.HandleMessage(context.Background(), &proto.Run{
					Query: "MATCH (n) RETURN n",
					Extra: map[string]interface{}{},
				}); err != nil {
					t.Fatalf("RUN: %v", err)
				}
				if sess.state != StateStreaming {
					t.Fatalf("pre-condition: want STREAMING, got %v", sess.state)
				}
				return sess
			},
		},
		{
			name: "from_tx_streaming",
			setup: func(t *testing.T) *Session {
				t.Helper()
				sess := newReadySession(t)
				if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
					Extra: map[string]interface{}{},
				}); err != nil {
					t.Fatalf("BEGIN: %v", err)
				}
				if _, err := sess.HandleMessage(context.Background(), &proto.Run{
					Query: "MATCH (n) RETURN n",
					Extra: map[string]interface{}{},
				}); err != nil {
					t.Fatalf("RUN in tx: %v", err)
				}
				if sess.state != StateTxStreaming {
					t.Fatalf("pre-condition: want TX_STREAMING, got %v", sess.state)
				}
				return sess
			},
		},
		{
			name: "from_failed",
			setup: func(t *testing.T) *Session {
				t.Helper()
				sess := newSession(newTestEngine(t), NoAuthHandler{}, "")
				sess.state = StateFailed
				return sess
			},
		},
	}

	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess := tc.setup(t)

			msgs, err := sess.HandleMessage(context.Background(), logonMsg())
			if err != nil {
				t.Fatalf("HandleMessage: %v", err)
			}
			if len(msgs) != 1 {
				t.Fatalf("response count: got %d, want 1", len(msgs))
			}
			if _, ok := msgs[0].(*proto.Failure); !ok {
				t.Fatalf("expected *proto.Failure, got %T", msgs[0])
			}
			// All illegal-transition paths land in FAILED per the implementation.
			if sess.state != StateFailed {
				t.Fatalf("state after illegal Logon: got %v, want FAILED", sess.state)
			}
		})
	}
}

// TestLogon_SuccessFromTxReady covers the secondary happy path:
// Logon is also legal from TX_READY and must keep the session in TX_READY.
func TestLogon_SuccessFromTxReady(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if sess.state != StateTxReady {
		t.Fatalf("pre-condition: want TX_READY, got %v", sess.state)
	}

	msgs, err := sess.HandleMessage(context.Background(), logonMsg())
	if err != nil {
		t.Fatalf("Logon in TX_READY: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("response count: got %d, want 1", len(msgs))
	}
	if _, ok := msgs[0].(*proto.Success); !ok {
		t.Fatalf("expected *proto.Success, got %T", msgs[0])
	}
	if sess.state != StateTxReady {
		t.Fatalf("state after Logon in TX_READY: got %v, want TX_READY", sess.state)
	}
}
