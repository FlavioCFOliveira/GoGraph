package server

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestReap_DeliversTerminatedFailureThenIgnores is the #1784 gate: when the
// tx-timeout reaper terminates an idle transaction (server-initiated FAILED),
// the client's NEXT request-phase message must receive a typed
// TransactionTimedOut FAILURE (so it learns why), a SUBSEQUENT message must be
// IGNORED, and RESET must restore READY.
func TestReap_DeliversTerminatedFailureThenIgnores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := newReadySession(t)

	// Open an explicit transaction, then simulate the reaper firing.
	if _, err := sess.HandleMessage(ctx, &proto.Begin{}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	sess.reapTimedOutTx()
	if sess.state != StateFailed {
		t.Fatalf("after reap: state = %v, want FAILED", sess.state)
	}

	// First request-phase message → typed TransactionTimedOut FAILURE.
	msgs, err := sess.HandleMessage(ctx, &proto.Run{Query: "RETURN 1"})
	if err != nil {
		t.Fatalf("RUN after reap: %v", err)
	}
	f, ok := msgs[0].(*proto.Failure)
	if !ok {
		t.Fatalf("first message after reap: got %T, want *proto.Failure", msgs[0])
	}
	if f.Code != "Neo.ClientError.Transaction.TransactionTimedOut" {
		t.Errorf("reap FAILURE code = %q, want Neo.ClientError.Transaction.TransactionTimedOut", f.Code)
	}

	// Subsequent message → IGNORED (the FAILURE was delivered once).
	msgs, err = sess.HandleMessage(ctx, &proto.Run{Query: "RETURN 2"})
	if err != nil {
		t.Fatalf("second RUN: %v", err)
	}
	if _, ok := msgs[0].(*proto.Ignored); !ok {
		t.Errorf("second message after reap: got %T, want *proto.Ignored", msgs[0])
	}

	// RESET restores READY.
	msgs, err = sess.HandleMessage(ctx, &proto.Reset{})
	if err != nil {
		t.Fatalf("RESET: %v", err)
	}
	if _, ok := msgs[0].(*proto.Success); !ok {
		t.Errorf("RESET: got %T, want *proto.Success", msgs[0])
	}
	if sess.state != StateReady {
		t.Fatalf("after RESET: state = %v, want READY", sess.state)
	}
}
