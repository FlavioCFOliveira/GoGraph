package server

// security_preauth_statemachine_test.go — DEFENSE LOCK-IN for the Bolt
// pre-authentication state machine (security audit, Bolt/protocol cluster).
//
// The authoritative authentication gate is split across two layers:
//
//   - the transport state machine ([Transition]) refuses application messages
//     in CONNECTED/NEGOTIATION and never mints READY out of the pre-HELLO
//     phase (task #1345); and
//   - the session handlers ([Session.handleRun]/handleBegin/handleRoute) reject
//     unless [Session.authenticated] is set.
//
// The existing auth_bypass_reset_test.go / auth_logoff_test.go pin the
// RESET-first and LOGOFF routes specifically. This file pins the complementary
// surface those tests do not cover directly: a fresh, never-authenticated
// NEGOTIATION session that sends each query-bearing verb (RUN, BEGIN, ROUTE,
// PULL) as its FIRST message must be rejected — without reaching READY,
// STREAMING, or TX_READY, and without opening a transaction. Driving the
// handlers directly (white-box) lets the test inject states a real wire client
// cannot, so the gate is asserted at its source.
//
// These assertions pass on the current (fixed) code; they exist to prevent a
// regression that would re-open the pre-auth bypass. Layer: short; the package
// goleak TestMain enforces goroutine cleanliness and these tests open no
// sockets.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// secBoltFreshSession returns a brand-new session against a rejecting handler.
// It has performed no HELLO/LOGON, so it is unauthenticated and in NEGOTIATION
// — the exact pre-auth posture a network peer holds before its first message.
func secBoltFreshSession(t *testing.T) *Session {
	t.Helper()
	s := newSession(newTestEngine(t), rejectAllAuthHandler(), "")
	if s.state != StateNegotiation {
		t.Fatalf("pre-condition: fresh session state = %v, want NEGOTIATION", s.state)
	}
	if s.authenticated {
		t.Fatal("pre-condition: fresh session must be unauthenticated")
	}
	return s
}

// TestSec_Bolt_PreAuthQueryVerbsRejected is the unified gate: every
// query-bearing verb sent as the first message on an unauthenticated session is
// repelled with a FAILURE and never advances the connection into an execution
// state.
func TestSec_Bolt_PreAuthQueryVerbsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name string
		msg  any
		// forbidState is a state the rejected message must NOT have reached.
		forbidState State
	}{
		{"RUN_before_auth", &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}}, StateStreaming},
		{"BEGIN_before_auth", &proto.Begin{Extra: map[string]interface{}{}}, StateTxReady},
		{"ROUTE_before_auth", &proto.Route{Routing: map[string]packstream.Value{}}, StateReady},
		// PULL is doubly illegal pre-auth: there is no prior RUN, and the
		// session is unauthenticated. Either way it must be refused.
		{"PULL_before_auth", &proto.Pull{N: -1, QID: -1}, StateStreaming},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := secBoltFreshSession(t)

			got, err := s.HandleMessage(ctx, tc.msg)
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			if !isFailure(got) {
				t.Fatalf("BYPASS: %s on an unauthenticated session returned %#v, want a FAILURE", tc.name, got)
			}
			if s.authenticated {
				t.Fatalf("BYPASS: %s authenticated the session", tc.name)
			}
			if s.state == StateReady {
				t.Fatalf("BYPASS: %s reached READY", tc.name)
			}
			if s.state == tc.forbidState {
				t.Fatalf("BYPASS: %s reached forbidden state %v", tc.name, tc.forbidState)
			}
			if s.tx != nil {
				t.Fatalf("BYPASS: %s opened a transaction", tc.name)
			}
		})
	}
}

// TestSec_Bolt_LogonBeforeHelloRejected pins that LOGON is not a substitute for
// HELLO: a fresh NEGOTIATION session cannot authenticate by sending LOGON
// first. LOGON is legal only in READY/TX_READY (post-HELLO), so it must be
// refused and must not authenticate the session.
func TestSec_Bolt_LogonBeforeHelloRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := newSession(newTestEngine(t), acceptAdminAuthHandler(), "")
	if s.state != StateNegotiation {
		t.Fatalf("pre-condition: state = %v, want NEGOTIATION", s.state)
	}

	got, err := s.HandleMessage(ctx, goodLogon())
	if err != nil {
		t.Fatalf("LOGON-first: unexpected error %v", err)
	}
	if !isFailure(got) {
		t.Fatalf("BYPASS: LOGON before HELLO returned %#v, want a FAILURE", got)
	}
	if s.authenticated {
		t.Fatal("BYPASS: LOGON before HELLO authenticated the session")
	}
	if s.state == StateReady {
		t.Fatal("BYPASS: LOGON before HELLO reached READY")
	}
}

// TestSec_Bolt_FailedHelloThenReauthRouteClosed proves the failed-HELLO →
// DEFUNCT route cannot be reopened by any follow-up verb. After a rejected
// HELLO the connection is DEFUNCT; RESET, LOGON, BEGIN, ROUTE, and RUN must all
// fail to grant READY or authenticate. This complements
// TestAuthBypass_FailedHelloIsDefunct (which checks RESET then RUN) with the
// full verb set, so a future change that makes any single verb escape DEFUNCT
// is caught.
func TestSec_Bolt_FailedHelloThenReauthRouteClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	verbs := []struct {
		name string
		msg  any
	}{
		{"RESET", &proto.Reset{}},
		{"LOGON", goodLogon()},
		{"BEGIN", &proto.Begin{Extra: map[string]interface{}{}}},
		{"ROUTE", &proto.Route{Routing: map[string]packstream.Value{}}},
		{"RUN", &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}}},
	}

	for _, v := range verbs {
		t.Run(v.name+"_after_failed_hello", func(t *testing.T) {
			t.Parallel()
			// rejectAllAuthHandler makes HELLO fail → DEFUNCT.
			s := newSession(newTestEngine(t), rejectAllAuthHandler(), "")
			if msgs, err := s.HandleMessage(ctx, goodHello()); err != nil || !isFailure(msgs) {
				t.Fatalf("pre-condition failed HELLO: msgs=%#v err=%v", msgs, err)
			}
			if s.state != StateDefunct {
				t.Fatalf("pre-condition: state after failed HELLO = %v, want DEFUNCT", s.state)
			}

			got, err := s.HandleMessage(ctx, v.msg)
			if err != nil {
				t.Fatalf("%s after failed HELLO: unexpected error %v", v.name, err)
			}
			if isSuccess(got) {
				t.Fatalf("BYPASS: %s after failed HELLO returned SUCCESS: %#v", v.name, got)
			}
			if s.authenticated {
				t.Fatalf("BYPASS: %s after failed HELLO authenticated the session", v.name)
			}
			if s.state == StateReady {
				t.Fatalf("BYPASS: %s after failed HELLO reached READY", v.name)
			}
		})
	}
}
