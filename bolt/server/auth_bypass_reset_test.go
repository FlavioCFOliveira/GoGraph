package server

// auth_bypass_reset_test.go — regression gate for task #1345 (P9/S9):
// authentication bypass via pre-authentication RESET.
//
// Before the fix, Transition treated RESET as a universal transition to READY
// from every non-DEFUNCT state, and no query-bearing handler checked whether
// the connection had authenticated. A network client could therefore send
// RESET as its FIRST message (no HELLO) against a server with a REJECTING auth
// handler and then RUN/BEGIN arbitrary Cypher with no credentials. A failed
// HELLO left the socket open in FAILED, so "failed HELLO → RESET → READY" was a
// second route to the same bypass.
//
// The fix:
//   - a pre-auth RESET returns to NEGOTIATION (never READY);
//   - RUN/BEGIN/ROUTE reject unless Session.authenticated is set;
//   - a failed HELLO makes the connection DEFUNCT.
//
// GATE: every assertion here fails on the unfixed code (a credential-less RESET
// reaches READY and RUN returns records) and passes after the fix.
//
// Layer: short. Goroutine cleanliness is enforced by the package goleak
// TestMain; these tests open no sockets (handlers are driven directly).

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// rejectAllAuthHandler rejects every credential, modelling a server that
// requires real authentication.
func rejectAllAuthHandler() AuthHandler {
	return BasicAuthHandler{Validate: func(_, _ string) error { return ErrAuthFailed }}
}

// acceptAdminAuthHandler accepts only principal="admin"/credentials="secret".
func acceptAdminAuthHandler() AuthHandler {
	return BasicAuthHandler{Validate: func(principal, credentials string) error {
		if principal == "admin" && credentials == "secret" {
			return nil
		}
		return ErrAuthFailed
	}}
}

// isFailure reports whether msgs is exactly one *proto.Failure (a rejection).
func isFailure(msgs []any) bool {
	if len(msgs) != 1 {
		return false
	}
	_, ok := msgs[0].(*proto.Failure)
	return ok
}

// goodHello is a HELLO carrying the admin credentials acceptAdminAuthHandler
// accepts.
func goodHello() *proto.Hello {
	return &proto.Hello{Extra: map[string]interface{}{
		"scheme":      "basic",
		"principal":   "admin",
		"credentials": "secret",
		"agent":       "test/1.0",
	}}
}

// TestAuthBypass_ResetFirstDoesNotGrantReady is the core gate: RESET sent as the
// first message must not authenticate the session or reach READY, and RUN,
// BEGIN, and ROUTE must all be rejected afterwards.
func TestAuthBypass_ResetFirstDoesNotGrantReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// RESET-first against a rejecting handler must leave the session
	// unauthenticated and in NEGOTIATION (never READY).
	sess := newSession(newTestEngine(t), rejectAllAuthHandler(), "")
	if sess.state != StateNegotiation {
		t.Fatalf("pre-condition: fresh session state = %v, want NEGOTIATION", sess.state)
	}
	if sess.authenticated {
		t.Fatal("pre-condition: fresh session must be unauthenticated")
	}

	msgs, err := sess.HandleMessage(ctx, &proto.Reset{})
	if err != nil {
		t.Fatalf("RESET: %v", err)
	}
	if !isSuccess(msgs) {
		t.Fatalf("RESET response: got %#v, want a single SUCCESS", msgs)
	}
	if sess.state == StateReady {
		t.Fatal("BYPASS: RESET as the first message reached READY")
	}
	if sess.state != StateNegotiation {
		t.Fatalf("state after RESET-first: got %v, want NEGOTIATION", sess.state)
	}
	if sess.authenticated {
		t.Fatal("BYPASS: RESET as the first message authenticated the session")
	}

	// Every query-bearing message must now be rejected — on a fresh RESET-first
	// session each time so the rejection is attributable to the auth gate, not
	// to a prior FAILED transition.
	t.Run("RUN_rejected", func(t *testing.T) {
		s := resetFirstSession(t)
		if got, _ := s.HandleMessage(ctx, &proto.Run{Query: "RETURN 42", Extra: map[string]interface{}{}}); !isFailure(got) {
			t.Fatalf("BYPASS: RUN after RESET-first returned %#v, want a FAILURE", got)
		}
		if s.state == StateStreaming {
			t.Fatal("BYPASS: RUN after RESET-first reached STREAMING")
		}
	})
	t.Run("BEGIN_rejected", func(t *testing.T) {
		s := resetFirstSession(t)
		if got, _ := s.HandleMessage(ctx, &proto.Begin{Extra: map[string]interface{}{}}); !isFailure(got) {
			t.Fatalf("BYPASS: BEGIN after RESET-first returned %#v, want a FAILURE", got)
		}
		if s.tx != nil {
			t.Fatal("BYPASS: BEGIN after RESET-first opened a transaction")
		}
	})
	t.Run("ROUTE_rejected", func(t *testing.T) {
		s := resetFirstSession(t)
		got, _ := s.HandleMessage(ctx, &proto.Route{Routing: map[string]packstream.Value{}})
		if !isFailure(got) {
			t.Fatalf("BYPASS: ROUTE after RESET-first returned %#v, want a FAILURE", got)
		}
	})
}

// resetFirstSession returns a session that has received a RESET as its first
// message against a rejecting auth handler (still unauthenticated, NEGOTIATION).
func resetFirstSession(t *testing.T) *Session {
	t.Helper()
	s := newSession(newTestEngine(t), rejectAllAuthHandler(), "")
	if _, err := s.HandleMessage(context.Background(), &proto.Reset{}); err != nil {
		t.Fatalf("RESET-first: %v", err)
	}
	if s.authenticated {
		t.Fatal("resetFirstSession: session unexpectedly authenticated")
	}
	return s
}

// TestAuthBypass_FailedHelloIsDefunct verifies that a failed HELLO terminates
// the connection (DEFUNCT), removing the "failed HELLO → RESET → READY" route.
func TestAuthBypass_FailedHelloIsDefunct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sess := newSession(newTestEngine(t), rejectAllAuthHandler(), "")
	msgs, err := sess.HandleMessage(ctx, goodHello()) // rejected: handler rejects all
	if err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if !isFailure(msgs) {
		t.Fatalf("failed HELLO response: got %#v, want a FAILURE", msgs)
	}
	if sess.state != StateDefunct {
		t.Fatalf("state after failed HELLO: got %v, want DEFUNCT", sess.state)
	}
	if sess.authenticated {
		t.Fatal("failed HELLO must not authenticate the session")
	}

	// The serve loop closes the connection on DEFUNCT, but assert directly that
	// even a RESET after a failed HELLO cannot grant READY or authenticate — the
	// "failed HELLO → RESET → READY" route is closed at the handler level too.
	resetMsgs, err := sess.HandleMessage(ctx, &proto.Reset{})
	if err != nil {
		t.Fatalf("RESET after failed HELLO: %v", err)
	}
	if isSuccess(resetMsgs) {
		t.Fatalf("RESET after failed HELLO returned SUCCESS: %#v", resetMsgs)
	}
	if sess.state == StateReady {
		t.Fatal("BYPASS: RESET after failed HELLO reached READY")
	}
	if sess.authenticated {
		t.Fatal("BYPASS: RESET after failed HELLO authenticated the session")
	}
	if got, _ := sess.HandleMessage(ctx, &proto.Run{Query: "RETURN 42", Extra: map[string]interface{}{}}); !isFailure(got) {
		t.Fatalf("BYPASS: RUN after failed-HELLO+RESET returned %#v, want a FAILURE", got)
	}
}

// TestAuthBypass_GateOpensOnlyAfterHello proves the converse: after a pre-auth
// RESET the gate is closed (RUN rejected), and a subsequent successful HELLO
// authenticates and opens it (RUN succeeds). This guards against an over-broad
// fix that would reject legitimate authenticated clients.
func TestAuthBypass_GateOpensOnlyAfterHello(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sess := newSession(newTestEngine(t), acceptAdminAuthHandler(), "")

	// RESET first → NEGOTIATION, unauthenticated.
	if _, err := sess.HandleMessage(ctx, &proto.Reset{}); err != nil {
		t.Fatalf("RESET: %v", err)
	}
	if sess.authenticated || sess.state != StateNegotiation {
		t.Fatalf("after RESET-first: authenticated=%v state=%v, want false/NEGOTIATION", sess.authenticated, sess.state)
	}

	// RUN before HELLO is rejected.
	if got, _ := sess.HandleMessage(ctx, &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}}); !isFailure(got) {
		t.Fatalf("RUN before HELLO: got %#v, want FAILURE", got)
	}

	// RESET clears FAILED back to NEGOTIATION (still unauthenticated).
	if _, err := sess.HandleMessage(ctx, &proto.Reset{}); err != nil {
		t.Fatalf("RESET after failed RUN: %v", err)
	}
	if sess.state != StateNegotiation {
		t.Fatalf("state after RESET: got %v, want NEGOTIATION", sess.state)
	}

	// A successful HELLO authenticates and reaches READY.
	msgs, err := sess.HandleMessage(ctx, goodHello())
	if err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if !isSuccess(msgs) {
		t.Fatalf("HELLO response: got %#v, want SUCCESS", msgs)
	}
	if !sess.authenticated {
		t.Fatal("HELLO with valid credentials must authenticate the session")
	}
	if sess.state != StateReady {
		t.Fatalf("state after HELLO: got %v, want READY", sess.state)
	}

	// RUN now succeeds (auto-commit stream over an empty graph: zero rows).
	got, err := sess.HandleMessage(ctx, &proto.Run{Query: "MATCH (n) RETURN n", Extra: map[string]interface{}{}})
	if err != nil {
		t.Fatalf("RUN after HELLO: %v", err)
	}
	if isFailure(got) {
		t.Fatalf("RUN after successful HELLO was rejected: %#v", got)
	}
	if sess.state != StateStreaming {
		t.Fatalf("state after RUN: got %v, want STREAMING", sess.state)
	}
}

// isSuccess reports whether msgs is exactly one *proto.Success.
func isSuccess(msgs []any) bool {
	if len(msgs) != 1 {
		return false
	}
	_, ok := msgs[0].(*proto.Success)
	return ok
}
