package server

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestSession_InFlightCursorCap_RejectionAbortsDoomedTx verifies the
// per-connection in-flight cursor cap (T737) together with the #1309
// FAILED-with-open-transaction rollback. A second RUN over an explicit
// transaction whose cap is 1 is rejected with LimitExceeded and moves the
// session to FAILED; because only RESET can escape FAILED (and would roll the
// transaction back anyway), the now-doomed transaction is rolled back
// immediately so its in-memory writes are unwound and the engine writer
// serialisation is released without waiting for the client's RESET. The
// in-flight count therefore drops to 0 (the transaction is gone), and the cap
// rejection itself still surfaces as the typed LimitExceeded FAILURE.
//
// This supersedes the pre-#1280 behaviour in which a cap rejection left the open
// transaction (and the cursor it had accumulated) intact until RESET — under
// true explicit transactions that would leak the global write lock.
func TestSession_InFlightCursorCap_RejectionAbortsDoomedTx(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)
	sess.setMaxInFlight(1) // explicit cap=1 so the second RUN is rejected

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

	// The cap breach moved the session to FAILED, which now rolls back the doomed
	// transaction (#1309): the writer serialisation is released and the
	// transaction is cleared, so the in-flight count drops to 0.
	if sess.state != StateFailed {
		t.Fatalf("state after rejection = %v; want FAILED", sess.state)
	}
	if sess.tx != nil {
		t.Fatal("explicit transaction must be rolled back and cleared after a cap-breach FAILED")
	}
	if got := sess.inFlightCount(); got != 0 {
		t.Fatalf("inFlightCount after rejection = %d; want 0 (doomed tx rolled back)", got)
	}
}
