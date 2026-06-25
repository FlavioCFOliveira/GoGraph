package server

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestFailedState_IgnoresUntilReset is the #1781 gate: per the Bolt v5 spec,
// once a connection is FAILED, every message except RESET/GOODBYE must be
// answered with IGNORED (not executed, not re-failed) until the client RESETs.
// Before the fix the server returned a fresh FAILURE for each, which could
// overwrite the actionable error on a pipelined stream and (for a stricter
// driver) read as a protocol violation.
func TestFailedState_IgnoresUntilReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := newReadySession(t)

	// Enter FAILED: PULL is illegal in READY.
	if _, err := sess.HandleMessage(ctx, &proto.Pull{N: -1, QID: -1}); err != nil {
		t.Fatalf("HandleMessage(PULL): %v", err)
	}
	if sess.state != StateFailed {
		t.Fatalf("state after illegal PULL = %v, want FAILED", sess.state)
	}

	// Every non-RESET/GOODBYE message in FAILED must be IGNORED, including a write
	// (which must therefore NOT execute).
	for _, m := range []any{
		&proto.Run{Query: "RETURN 1"},
		&proto.Run{Query: "CREATE (:Ghost)"},
		&proto.Pull{N: -1, QID: -1},
		&proto.Begin{},
	} {
		msgs, err := sess.HandleMessage(ctx, m)
		if err != nil {
			t.Fatalf("HandleMessage(%T) in FAILED: %v", m, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("HandleMessage(%T): %d responses, want 1", m, len(msgs))
		}
		if _, ok := msgs[0].(*proto.Ignored); !ok {
			t.Errorf("HandleMessage(%T) in FAILED returned %T, want *proto.Ignored", m, msgs[0])
		}
		if sess.state != StateFailed {
			t.Fatalf("state after %T in FAILED = %v, want still FAILED", m, sess.state)
		}
	}

	// RESET escapes FAILED back to READY with SUCCESS.
	msgs, err := sess.HandleMessage(ctx, &proto.Reset{})
	if err != nil {
		t.Fatalf("HandleMessage(RESET): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("RESET: %d responses, want 1", len(msgs))
	}
	if _, ok := msgs[0].(*proto.Success); !ok {
		t.Errorf("RESET returned %T, want *proto.Success", msgs[0])
	}
	if sess.state != StateReady {
		t.Fatalf("state after RESET = %v, want READY", sess.state)
	}
}
