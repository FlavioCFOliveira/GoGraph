package server

// failed_tx_reclaim_test.go — white-box regression for the #1312 invariant that
// EVERY transition into FAILED reclaims an open explicit transaction, including
// the paths that never reach a per-message handler.
//
// These drive Session.HandleMessage directly (package server) so they can probe
// the unexported tx/txActive/state fields and exercise the FAILED-entry sites
// that a wire-level test cannot isolate cleanly — in particular the
// context-cancellation early return, which returns before dispatch and so was
// NOT covered by the previous post-dispatch reclaim. The engine is store-less,
// so BeginTx holds the engine writer mutex; a second BeginTx that completes
// promptly proves that mutex was released by the reclaim, not held until RESET.

import (
	"context"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// beginOpenSession returns a session in TX_READY with an open explicit
// transaction (engine writer mutex held), having driven HELLO + BEGIN through
// HandleMessage.
func beginOpenSession(t *testing.T) *Session {
	t.Helper()
	sess := newSession(newTestEngine(t), NoAuthHandler{}, "")
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if sess.state != StateReady {
		t.Fatalf("state after HELLO: got %v, want READY", sess.state)
	}
	if _, err := sess.HandleMessage(context.Background(), &proto.Begin{Extra: map[string]any{}}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if sess.state != StateTxReady {
		t.Fatalf("state after BEGIN: got %v, want TX_READY", sess.state)
	}
	if sess.tx == nil || !sess.txActive {
		t.Fatalf("after BEGIN: tx=%v txActive=%v, want non-nil tx and txActive=true", sess.tx, sess.txActive)
	}
	return sess
}

// assertWriterReleased opens a second explicit transaction on eng and asserts it
// is acquired promptly. The store-less engine writer mutex is a plain (not
// context-aware) lock, so a leaked first transaction would make this acquire
// block indefinitely; the watchdog converts that into a test failure rather than
// a hang.
func assertWriterReleased(t *testing.T, sess *Session) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		tx, err := sess.eng.BeginTx(context.Background())
		if err == nil {
			_ = tx.Rollback() // release immediately; we only needed to acquire
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("second BeginTx after FAILED reclaim returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second BeginTx blocked — the FAILED transition did not release the writer mutex (#1312)")
	}
}

// assertReclaimedAndResettable asserts the session reclaimed its transaction at
// the FAILED transition (tx nil, txActive false, state FAILED, writer released)
// and that a subsequent RESET is a clean idempotent no-op returning to READY
// with a SUCCESS — never a double-rollback FAILURE.
func assertReclaimedAndResettable(t *testing.T, sess *Session) {
	t.Helper()
	if sess.state != StateFailed {
		t.Fatalf("state after forced failure: got %v, want FAILED", sess.state)
	}
	if sess.tx != nil {
		t.Fatalf("tx after FAILED transition: got non-nil, want nil (reclaimed at the transition)")
	}
	if sess.txActive {
		t.Fatalf("txActive after FAILED transition: got true, want false")
	}
	assertWriterReleased(t, sess)

	// RESET must cleanly recover the session: the transaction is already gone, so
	// RESET's own rollback is an idempotent no-op (no error, returns to READY).
	resp, err := sess.HandleMessage(context.Background(), &proto.Reset{})
	if err != nil {
		t.Fatalf("RESET HandleMessage: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("RESET response count: got %d, want 1", len(resp))
	}
	if f, isFail := resp[0].(*proto.Failure); isFail {
		t.Fatalf("RESET after reclaimed FAILED tx returned FAILURE: code=%s message=%s (double-rollback?)", f.Code, f.Message)
	}
	if _, isSuccess := resp[0].(*proto.Success); !isSuccess {
		t.Fatalf("RESET after reclaimed FAILED tx: got %T, want *proto.Success", resp[0])
	}
	if sess.state != StateReady {
		t.Fatalf("state after RESET: got %v, want READY", sess.state)
	}
}

// TestSession_FailedOnIllegalMessage_ReclaimsOpenTx forces FAILED via an illegal
// message (PULL in TX_READY) while a transaction is open and asserts the
// transaction is reclaimed at the FAILED transition and a later RESET is clean.
func TestSession_FailedOnIllegalMessage_ReclaimsOpenTx(t *testing.T) {
	t.Parallel()
	sess := beginOpenSession(t)

	// PULL is illegal in TX_READY → failTransition → enterFailed reclaims the tx.
	resp, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("illegal PULL HandleMessage: %v", err)
	}
	if _, isFail := resp[0].(*proto.Failure); !isFail {
		t.Fatalf("illegal PULL: got %T, want *proto.Failure", resp[0])
	}
	assertReclaimedAndResettable(t, sess)
}

// TestSession_FailedOnContextCancel_ReclaimsOpenTx is the path the previous
// post-dispatch reclaim MISSED: HandleMessage's context-cancellation early
// return (it fails the message before dispatch, so a post-dispatch tail never
// runs). With the FAILED-entry funnel, the open transaction is still reclaimed.
func TestSession_FailedOnContextCancel_ReclaimsOpenTx(t *testing.T) {
	t.Parallel()
	sess := beginOpenSession(t)

	// Deliver any message under an already-cancelled context: HandleMessage fails
	// it immediately via failWith → enterFailed, before reaching dispatch.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := sess.HandleMessage(cancelled, &proto.Run{Query: "RETURN 1", Parameters: map[string]any{}, Extra: map[string]any{}})
	if err != nil {
		t.Fatalf("cancelled HandleMessage: %v", err)
	}
	f, isFail := resp[0].(*proto.Failure)
	if !isFail {
		t.Fatalf("cancelled message: got %T, want *proto.Failure", resp[0])
	}
	if f.Code != "Neo.TransientError.General.RequestInterrupted" {
		t.Fatalf("cancelled message FAILURE code: got %q, want Neo.TransientError.General.RequestInterrupted", f.Code)
	}
	assertReclaimedAndResettable(t, sess)
}

// TestSession_FailedThenReset_NoDoubleRollback pins the idempotency contract: a
// reclaimed FAILED transaction tolerates RESET without a double-rollback even
// when RESET is preceded by the connection-teardown Close (the other reclaim
// path). Calling Close then RESET then a second Close must all be no-ops.
func TestSession_FailedThenReset_NoDoubleRollback(t *testing.T) {
	t.Parallel()
	sess := beginOpenSession(t)

	if _, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1}); err != nil {
		t.Fatalf("illegal PULL HandleMessage: %v", err)
	}
	if sess.tx != nil {
		t.Fatalf("tx not reclaimed at FAILED transition")
	}

	// Close on an already-reclaimed session is a no-op (tx already nil).
	sess.Close()
	if sess.tx != nil || sess.txActive {
		t.Fatalf("after Close: tx=%v txActive=%v, want nil/false", sess.tx, sess.txActive)
	}

	// RESET still returns cleanly to READY.
	resp, err := sess.HandleMessage(context.Background(), &proto.Reset{})
	if err != nil {
		t.Fatalf("RESET HandleMessage: %v", err)
	}
	if _, isSuccess := resp[0].(*proto.Success); !isSuccess {
		t.Fatalf("RESET: got %T, want *proto.Success", resp[0])
	}
	if sess.state != StateReady {
		t.Fatalf("state after RESET: got %v, want READY", sess.state)
	}

	// A final Close remains a no-op.
	sess.Close()
}
