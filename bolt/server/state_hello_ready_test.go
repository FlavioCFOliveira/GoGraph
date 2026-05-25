package server

import (
	"context"
	"testing"

	"gograph/bolt/proto"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"

	"gograph/cypher"
)

// TestHelloReady_BothOutcomes covers T662 AC1–AC3:
//
//   - Success in NEGOTIATION state → READY (the only target state in this
//     implementation; no separate Authentication state exists).
//   - Failure (bad credentials) → FAILED and a *proto.Failure response.
//   - Race-clean (t.Parallel on every sub-test).
//
// Note: this implementation has no StateAuthentication state.
// HELLO always transitions NEGOTIATION→READY on success.
func TestHelloReady_BothOutcomes(t *testing.T) {
	t.Parallel()

	t.Run("success_negotiation_to_ready", func(t *testing.T) {
		t.Parallel()
		sess := newSession(newTestEngine(t), NoAuthHandler{}, "")
		if sess.state != StateNegotiation {
			t.Fatalf("pre-condition: want NEGOTIATION, got %v", sess.state)
		}

		msgs, err := sess.HandleMessage(context.Background(), helloMsg())
		if err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("response count: got %d, want 1", len(msgs))
		}
		s, ok := msgs[0].(*proto.Success)
		if !ok {
			t.Fatalf("response type: got %T, want *proto.Success", msgs[0])
		}
		if s.Metadata == nil {
			t.Fatal("SUCCESS metadata is nil")
		}
		if _, hasServer := s.Metadata["server"]; !hasServer {
			t.Error("SUCCESS metadata missing 'server' field")
		}
		if sess.state != StateReady {
			t.Fatalf("state after HELLO success: got %v, want READY", sess.state)
		}
	})

	t.Run("failure_wrong_credentials_to_failed", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[string, float64](adjlist.Config{})
		eng := cypher.NewEngine(g)
		auth := BasicAuthHandler{
			Validate: func(_, _ string) error { return ErrAuthFailed },
		}
		sess := newSession(eng, auth, "")

		msgs, err := sess.HandleMessage(context.Background(), &proto.Hello{
			Extra: map[string]interface{}{
				"scheme":      "basic",
				"principal":   "user",
				"credentials": "wrong",
				"agent":       "test/1.0",
			},
		})
		if err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("response count: got %d, want 1", len(msgs))
		}
		f, ok := msgs[0].(*proto.Failure)
		if !ok {
			t.Fatalf("response type: got %T, want *proto.Failure", msgs[0])
		}
		if f.Code != "Neo.ClientError.Security.Unauthorized" {
			t.Errorf("failure code: got %q, want Neo.ClientError.Security.Unauthorized", f.Code)
		}
		// Implementation sets FAILED (not DEFUNCT) on auth failure.
		if sess.state != StateFailed {
			t.Fatalf("state after HELLO failure: got %v, want FAILED", sess.state)
		}
	})

	t.Run("hello_from_non_negotiation_is_illegal", func(t *testing.T) {
		t.Parallel()
		// HELLO in READY state is an illegal transition.
		sess := newSession(newTestEngine(t), NoAuthHandler{}, "")
		// Advance to READY.
		if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
			t.Fatalf("first HELLO: %v", err)
		}
		if sess.state != StateReady {
			t.Fatalf("pre-condition: want READY, got %v", sess.state)
		}

		msgs, err := sess.HandleMessage(context.Background(), helloMsg())
		if err != nil {
			t.Fatalf("second HELLO: %v", err)
		}
		if _, ok := msgs[0].(*proto.Failure); !ok {
			t.Fatalf("expected *proto.Failure, got %T", msgs[0])
		}
		if sess.state != StateFailed {
			t.Fatalf("state after illegal HELLO: got %v, want FAILED", sess.state)
		}
	})
}
