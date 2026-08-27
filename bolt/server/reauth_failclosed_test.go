package server

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// newAuthenticatedSession returns a READY session authenticated against a handler
// that ACCEPTS ONLY admin/secret.
//
// It does not reuse newReadySession, which installs NoAuthHandler{} and therefore
// accepts every credential: every assertion about a REFUSED authentication would
// be vacuous against it, and the first version of this test duly reported that a
// wrong-credential LOGON had been accepted.
func newAuthenticatedSession(t *testing.T) *Session {
	t.Helper()
	s := newSession(newTestEngine(t), acceptAdminAuthHandler(), "")
	if _, err := s.HandleMessage(context.Background(), goodHello()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if s.state != StateReady {
		t.Fatalf("state after HELLO: got %v, want READY", s.state)
	}
	if !s.authenticated {
		t.Fatalf("HELLO with accepted credentials did not authenticate the session")
	}
	return s
}

// logonAs sends a LOGON with the given credentials and returns the reply.
func logonAs(t *testing.T, s *Session, principal, credentials string) []any {
	t.Helper()
	msgs, err := s.HandleMessage(context.Background(), &proto.Logon{Auth: map[string]interface{}{
		"scheme":      "basic",
		"principal":   principal,
		"credentials": credentials,
	}})
	if err != nil {
		t.Fatalf("LOGON(%s): %v", principal, err)
	}
	return msgs
}

// TestReauth_FailedLogonTerminatesTheConnection guards rmp #2556.
//
// The non-firstAuth failure branch was the only exit from handleLogon that set
// neither s.identity nor s.authenticated — the assignments sit after the error
// return — so a REFUSED identity switch changed nothing and the connection went
// on as the PREVIOUS principal. handleReset then took the authenticated path
// back to READY with full write capability. Measured end to end over a real
// socket before the fix: LOGON(alice, ok), LOGON(bob, WRONG) -> FAILURE, RESET,
// CREATE (:Ghost) -> SUCCESS, nodes-created 1.
//
// The identity is security-relevant even without roles: it keys the
// per-principal transaction quota and the SHOW TRANSACTIONS attribution, so a
// refused switch left both pointing at the wrong principal.
//
// The contract is now the one the Bolt specification states for LOGON — a failed
// authentication closes the connection — with no carve-out for
// re-authentication.
func TestReauth_FailedLogonTerminatesTheConnection(t *testing.T) {
	t.Parallel()
	s := newAuthenticatedSession(t)

	// Precondition: the session really is authenticated and in READY, or the
	// assertions below would be about the wrong branch entirely.
	if !s.authenticated {
		t.Fatalf("precondition: the session is not authenticated")
	}
	if s.state != StateReady {
		t.Fatalf("precondition: state = %v, want READY", s.state)
	}
	before := s.identity

	// A REFUSED re-authentication. No LOGOFF first — that is the whole point: the
	// official driver always sends one, so the branch where authenticated is TRUE
	// on entry was reachable only by a non-conforming client, which is exactly an
	// attacker's shape.
	msgs := logonAs(t, s, "nobody", "wrong-password")
	if f := failureOf(msgs); f == nil {
		t.Fatalf("a wrong-credential LOGON returned %#v, want a FAILURE", msgs)
	}

	if s.state != StateDefunct {
		t.Errorf("state after a failed re-authentication = %v, want DEFUNCT. A recoverable "+
			"state lets RESET return the connection to READY as the PREVIOUS principal "+
			"(rmp #2556)", s.state)
	}
	if s.authenticated {
		t.Errorf("the session is still authenticated after a REFUSED identity switch")
	}
	if s.identity == before && before != (Identity{}) {
		t.Errorf("the session still carries the previous identity %+v after a refused switch: "+
			"it keys the transaction quota and the SHOW TRANSACTIONS attribution", s.identity)
	}
}

// TestReauth_ResetCannotRecoverAWriteAfterAFailedLogon reproduces the PoC's
// decisive step: the write that used to succeed.
//
// Asserting the state alone would not settle it — the question a reader has is
// whether the graph can still be written, so that is what is asserted.
func TestReauth_ResetCannotRecoverAWriteAfterAFailedLogon(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newAuthenticatedSession(t)

	if f := failureOf(logonAs(t, s, "nobody", "wrong-password")); f == nil {
		t.Fatalf("a wrong-credential LOGON was accepted")
	}

	// RESET is where the recovery used to happen.
	if _, err := s.HandleMessage(ctx, &proto.Reset{}); err != nil {
		t.Logf("RESET on a DEFUNCT session returned %v (acceptable: the connection is gone)", err)
	}
	if s.state == StateReady && s.authenticated {
		t.Fatalf("RESET returned the connection to READY and authenticated after a REFUSED " +
			"re-authentication: this is the recovery rmp #2556 closes")
	}

	// And the write must not land.
	msgs, err := s.HandleMessage(ctx, &proto.Run{Query: "CREATE (:Ghost)", Extra: map[string]interface{}{}})
	if err == nil && failureOf(msgs) == nil {
		// Not a FAILURE and not an error — check it is at least not a SUCCESS that
		// wrote something.
		for _, m := range msgs {
			if _, ok := m.(*proto.Success); ok {
				t.Fatalf("CREATE was accepted after a refused re-authentication: the connection "+
					"still writes as the previous principal (rmp #2556). Reply: %#v", msgs)
			}
		}
	}
}

// TestReauth_SuccessfulReauthenticationStillWorks is the control the negatives
// need: a guard that terminated the connection on EVERY LogOn would satisfy both
// tests above while breaking re-authentication outright.
func TestReauth_SuccessfulReauthenticationStillWorks(t *testing.T) {
	t.Parallel()
	s := newAuthenticatedSession(t)

	// The same credentials newReadySession used must be accepted again.
	msgs := logonAs(t, s, "admin", "secret")
	if f := failureOf(msgs); f != nil {
		t.Fatalf("a CORRECT re-authentication was refused (%s: %s); re-authentication must "+
			"still work", f.Code, f.Message)
	}
	if s.state == StateDefunct {
		t.Errorf("a successful re-authentication terminated the connection")
	}
	if !s.authenticated {
		t.Errorf("a successful re-authentication left the session unauthenticated")
	}
}
