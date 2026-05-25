package server

import (
	"context"
	"testing"

	"gograph/bolt/proto"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newTwoNodeEngine builds a Cypher engine backed by a graph with two nodes so
// that "MATCH (n) RETURN n" yields exactly 2 rows — enough to exercise
// paginated PULL with has_more=true then has_more=false.
func newTwoNodeEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode alice: %v", err)
	}
	if err := g.AddNode("bob"); err != nil {
		t.Fatalf("AddNode bob: %v", err)
	}
	return cypher.NewEngine(g)
}

// TestStreaming_HasMoreKeepsStreaming covers T680 AC2:
// Pull with has_more=true must keep the session in STREAMING.
//
// The paginated case (n=1 on a 2-row result) already exercises this in
// TestSession_Pull_Paginated; this test makes the state assertion explicit and
// isolated so the AC is traceable.
func TestStreaming_HasMoreKeepsStreaming(t *testing.T) {
	t.Parallel()
	sess := newSession(newTwoNodeEngine(t), NoAuthHandler{}, "")

	// HELLO → READY.
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	// RUN → STREAMING.
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query: "MATCH (n) RETURN n",
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if sess.state != StateStreaming {
		t.Fatalf("pre-condition: want STREAMING, got %v", sess.state)
	}

	// PULL n=1 → has_more=true, state must remain STREAMING.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Pull{N: 1, QID: -1})
	if err != nil {
		t.Fatalf("PULL n=1: %v", err)
	}
	last, ok := msgs[len(msgs)-1].(*proto.Success)
	if !ok {
		t.Fatalf("last message type: %T, want *proto.Success", msgs[len(msgs)-1])
	}
	hasMore, ok := last.Metadata["has_more"]
	if !ok {
		t.Fatal("PULL SUCCESS missing 'has_more'")
	}
	if hasMore != true {
		t.Fatalf("has_more after partial PULL: got %v, want true", hasMore)
	}
	if sess.state != StateStreaming {
		t.Fatalf("state after partial PULL: got %v, want STREAMING", sess.state)
	}
}

// TestStreaming_HasMoreFalseToReady covers T680 AC3:
// Pull with has_more=false (stream exhausted) must transition to READY.
//
// Explicit test to make the AC traceable independently of TestSession_RunPullReady.
func TestStreaming_HasMoreFalseToReady(t *testing.T) {
	t.Parallel()
	sess := newSession(newTwoNodeEngine(t), NoAuthHandler{}, "")

	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query: "MATCH (n) RETURN n",
		Extra: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("RUN: %v", err)
	}

	// PULL all → has_more=false, state → READY.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("PULL all: %v", err)
	}
	last, ok := msgs[len(msgs)-1].(*proto.Success)
	if !ok {
		t.Fatalf("last message type: %T, want *proto.Success", msgs[len(msgs)-1])
	}
	hasMore, ok := last.Metadata["has_more"]
	if !ok {
		t.Fatal("PULL SUCCESS missing 'has_more'")
	}
	if hasMore != false {
		t.Fatalf("has_more after full PULL: got %v, want false", hasMore)
	}
	if sess.state != StateReady {
		t.Fatalf("state after full PULL: got %v, want READY", sess.state)
	}
}

// TestStreaming_PullFromNonStreaming covers T680 AC4:
// PULL from a non-STREAMING state returns a *proto.Failure and the session
// moves to FAILED.
func TestStreaming_PullFromNonStreaming(t *testing.T) {
	t.Parallel()

	states := []struct {
		name  string
		setup func(t *testing.T) *Session
	}{
		{
			name: "from_ready",
			setup: func(t *testing.T) *Session {
				t.Helper()
				return newReadySession(t)
			},
		},
		{
			name: "from_negotiation",
			setup: func(t *testing.T) *Session {
				t.Helper()
				return newSession(newTestEngine(t), NoAuthHandler{}, "")
			},
		},
		{
			name: "from_tx_ready",
			setup: func(t *testing.T) *Session {
				t.Helper()
				sess := newReadySession(t)
				if _, err := sess.HandleMessage(context.Background(), &proto.Begin{
					Extra: map[string]interface{}{},
				}); err != nil {
					t.Fatalf("BEGIN: %v", err)
				}
				if sess.state != StateTxReady {
					t.Fatalf("pre-condition: want TX_READY, got %v", sess.state)
				}
				return sess
			},
		},
	}

	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess := tc.setup(t)
			msgs, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1})
			if err != nil {
				t.Fatalf("HandleMessage: %v", err)
			}
			if len(msgs) != 1 {
				t.Fatalf("response count: got %d, want 1", len(msgs))
			}
			if _, ok := msgs[0].(*proto.Failure); !ok {
				t.Fatalf("expected *proto.Failure, got %T", msgs[0])
			}
			if sess.state != StateFailed {
				t.Fatalf("state: got %v, want FAILED", sess.state)
			}
		})
	}
}
