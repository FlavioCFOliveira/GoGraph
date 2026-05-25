package server

import (
	"context"
	"testing"

	"gograph/bolt/proto"
)

// TestReset_DrainsCursorAndRollsBackTx covers T686 AC1–AC4:
//
//  1. Reset from STREAMING returns *proto.Success.
//  2. Cursor is drained (sess.result == nil after Reset).
//  3. Session state is READY after Reset.
//  4. Active tx (if any) is rolled back (sess.tx == nil after Reset).
//
// Goroutine cleanliness (AC6) is verified globally by the goleak check in
// TestMain; no per-test goleak call is needed here.
func TestReset_DrainsCursorAndRollsBackTx(t *testing.T) {
	t.Parallel()

	t.Run("reset_from_streaming_drains_cursor", func(t *testing.T) {
		t.Parallel()
		sess := newReadySession(t)

		// RUN → STREAMING (empty graph: cursor opens, zero rows).
		if _, err := sess.HandleMessage(context.Background(), &proto.Run{
			Query: "MATCH (n) RETURN n",
			Extra: map[string]interface{}{},
		}); err != nil {
			t.Fatalf("RUN: %v", err)
		}
		if sess.state != StateStreaming {
			t.Fatalf("pre-condition: want STREAMING, got %v", sess.state)
		}
		if sess.result == nil {
			t.Fatal("pre-condition: result cursor must be non-nil after RUN")
		}

		// RESET.
		msgs, err := sess.HandleMessage(context.Background(), &proto.Reset{})
		if err != nil {
			t.Fatalf("RESET: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("RESET response count: got %d, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*proto.Success); !ok {
			t.Fatalf("RESET response type: got %T, want *proto.Success", msgs[0])
		}
		// AC3: state is READY.
		if sess.state != StateReady {
			t.Fatalf("state after RESET: got %v, want READY", sess.state)
		}
		// AC2: cursor is drained.
		if sess.result != nil {
			t.Fatal("result cursor not nil after RESET — goroutine leak possible")
		}
	})

	t.Run("reset_rolls_back_active_tx", func(t *testing.T) {
		t.Parallel()
		sess := newReadySession(t)

		// BEGIN → TX_READY.
		if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
			Extra: map[string]interface{}{},
		}); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		if sess.state != StateTxReady {
			t.Fatalf("pre-condition: want TX_READY, got %v", sess.state)
		}
		if sess.tx == nil {
			t.Fatal("pre-condition: tx must be non-nil after BEGIN")
		}

		// RESET from TX_READY — tx must be rolled back.
		msgs, err := sess.HandleMessage(context.Background(), &proto.Reset{})
		if err != nil {
			t.Fatalf("RESET: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("RESET response count: got %d, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*proto.Success); !ok {
			t.Fatalf("RESET response type: got %T, want *proto.Success", msgs[0])
		}
		// AC4: active tx is rolled back.
		if sess.tx != nil {
			t.Fatal("tx not nil after RESET — transaction was not rolled back")
		}
		if sess.txActive {
			t.Fatal("txActive is true after RESET")
		}
		// AC3: state is READY.
		if sess.state != StateReady {
			t.Fatalf("state after RESET: got %v, want READY", sess.state)
		}
	})

	t.Run("reset_from_tx_streaming_drains_cursor_and_tx", func(t *testing.T) {
		t.Parallel()
		sess := newReadySession(t)

		// BEGIN → TX_READY.
		if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
			Extra: map[string]interface{}{},
		}); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		// RUN in tx → TX_STREAMING.
		if _, err := sess.HandleMessage(context.Background(), &proto.Run{
			Query: "MATCH (n) RETURN n",
			Extra: map[string]interface{}{},
		}); err != nil {
			t.Fatalf("RUN: %v", err)
		}
		if sess.state != StateTxStreaming {
			t.Fatalf("pre-condition: want TX_STREAMING, got %v", sess.state)
		}

		// RESET.
		msgs, err := sess.HandleMessage(context.Background(), &proto.Reset{})
		if err != nil {
			t.Fatalf("RESET: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("RESET response count: got %d, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*proto.Success); !ok {
			t.Fatalf("RESET response type: got %T, want *proto.Success", msgs[0])
		}
		if sess.state != StateReady {
			t.Fatalf("state after RESET: got %v, want READY", sess.state)
		}
		if sess.tx != nil {
			t.Fatal("tx not nil after RESET")
		}
	})

	t.Run("reset_from_failed_returns_to_ready", func(t *testing.T) {
		t.Parallel()
		sess := newSession(newTestEngine(t), NoAuthHandler{}, "")
		sess.state = StateFailed

		msgs, err := sess.HandleMessage(context.Background(), &proto.Reset{})
		if err != nil {
			t.Fatalf("RESET: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("RESET response count: got %d, want 1", len(msgs))
		}
		if _, ok := msgs[0].(*proto.Success); !ok {
			t.Fatalf("RESET response type: got %T, want *proto.Success", msgs[0])
		}
		if sess.state != StateReady {
			t.Fatalf("state after RESET from FAILED: got %v, want READY", sess.state)
		}
	})
}
