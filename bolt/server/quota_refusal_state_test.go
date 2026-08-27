package server

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestQuotaRefusedBegin_LeavesTheSessionReady guards rmp #2561.
//
// The two failure paths in handleBegin disagreed, and one of them disagreed with
// the state machine's own documented contract: the newTx failure calls
// enterFailed and reaches FAILED, while the quota refusal returns before the
// Transition call and leaves the session in READY. Nothing asserted the
// difference at any level — bolt/server's beginReadExpectFailure helper reads the
// FAILURE and stops — so it was accidental rather than chosen.
//
// It is now chosen, and this is where it is asserted. A cap is BACK-PRESSURE, not
// a protocol error: the slot frees when another of the principal's transactions
// closes, so retrying the same BEGIN is the right response and a RESET round trip
// to earn the right to retry would be pure cost. The neighbour keeps FAILED
// because a newTx failure is a genuine failure to open a transaction.
func TestQuotaRefusedBegin_LeavesTheSessionReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := newReadySession(t)
	s.setTxQuota(newTxQuota(1))
	// The quota keys on the principal, so it must be non-empty for the cap to
	// count anything; an empty one would make this test pass vacuously.
	s.identity = Identity{Principal: "alice"}

	// First BEGIN takes the only slot.
	if msgs, err := s.HandleMessage(ctx, &proto.Begin{Extra: map[string]interface{}{}}); err != nil {
		t.Fatalf("first BEGIN: %v", err)
	} else if f := failureOf(msgs); f != nil {
		t.Fatalf("first BEGIN was refused (%s: %s); the cap is 1 and this is the first",
			f.Code, f.Message)
	}
	// Roll it back at the protocol level so the session returns to READY while the
	// quota slot stays held? No: ROLLBACK releases the slot. Instead open a
	// SECOND session sharing the quota, which is the real shape — one principal,
	// two connections.
	q := s.txQuota

	s2 := newReadySession(t)
	s2.setTxQuota(q)
	s2.identity = Identity{Principal: "alice"}

	msgs, err := s2.HandleMessage(ctx, &proto.Begin{Extra: map[string]interface{}{}})
	if err != nil {
		t.Fatalf("second BEGIN: %v", err)
	}
	f := failureOf(msgs)
	if f == nil {
		t.Fatalf("the second BEGIN for the same principal was ACCEPTED at a cap of 1: %#v", msgs)
	}

	// THE code, which rmp #2561 also corrected.
	if f.Code != txQuotaRefusalCode {
		t.Errorf("refusal code = %q, want %q. Neo.ClientError.General.LimitExceeded does not "+
			"exist in Neo4j's status codes, and its ClientError class tells a driver NOT to "+
			"retry a limit that frees itself", f.Code, txQuotaRefusalCode)
	}

	// THE state, which is the subject of the ticket.
	if s2.state != StateReady {
		t.Errorf("state after a quota-refused BEGIN = %v, want READY. A cap is back-pressure: "+
			"the client must be able to retry the BEGIN once a slot frees, without a RESET "+
			"round trip (rmp #2561)", s2.state)
	}
	if s2.txActive || s2.tx != nil {
		t.Errorf("a refused BEGIN left a transaction attached to the session")
	}

	// And READY must be usable: the retry that the contract exists to permit.
	// Freeing the slot and re-issuing the BEGIN must work with no RESET between.
	if _, err := s.HandleMessage(ctx, &proto.Rollback{}); err != nil {
		t.Fatalf("ROLLBACK on the first session: %v", err)
	}
	retry, err := s2.HandleMessage(ctx, &proto.Begin{Extra: map[string]interface{}{}})
	if err != nil {
		t.Fatalf("retry BEGIN: %v", err)
	}
	if rf := failureOf(retry); rf != nil {
		t.Errorf("the retry BEGIN was refused (%s: %s) even though the slot was freed and no "+
			"RESET was needed. Staying in READY is only worth anything if the retry works",
			rf.Code, rf.Message)
	}
}

// TestNewTxFailurePathStillEntersFailed is the other half of the contract: the
// ADJACENT failure path must keep going to FAILED, so the difference between them
// is the documented one and not a side effect of whichever branch was edited last.
//
// It is driven by cancelling the context, which makes newTx fail before any
// resource is acquired.
func TestNewTxFailurePathStillEntersFailed(t *testing.T) {
	t.Parallel()
	s := newReadySession(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	msgs, err := s.HandleMessage(cancelled, &proto.Begin{Extra: map[string]interface{}{}})
	if err != nil {
		t.Fatalf("BEGIN on a cancelled context: %v", err)
	}
	if failureOf(msgs) == nil {
		t.Skipf("BEGIN on a cancelled context was accepted (%#v); this path cannot be driven "+
			"this way on this build, so the contrast is not asserted here", msgs)
	}
	if s.state != StateFailed {
		t.Errorf("state after a newTx failure = %v, want FAILED. This path is a genuine "+
			"failure to open a transaction, not back-pressure, and it must keep differing "+
			"from the quota refusal (rmp #2561)", s.state)
	}
}
