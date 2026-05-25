package server

import (
	"context"
	"testing"

	"gograph/bolt/proto"
)

// TestSession_InFlightCursorCap_FirstCursorSurvivesRejection verifies AC2 of
// T737: after the second RUN is rejected with LimitExceeded, the first cursor
// that was already accumulated in tx.results is still present and the in-flight
// count remains 1. The rejection must not close or evict the first cursor.
func TestSession_InFlightCursorCap_FirstCursorSurvivesRejection(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	// BEGIN → TX_READY.
	if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}

	// First RUN + PULL — cursor lands in tx.results[0], not yet closed.
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query: "MATCH (n) RETURN n",
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("first RUN: %v", err)
	}
	if _, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1}); err != nil {
		t.Fatalf("PULL: %v", err)
	}
	if got := sess.inFlightCount(); got != 1 {
		t.Fatalf("inFlightCount after first PULL = %d; want 1", got)
	}

	// Second RUN — must be rejected.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query: "MATCH (n) RETURN n",
		Extra: map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("second RUN HandleMessage error: %v", err)
	}
	fail, ok := msgs[0].(*proto.Failure)
	if !ok {
		t.Fatalf("second RUN response: %T, want *proto.Failure", msgs[0])
	}
	if fail.Code != "Neo.ClientError.General.LimitExceeded" {
		t.Fatalf("failure code = %q; want LimitExceeded", fail.Code)
	}

	// AC2: first cursor must still be present in tx.results. The in-flight count
	// reflects what is still registered in the transaction; a count of 1 proves
	// the cursor was not evicted or closed by the rejection logic.
	//
	// Note: handleRun moves state to FAILED on cap breach, so we access the
	// internal counter directly rather than via another HandleMessage call.
	if got := sess.inFlightCount(); got != 1 {
		t.Fatalf("inFlightCount after rejection = %d; want 1 (first cursor must survive)", got)
	}
}
