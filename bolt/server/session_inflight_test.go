package server

import (
	"context"
	"strings"
	"testing"

	"gograph/bolt/proto"
)

// TestSession_InFlightCursorCap_DefaultRejectsSecondRunInTx confirms that a
// session with an explicit cap of 1 rejects a second RUN inside an explicit
// transaction once the first cursor has been registered (even after it has
// been fully PULL'd and drained). inFlightCount counts all Result cursors
// appended to tx.results since BEGIN — both open and already-exhausted — so
// the cap bounds the total number of RUN statements per transaction.
//
// Without the cap, a client could BEGIN and loop RUN→PULL indefinitely,
// growing tx.results without bound.
func TestSession_InFlightCursorCap_DefaultRejectsSecondRunInTx(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)
	sess.setMaxInFlight(1) // explicit cap=1; does not rely on the server default

	// BEGIN → TX_READY.
	if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}

	// First RUN — accepted; cursor lands in tx.results.
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}); err != nil {
		t.Fatalf("first RUN: %v", err)
	}

	// PULL all → cursor is drained, but tx.results still holds the pointer.
	if _, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1}); err != nil {
		t.Fatalf("PULL: %v", err)
	}
	// tx.results has one entry (drained cursor); the cap counts it.
	if got := sess.inFlightCount(); got != 1 {
		t.Fatalf("inFlightCount after first PULL = %d; want 1", got)
	}

	// Second RUN — must be rejected by the cap.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("second RUN HandleMessage error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("second RUN response count = %d; want 1", len(msgs))
	}
	fail, ok := msgs[0].(*proto.Failure)
	if !ok {
		t.Fatalf("second RUN response: %T, want *proto.Failure", msgs[0])
	}
	if fail.Code != "Neo.ClientError.General.LimitExceeded" {
		t.Fatalf("FAILURE code = %q; want Neo.ClientError.General.LimitExceeded", fail.Code)
	}
	if !strings.Contains(fail.Message, "cap=1") {
		t.Errorf("FAILURE message %q does not mention cap; want a diagnostic that names the limit", fail.Message)
	}
}

// TestSession_InFlightCursorCap_RaisedAllowsMoreCursors verifies that a
// session with cap=3 allows exactly three RUN+PULL cycles within a single
// explicit transaction and rejects the fourth. After each PULL the cursor is
// drained but its entry remains in tx.results, so inFlightCount increments
// monotonically until COMMIT/ROLLBACK clears the slice.
func TestSession_InFlightCursorCap_RaisedAllowsMoreCursors(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)
	sess.setMaxInFlight(3)

	if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}

	// Three RUN+PULL cycles fit under cap=3.
	for i := 0; i < 3; i++ {
		if _, err := sess.HandleMessage(context.Background(), &proto.Run{
			Query:      "MATCH (n) RETURN n",
			Parameters: nil,
			Extra:      map[string]interface{}{},
		}); err != nil {
			t.Fatalf("RUN %d: %v", i, err)
		}
		if _, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1}); err != nil {
			t.Fatalf("PULL %d: %v", i, err)
		}
	}
	// After three RUN+PULL cycles, tx.results holds three (drained) cursors.
	if got := sess.inFlightCount(); got != 3 {
		t.Fatalf("inFlightCount after 3 RUN+PULL cycles = %d; want 3", got)
	}

	// Fourth RUN trips the cap.
	msgs, _ := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	})
	if fail, ok := msgs[0].(*proto.Failure); !ok || fail.Code != "Neo.ClientError.General.LimitExceeded" {
		t.Fatalf("fourth RUN: %T %+v; want LimitExceeded Failure", msgs[0], msgs[0])
	}
}

// TestSession_InFlightCursorCap_SetMaxInFlightIgnoresNonPositive
// confirms the setter rejects misconfiguration (zero / negative
// values) instead of silently disabling the cap.
func TestSession_InFlightCursorCap_SetMaxInFlightIgnoresNonPositive(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)
	before := sess.maxInFlight
	for _, n := range []int{0, -1, -1024} {
		sess.setMaxInFlight(n)
		if sess.maxInFlight != before {
			t.Errorf("setMaxInFlight(%d) mutated cap to %d (was %d)", n, sess.maxInFlight, before)
		}
	}
}

// TestSession_InFlightCursorCap_AutoCommitNotAffected confirms that an
// auto-commit RUN+PULL+RUN sequence succeeds. The cap only counts cursors
// registered inside an explicit transaction (tx.results); the auto-commit
// cursor (s.result) is cleared by drainResult after a full PULL, so
// inFlightCount returns 0 and the second RUN is accepted.
func TestSession_InFlightCursorCap_AutoCommitNotAffected(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}); err != nil {
		t.Fatalf("first RUN: %v", err)
	}
	if _, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1}); err != nil {
		t.Fatalf("PULL: %v", err)
	}
	if sess.state != StateReady {
		t.Fatalf("state after auto-commit PULL = %v; want READY", sess.state)
	}
	// drainResult clears s.result after exhaustion; inFlightCount = 0.
	// The second auto-commit RUN must be accepted.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("second RUN: %v", err)
	}
	if _, ok := msgs[0].(*proto.Success); !ok {
		t.Fatalf("second RUN response: %T %+v; want *proto.Success (the cap must not block auto-commit reuse)", msgs[0], msgs[0])
	}
}
