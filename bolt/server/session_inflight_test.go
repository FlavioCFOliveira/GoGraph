package server

import (
	"context"
	"strings"
	"testing"

	"gograph/bolt/proto"
)

// TestSession_InFlightCursorCap_DefaultRejectsSecondRunInTx confirms the
// default Options.MaxInFlightPerConnection (= 1) rejects a second RUN
// inside an explicit transaction once the first cursor has been
// returned to the session but not yet COMMIT'd.
//
// Without the cap, an attacker could BEGIN, then loop RUN→PULL all
// indefinitely; every Result lands in tx.results unclosed and the
// connection's heap footprint grows linearly with the loop count.
func TestSession_InFlightCursorCap_DefaultRejectsSecondRunInTx(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

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

	// PULL all → drain but DO NOT close (mimics legitimate driver behaviour).
	if _, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1}); err != nil {
		t.Fatalf("PULL: %v", err)
	}
	// We are back in TX_READY but tx.results still holds the cursor.
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

// TestSession_InFlightCursorCap_RaisedAllowsMoreCursors verifies that
// raising MaxInFlightPerConnection on the session lets the
// corresponding number of cursors accumulate before the cap trips.
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

// TestSession_InFlightCursorCap_AutoCommitNotAffected confirms that
// an auto-commit RUN under the default cap still works: the state
// machine already prevents two auto-commit RUNs from co-existing, so
// the cap of 1 is non-restrictive in this path.
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
	// A second auto-commit RUN must be accepted; the cap counts the
	// auto-commit cursor as 0 once handlePull has cleared s.result.
	// (handlePull behaviour: when the cursor is exhausted s.result
	// is not yet cleared in the current implementation, but the
	// new s.result assignment on the next RUN drops the old one.
	// We assert the behaviour observed at the public surface: RUN
	// after PULL returns SUCCESS in auto-commit mode.)
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
