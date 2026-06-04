package server_test

// e2e_failed_tx_reclaim_test.go — regression test for reclaiming the open
// transaction when a Bolt session enters the FAILED state (#1312).
//
// While an explicit write transaction is open, the session holds the engine's
// single-writer serialisation (the store mutex on a WAL-backed engine). An
// illegal message moves the session to FAILED. A FAILED session can never
// legally resume the transaction — only RESET escapes FAILED, and RESET
// discards the transaction anyway — so holding the writer lock for the whole
// FAILED→RESET window would needlessly block every other writer. The session
// must therefore roll the transaction back AT the FAILED transition, releasing
// the writer lock immediately, and that rollback must be idempotent so a later
// RESET does not double-roll-back or error.
//
// This drives the RAW Bolt wire (bolt test client) against a WAL-backed server
// so the open transaction holds the store single-writer mutex — the resource
// whose prompt release this test asserts.

import (
	"time"

	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// TestExplicitTx_FailedTransition_ReleasesWriterMutexPromptly is the #1312 AC.
//
// Session A completes the handshake, sends BEGIN + RUN CREATE (acquiring the
// store single-writer mutex), then sends an illegal message (PULL is illegal in
// TX_READY) to force the session into FAILED. The FAILED transition must roll
// the open transaction back, releasing the writer mutex IMMEDIATELY — before the
// client sends RESET. A second connection's write must therefore complete
// promptly. Then A sends RESET: the session must return cleanly to READY with a
// SUCCESS (not a FAILURE) — the transaction was already reclaimed, so RESET's
// own rollback is a clean idempotent no-op and never double-rolls-back.
//
// Pre-fix, the FAILED transition left sess.tx open and the writer mutex held
// until the client sent RESET (or disconnected): the second connection's write
// would block until A's RESET, so the watchdog below would fire.
func TestExplicitTx_FailedTransition_ReleasesWriterMutexPromptly(t *testing.T) {
	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{})

	// Connection A: handshake, HELLO, BEGIN, RUN CREATE — acquires the writer.
	cA := newBoltTestClient(t, addr)
	defer cA.close(t)
	cA.negotiate(t)
	cA.hello(t)
	cA.begin(t)
	cA.run(t, "CREATE (:FailDoomed {v:1})", nil)

	// Force FAILED with an illegal message. PULL is only legal in a streaming
	// state; in TX_READY (after the RUN's SUCCESS was consumed without PULL it is
	// actually TX_STREAMING, so use a message illegal there too). To get a
	// deterministic illegal transition we send BEGIN again: a nested BEGIN is
	// rejected, but that path returns a typed FAILURE WITHOUT leaving FAILED. The
	// reliable FAILED trigger is an out-of-state PULL after draining the cursor.
	//
	// After RUN the session is in TX_STREAMING with an open cursor. DISCARD it to
	// return to TX_READY, then send a PULL — illegal in TX_READY — which drives an
	// invalid transition to FAILED.
	cA.sendRequest(t, &proto.Discard{N: -1, QID: -1})
	cA.recvSuccess(t) // DISCARD success → back to TX_READY, tx still open
	cA.sendRequest(t, &proto.Pull{N: -1, QID: -1})
	failResp := cA.recvFailure(t) // illegal PULL in TX_READY → FAILED
	if failResp.Code == "" {
		t.Fatalf("expected a typed FAILURE on the illegal transition, got empty code")
	}

	// Connection B: its write must complete promptly. The writer mutex must have
	// been released by the FAILED-transition rollback, NOT held until A's RESET.
	done := make(chan struct{})
	go func() {
		defer close(done)
		cB := newBoltTestClient(t, addr)
		defer cB.close(t)
		cB.negotiate(t)
		cB.hello(t)
		cB.run(t, "CREATE (:FailB {v:1})", nil)
		cB.pullAll(t)
	}()
	select {
	case <-done:
		// Expected: the writer mutex was released on A's FAILED-transition rollback.
	case <-time.After(5 * time.Second):
		t.Fatal("second connection's write did not complete promptly — writer mutex held by the FAILED session until RESET (#1312)")
	}

	// A now sends RESET: it must return cleanly to READY with a SUCCESS. The
	// transaction was already reclaimed at the FAILED transition, so RESET's own
	// rollback must be an idempotent no-op (no double-rollback, no FAILURE).
	cA.sendRequest(t, &proto.Reset{})
	resetResp := cA.recvResponse(t)
	if f, isFail := resetResp.(*proto.Failure); isFail {
		t.Fatalf("RESET after a reclaimed FAILED transaction returned FAILURE: code=%s message=%s (double-rollback?)", f.Code, f.Message)
	}
	if _, isSuccess := resetResp.(*proto.Success); !isSuccess {
		t.Fatalf("RESET after a reclaimed FAILED transaction: expected SUCCESS, got %T", resetResp)
	}

	// Sanity: the session is back in READY and usable for a fresh auto-commit
	// query (proves RESET cleanly recovered the session, not just the lock).
	cA.run(t, "RETURN 1 AS n", nil)
	cA.pullAll(t)
}
