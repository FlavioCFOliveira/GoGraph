package server

import (
	"context"
	"testing"

	"gograph/bolt/proto"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Task 312: Explicit transaction management tests
// ─────────────────────────────────────────────────────────────────────────────

// TestTx_BeginRunPullCommit verifies the full BEGIN → RUN → PULL → COMMIT
// cycle. After commit the session must be in READY state and the SUCCESS
// response must carry a non-empty bookmark.
func TestTx_BeginRunPullCommit(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	// BEGIN → TX_READY.
	beginMsgs, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if len(beginMsgs) != 1 {
		t.Fatalf("BEGIN response count: %d", len(beginMsgs))
	}
	if _, ok := beginMsgs[0].(*proto.Success); !ok {
		t.Fatalf("BEGIN response: %T, want *proto.Success", beginMsgs[0])
	}
	if sess.state != StateTxReady {
		t.Fatalf("state after BEGIN: got %v, want TX_READY", sess.state)
	}

	// RUN inside tx → TX_STREAMING.
	runMsgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if _, ok := runMsgs[0].(*proto.Success); !ok {
		t.Fatalf("RUN response: %T, want *proto.Success", runMsgs[0])
	}
	if sess.state != StateTxStreaming {
		t.Fatalf("state after RUN: got %v, want TX_STREAMING", sess.state)
	}

	// PULL all → TX_READY.
	pullMsgs, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("PULL: %v", err)
	}
	last := pullMsgs[len(pullMsgs)-1]
	if _, ok := last.(*proto.Success); !ok {
		t.Fatalf("PULL last message: %T, want *proto.Success", last)
	}
	if sess.state != StateTxReady {
		t.Fatalf("state after PULL in tx: got %v, want TX_READY", sess.state)
	}

	// COMMIT → READY.
	commitMsgs, err := sess.HandleMessage(context.Background(), &proto.Commit{})
	if err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if len(commitMsgs) != 1 {
		t.Fatalf("COMMIT response count: %d", len(commitMsgs))
	}
	commitSuccess, ok := commitMsgs[0].(*proto.Success)
	if !ok {
		t.Fatalf("COMMIT response: %T, want *proto.Success", commitMsgs[0])
	}
	bm, ok := commitSuccess.Metadata["bookmark"]
	if !ok {
		t.Fatal("COMMIT SUCCESS missing 'bookmark'")
	}
	if bm == "" {
		t.Fatal("COMMIT SUCCESS bookmark is empty")
	}
	if sess.state != StateReady {
		t.Fatalf("state after COMMIT: got %v, want READY", sess.state)
	}
	if sess.txActive {
		t.Fatal("txActive is true after COMMIT")
	}
	if sess.tx != nil {
		t.Fatal("tx is non-nil after COMMIT")
	}
}

// TestTx_BeginRunRollback verifies that BEGIN → RUN → ROLLBACK discards the
// result and returns the session to READY.
func TestTx_BeginRunRollback(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)

	// BEGIN.
	if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}

	// RUN.
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}); err != nil {
		t.Fatalf("RUN: %v", err)
	}

	// PULL to move to TX_READY.
	if _, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1}); err != nil {
		t.Fatalf("PULL: %v", err)
	}
	if sess.state != StateTxReady {
		t.Fatalf("state before ROLLBACK: got %v, want TX_READY", sess.state)
	}

	// ROLLBACK → READY.
	rollMsgs, err := sess.HandleMessage(context.Background(), &proto.Rollback{})
	if err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}
	if len(rollMsgs) != 1 {
		t.Fatalf("ROLLBACK response count: %d", len(rollMsgs))
	}
	if _, ok := rollMsgs[0].(*proto.Success); !ok {
		t.Fatalf("ROLLBACK response: %T, want *proto.Success", rollMsgs[0])
	}
	if sess.state != StateReady {
		t.Fatalf("state after ROLLBACK: got %v, want READY", sess.state)
	}
	if sess.txActive {
		t.Fatal("txActive is true after ROLLBACK")
	}
	if sess.tx != nil {
		t.Fatal("tx is non-nil after ROLLBACK")
	}
}

// TestTx_NestedBeginRejected verifies that a second BEGIN while a transaction
// is already active is rejected with FAILURE.
func TestTx_NestedBeginRejected(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := newTestEngineFromGraph(t, g)
	sess := newSession(eng, NoAuthHandler{}, "")
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}

	// First BEGIN — valid.
	if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("first BEGIN: %v", err)
	}
	if sess.state != StateTxReady {
		t.Fatalf("state after first BEGIN: got %v, want TX_READY", sess.state)
	}

	// Second BEGIN — must be rejected.
	// In TX_READY state BEGIN is not a valid transition → failTransition returns FAILURE.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("second BEGIN HandleMessage returned non-nil error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("second BEGIN response count: %d", len(msgs))
	}
	if _, ok := msgs[0].(*proto.Failure); !ok {
		t.Fatalf("second BEGIN response: %T, want *proto.Failure", msgs[0])
	}
}

// newTestEngineFromGraph creates a cypher.Engine backed by an existing graph.
func newTestEngineFromGraph(t *testing.T, g *lpg.Graph[string, float64]) *cypher.Engine {
	t.Helper()
	return cypher.NewEngine(g)
}
