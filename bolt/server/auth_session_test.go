package server

import (
	"context"
	"testing"

	"gograph/bolt/packstream"
	"gograph/bolt/proto"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// newBasicAuthSession builds a Session backed by a BasicAuthHandler that
// accepts only principal="admin" with credentials="secret".
func newBasicAuthSession(t *testing.T) *Session {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	auth := BasicAuthHandler{
		Validate: func(principal, credentials string) error {
			if principal == "admin" && credentials == "secret" {
				return nil
			}
			return ErrAuthFailed
		},
	}
	return newSession(eng, auth, "")
}

// TestAuth_CorrectCredentials_Ready covers T698 AC1:
// Correct credentials → READY state, no Failure on the wire.
func TestAuth_CorrectCredentials_Ready(t *testing.T) {
	t.Parallel()
	sess := newBasicAuthSession(t)

	msgs, err := sess.HandleMessage(context.Background(), &proto.Hello{
		Extra: map[string]interface{}{
			"scheme":      "basic",
			"principal":   "admin",
			"credentials": "secret",
			"agent":       "test/1.0",
		},
	})
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
	if sess.state != StateReady {
		t.Fatalf("state: got %v, want READY", sess.state)
	}
}

// TestAuth_WrongCredentials_Unauthorized covers T698 AC2–AC3:
//  1. Wrong credentials → *proto.Failure with code "Neo.ClientError.Security.Unauthorized".
//  2. Session state after failure is FAILED (implementation uses FAILED, not DEFUNCT).
//
// Goroutine cleanliness (AC5) is enforced globally by goleak in TestMain.
func TestAuth_WrongCredentials_Unauthorized(t *testing.T) {
	t.Parallel()
	sess := newBasicAuthSession(t)

	msgs, err := sess.HandleMessage(context.Background(), &proto.Hello{
		Extra: map[string]interface{}{
			"scheme":      "basic",
			"principal":   "admin",
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
	// AC2: exact failure code.
	if f.Code != "Neo.ClientError.Security.Unauthorized" {
		t.Errorf("failure code: got %q, want Neo.ClientError.Security.Unauthorized", f.Code)
	}
	// AC3: session is FAILED after the failure.
	if sess.state != StateFailed {
		t.Fatalf("state after auth failure: got %v, want FAILED", sess.state)
	}
}

// TestAuth_SchemeUnknown_FailureCode covers T704 AC1–AC3:
//  1. Unknown auth scheme → *proto.Failure with "Neo.ClientError.Security.AuthProviderFailed".
//  2. Session state is FAILED after the failure.
//  3. Wire response is exactly one Failure (no subsequent messages at the
//     session-handler level; connection close is the transport layer's job).
func TestAuth_SchemeUnknown_FailureCode(t *testing.T) {
	t.Parallel()

	t.Run("hello_with_unknown_scheme", func(t *testing.T) {
		t.Parallel()
		sess := newBasicAuthSession(t)

		msgs, err := sess.HandleMessage(context.Background(), &proto.Hello{
			Extra: map[string]interface{}{
				"scheme":      "kerberos",
				"principal":   "user",
				"credentials": "token",
				"agent":       "test/1.0",
			},
		})
		if err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
		// AC3: exactly one message.
		if len(msgs) != 1 {
			t.Fatalf("response count: got %d, want 1", len(msgs))
		}
		f, ok := msgs[0].(*proto.Failure)
		if !ok {
			t.Fatalf("response type: got %T, want *proto.Failure", msgs[0])
		}
		// AC1: exact failure code.
		if f.Code != "Neo.ClientError.Security.AuthProviderFailed" {
			t.Errorf("failure code: got %q, want Neo.ClientError.Security.AuthProviderFailed", f.Code)
		}
		// AC2: session is FAILED.
		if sess.state != StateFailed {
			t.Fatalf("state: got %v, want FAILED", sess.state)
		}
	})

	t.Run("logon_with_unknown_scheme", func(t *testing.T) {
		t.Parallel()
		sess := newBasicAuthSession(t)
		// Advance to READY first (with correct credentials for HELLO).
		if _, err := sess.HandleMessage(context.Background(), &proto.Hello{
			Extra: map[string]interface{}{
				"scheme":      "basic",
				"principal":   "admin",
				"credentials": "secret",
				"agent":       "test/1.0",
			},
		}); err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if sess.state != StateReady {
			t.Fatalf("pre-condition: want READY, got %v", sess.state)
		}

		// LOGON with unknown scheme.
		msgs, err := sess.HandleMessage(context.Background(), &proto.Logon{
			Auth: map[string]packstream.Value{
				"scheme":      "bearer",
				"principal":   "user",
				"credentials": "tok_xyz",
			},
		})
		if err != nil {
			t.Fatalf("Logon: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("response count: got %d, want 1", len(msgs))
		}
		f, ok := msgs[0].(*proto.Failure)
		if !ok {
			t.Fatalf("response type: got %T, want *proto.Failure", msgs[0])
		}
		if f.Code != "Neo.ClientError.Security.AuthProviderFailed" {
			t.Errorf("failure code: got %q, want Neo.ClientError.Security.AuthProviderFailed", f.Code)
		}
		if sess.state != StateFailed {
			t.Fatalf("state: got %v, want FAILED", sess.state)
		}
	})
}
