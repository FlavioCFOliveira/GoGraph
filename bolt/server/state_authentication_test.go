package server

// state_authentication_test.go — REGRESSION GATE for task #1470: the Bolt >= 5.1
// deferred-authentication flow and the StateAuthentication pre-LOGON state.
//
// Bolt 5.1 split authentication out of HELLO into a dedicated LOGON message. A
// 5.1+ driver sends a credential-less HELLO (carrying only driver metadata) then
// a LOGON carrying scheme/principal/credentials. The fix gates HELLO
// authentication on the negotiated Bolt version:
//
//   - Bolt <= 5.0 (and the zero-value version used by the other white-box auth
//     tests): HELLO authenticates inline and advances NEGOTIATION → READY.
//   - Bolt >= 5.1: HELLO does NOT authenticate, does NOT set authenticated, and
//     advances NEGOTIATION → StateAuthentication; only LOGON/LOGOFF/RESET/GOODBYE
//     are legal there, and a successful LOGON authenticates and reaches READY.
//
// These assertions drive the handlers directly (white-box) with an explicit
// negotiated version via setBoltVersion, so the version gate is pinned at its
// source. They complement security_basicauth_logon_test.go, which exercises the
// same flow end-to-end through the real neo4j-go-driver.
//
// Layer: short. The package goleak TestMain enforces goroutine cleanliness;
// these tests open no sockets.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// boltV51 is the lowest Bolt version that defers authentication to LOGON.
var boltV51 = proto.Version{Major: 5, Minor: 1}

// new51DeferredSession returns a fresh session negotiated at Bolt 5.1 against an
// admin-only handler, in NEGOTIATION and unauthenticated.
func new51DeferredSession(t *testing.T) *Session {
	t.Helper()
	s := newSession(newTestEngine(t), acceptAdminAuthHandler(), "")
	s.setBoltVersion(boltV51)
	if s.state != StateNegotiation {
		t.Fatalf("pre-condition: state = %v, want NEGOTIATION", s.state)
	}
	if s.authenticated {
		t.Fatal("pre-condition: fresh session must be unauthenticated")
	}
	return s
}

// credlessHello is the credential-less HELLO a Bolt 5.1+ driver sends (only
// driver metadata, no scheme/principal/credentials).
func credlessHello() *proto.Hello {
	return &proto.Hello{Extra: map[string]interface{}{
		"user_agent": "neo4j-go/5.0",
	}}
}

// TestSec_Bolt51_HelloDefersAuthToLogon is the core gate: on Bolt >= 5.1 a
// credential-less HELLO succeeds WITHOUT authenticating and lands in
// StateAuthentication; a subsequent LOGON with correct credentials authenticates
// and reaches READY, after which a query runs.
func TestSec_Bolt51_HelloDefersAuthToLogon(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := new51DeferredSession(t)

	// Credential-less HELLO: SUCCESS, but the connection is NOT yet authenticated
	// and sits in the pre-LOGON StateAuthentication.
	msgs, err := sess.HandleMessage(ctx, credlessHello())
	if err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if !isSuccess(msgs) {
		t.Fatalf("credential-less HELLO on 5.1: got %#v, want SUCCESS", msgs)
	}
	if sess.state != StateAuthentication {
		t.Fatalf("state after 5.1 HELLO: got %v, want AUTHENTICATION", sess.state)
	}
	if sess.authenticated {
		t.Fatal("5.1 HELLO must NOT authenticate the session (auth is deferred to LOGON)")
	}

	// LOGON with the correct credentials authenticates and advances to READY.
	logonMsgs, err := sess.HandleMessage(ctx, goodLogon())
	if err != nil {
		t.Fatalf("LOGON: %v", err)
	}
	if !isSuccess(logonMsgs) {
		t.Fatalf("LOGON after 5.1 HELLO: got %#v, want SUCCESS", logonMsgs)
	}
	if !sess.authenticated {
		t.Fatal("LOGON with valid credentials must authenticate the session")
	}
	if sess.state != StateReady {
		t.Fatalf("state after LOGON: got %v, want READY", sess.state)
	}

	// A query now runs on the authenticated connection.
	runMsgs, err := sess.HandleMessage(ctx, &proto.Run{Query: "MATCH (n) RETURN n", Extra: map[string]interface{}{}})
	if err != nil {
		t.Fatalf("RUN after LOGON: %v", err)
	}
	if isFailure(runMsgs) {
		t.Fatalf("RUN after a successful 5.1 LOGON was rejected: %#v", runMsgs)
	}
	if sess.state != StateStreaming {
		t.Fatalf("state after RUN: got %v, want STREAMING", sess.state)
	}
}

// TestSec_Bolt51_QueryVerbsRejectedInAuthentication pins that the pre-LOGON
// StateAuthentication is not a usable state: every query-bearing verb sent
// before LOGON is rejected and must not authenticate the session, reach an
// execution state, or open a transaction.
func TestSec_Bolt51_QueryVerbsRejectedInAuthentication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name        string
		msg         any
		forbidState State
	}{
		{"RUN_before_logon", &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}}, StateStreaming},
		{"BEGIN_before_logon", &proto.Begin{Extra: map[string]interface{}{}}, StateTxReady},
		{"ROUTE_before_logon", &proto.Route{Routing: map[string]packstream.Value{}}, StateReady},
		{"PULL_before_logon", &proto.Pull{N: -1, QID: -1}, StateStreaming},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess := new51DeferredSession(t)
			if _, err := sess.HandleMessage(ctx, credlessHello()); err != nil {
				t.Fatalf("HELLO: %v", err)
			}
			if sess.state != StateAuthentication {
				t.Fatalf("pre-condition: state = %v, want AUTHENTICATION", sess.state)
			}

			got, err := sess.HandleMessage(ctx, tc.msg)
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			if !isFailure(got) {
				t.Fatalf("BYPASS: %s in AUTHENTICATION returned %#v, want a FAILURE", tc.name, got)
			}
			if sess.authenticated {
				t.Fatalf("BYPASS: %s authenticated the session", tc.name)
			}
			if sess.state == StateReady {
				t.Fatalf("BYPASS: %s reached READY", tc.name)
			}
			if sess.state == tc.forbidState {
				t.Fatalf("BYPASS: %s reached forbidden state %v", tc.name, tc.forbidState)
			}
			if sess.tx != nil {
				t.Fatalf("BYPASS: %s opened a transaction", tc.name)
			}
		})
	}
}

// TestSec_Bolt51_FailedFirstLogonIsDefunct pins the disposition of a FAILED
// first authentication on Bolt >= 5.1: a LOGON with wrong credentials from
// StateAuthentication terminates the connection (DEFUNCT) — exactly like a failed
// <= 5.0 HELLO — and the FAILURE carries the Unauthorized code. The connection is
// not recoverable via RESET (it never reached an authenticated state).
func TestSec_Bolt51_FailedFirstLogonIsDefunct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := new51DeferredSession(t)

	if _, err := sess.HandleMessage(ctx, credlessHello()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if sess.state != StateAuthentication {
		t.Fatalf("pre-condition: state = %v, want AUTHENTICATION", sess.state)
	}

	// LOGON with WRONG credentials.
	badLogon := &proto.Logon{Auth: map[string]packstream.Value{
		"scheme":      "basic",
		"principal":   "admin",
		"credentials": "the-wrong-password",
	}}
	msgs, err := sess.HandleMessage(ctx, badLogon)
	if err != nil {
		t.Fatalf("LOGON: %v", err)
	}
	f, ok := msgs[0].(*proto.Failure)
	if !ok || len(msgs) != 1 {
		t.Fatalf("failed first LOGON: got %#v, want a single FAILURE", msgs)
	}
	if f.Code != "Neo.ClientError.Security.Unauthorized" {
		t.Errorf("failure code: got %q, want Neo.ClientError.Security.Unauthorized", f.Code)
	}
	if sess.authenticated {
		t.Fatal("failed first LOGON must not authenticate the session")
	}
	if sess.state != StateDefunct {
		t.Fatalf("state after failed first LOGON: got %v, want DEFUNCT", sess.state)
	}

	// Even a RESET after a failed first LOGON cannot grant READY or authenticate.
	resetMsgs, err := sess.HandleMessage(ctx, &proto.Reset{})
	if err != nil {
		t.Fatalf("RESET after failed first LOGON: %v", err)
	}
	if isSuccess(resetMsgs) {
		t.Fatalf("RESET after failed first LOGON returned SUCCESS: %#v", resetMsgs)
	}
	if sess.state == StateReady || sess.authenticated {
		t.Fatalf("BYPASS: RESET after failed first LOGON reached READY/authenticated (state=%v auth=%v)", sess.state, sess.authenticated)
	}
}

// TestSec_Bolt51_PreLogonResetReturnsToNegotiation pins that a RESET in the
// pre-LOGON StateAuthentication returns to NEGOTIATION (never READY) and leaves
// the session unauthenticated — the same secure pre-auth RESET behaviour as a
// pre-HELLO RESET (task #1345), now also covering the 5.1 pre-LOGON window.
func TestSec_Bolt51_PreLogonResetReturnsToNegotiation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := new51DeferredSession(t)

	if _, err := sess.HandleMessage(ctx, credlessHello()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if sess.state != StateAuthentication {
		t.Fatalf("pre-condition: state = %v, want AUTHENTICATION", sess.state)
	}

	msgs, err := sess.HandleMessage(ctx, &proto.Reset{})
	if err != nil {
		t.Fatalf("RESET: %v", err)
	}
	if !isSuccess(msgs) {
		t.Fatalf("RESET in AUTHENTICATION: got %#v, want SUCCESS", msgs)
	}
	if sess.state != StateNegotiation {
		t.Fatalf("state after pre-LOGON RESET: got %v, want NEGOTIATION", sess.state)
	}
	if sess.authenticated {
		t.Fatal("pre-LOGON RESET must not authenticate the session")
	}
}

// TestSec_Bolt50_HelloStillAuthenticatesInline is the converse pin: on Bolt <= 5.0
// (modelled here at the zero-value version, exactly like the other white-box auth
// tests) the inline-HELLO flow is preserved — HELLO authenticates and advances
// straight to READY, never entering StateAuthentication.
func TestSec_Bolt50_HelloStillAuthenticatesInline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("zero_version_inline_auth", func(t *testing.T) {
		t.Parallel()
		// newSession leaves boltVersion at the zero value (< 5.1), the same posture
		// the existing white-box auth tests rely on.
		sess := newSession(newTestEngine(t), acceptAdminAuthHandler(), "")
		msgs, err := sess.HandleMessage(ctx, goodHello())
		if err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if !isSuccess(msgs) {
			t.Fatalf("HELLO on <= 5.0: got %#v, want SUCCESS", msgs)
		}
		if sess.state != StateReady {
			t.Fatalf("state after <= 5.0 HELLO: got %v, want READY", sess.state)
		}
		if !sess.authenticated {
			t.Fatal("HELLO on <= 5.0 with valid credentials must authenticate inline")
		}
	})

	t.Run("explicit_v50_inline_auth", func(t *testing.T) {
		t.Parallel()
		sess := newSession(newTestEngine(t), acceptAdminAuthHandler(), "")
		sess.setBoltVersion(proto.Version{Major: 5, Minor: 0})
		msgs, err := sess.HandleMessage(ctx, goodHello())
		if err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if !isSuccess(msgs) {
			t.Fatalf("HELLO on 5.0: got %#v, want SUCCESS", msgs)
		}
		if sess.state != StateReady {
			t.Fatalf("state after 5.0 HELLO: got %v, want READY", sess.state)
		}
		if !sess.authenticated {
			t.Fatal("HELLO on 5.0 with valid credentials must authenticate inline")
		}
	})
}
