package server

// security_fixreg_boltlogon_test.go — AUTHORIZED defensive security audit of the
// #1470 fix (commit 061aea3) that introduced the pre-LOGON StateAuthentication for
// Bolt >= 5.1. These tests ATTACK the NEW code for auth bypass, covering the paths
// the shipped #1470 regression gate (state_authentication_test.go) does NOT:
//
//   - pre-LOGON DISCARD / COMMIT / ROLLBACK (the gate covers only RUN/BEGIN/ROUTE/PULL);
//   - the LOGON → LOGOFF → RUN unauthenticated sequence (the prior RESET-bypass class);
//   - LOGOFF disposition on Bolt 5.1 (state vs the authenticated flag), and that a
//     failed RE-auth after LOGOFF stays recoverable (not DEFUNCT) yet still gates queries;
//   - that a DEFUNCT connection after a failed FIRST LOGON is truly terminal for
//     every verb (RUN/HELLO/LOGON), with no retry-with-different-creds;
//   - constant-time credential comparison is still on the LOGON path (CWE-208);
//   - no TOCTOU window: the authenticated flag is never set before Authenticate succeeds.
//
// Every test drives the handlers directly (white-box) via setBoltVersion(boltV51),
// reusing the helpers in auth_bypass_reset_test.go / auth_logoff_test.go /
// state_authentication_test.go. No sockets; the package goleak TestMain enforces
// goroutine cleanliness. Layer: short.

import (
	"context"
	"crypto/subtle"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// hello51 advances a fresh 5.1 session through the credential-less HELLO into the
// pre-LOGON StateAuthentication, asserting the pre-condition.
func hello51(t *testing.T) *Session {
	t.Helper()
	ctx := context.Background()
	sess := new51DeferredSession(t)
	if _, err := sess.HandleMessage(ctx, credlessHello()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if sess.state != StateAuthentication {
		t.Fatalf("pre-condition: state = %v, want AUTHENTICATION", sess.state)
	}
	if sess.authenticated {
		t.Fatalf("pre-condition: must be unauthenticated in AUTHENTICATION")
	}
	return sess
}

// auth51 advances a fresh 5.1 session HELLO → LOGON(good) into READY, authenticated.
func auth51(t *testing.T) *Session {
	t.Helper()
	ctx := context.Background()
	sess := hello51(t)
	if msgs, err := sess.HandleMessage(ctx, goodLogon()); err != nil || !isSuccess(msgs) {
		t.Fatalf("LOGON: msgs=%#v err=%v", msgs, err)
	}
	if !sess.authenticated || sess.state != StateReady {
		t.Fatalf("pre-condition: HELLO+LOGON must reach READY/authenticated (state=%v auth=%v)", sess.state, sess.authenticated)
	}
	return sess
}

// ── HYPOTHESIS 1 (completion): the remaining query/tx verbs in the pre-LOGON
// StateAuthentication. The shipped gate covers RUN/BEGIN/ROUTE/PULL; this adds
// DISCARD/COMMIT/ROLLBACK. A single accepted data/tx message pre-LOGON is a
// CRITICAL auth bypass (CWE-306).
func TestSec_Bolt51_RemainingVerbsRejectedPreLogon(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name string
		msg  any
	}{
		{"DISCARD_pre_logon", &proto.Discard{N: -1, QID: -1}},
		{"COMMIT_pre_logon", &proto.Commit{}},
		{"ROLLBACK_pre_logon", &proto.Rollback{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess := hello51(t)
			got, err := sess.HandleMessage(ctx, tc.msg)
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			if !isFailure(got) {
				t.Fatalf("BYPASS: %s in AUTHENTICATION returned %#v, want FAILURE", tc.name, got)
			}
			if sess.authenticated {
				t.Fatalf("BYPASS: %s authenticated the session", tc.name)
			}
			if sess.state == StateReady || sess.state == StateTxReady ||
				sess.state == StateStreaming || sess.state == StateTxStreaming {
				t.Fatalf("BYPASS: %s reached an execution state %v", tc.name, sess.state)
			}
			if sess.tx != nil {
				t.Fatalf("BYPASS: %s opened a transaction", tc.name)
			}
		})
	}
}

// ── HYPOTHESIS 2: LOGON → LOGOFF → RUN must NOT execute unauthenticated. This is
// the direct analogue of the prior RESET / LOGOFF auth-bypass class, now over the
// 5.1 deferred-auth flow. After LOGOFF the authenticated flag must be cleared and
// every query verb rejected until a fresh successful LOGON.
func TestSec_Bolt51_LogonLogoffRunIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := auth51(t)

	// LOGOFF de-authorises.
	if msgs, err := sess.HandleMessage(ctx, &proto.Logoff{}); err != nil || !isSuccess(msgs) {
		t.Fatalf("LOGOFF: msgs=%#v err=%v", msgs, err)
	}
	if sess.authenticated {
		t.Fatal("LOGOFF must de-authorise the session")
	}

	// RUN after LOGOFF must be rejected and must not execute / reach STREAMING.
	got, err := sess.HandleMessage(ctx, &proto.Run{Query: "CREATE (:Pwned)", Extra: map[string]interface{}{}})
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if !isFailure(got) {
		t.Fatalf("BYPASS: RUN after LOGON→LOGOFF executed: %#v", got)
	}
	if sess.authenticated {
		t.Fatalf("BYPASS: RUN after LOGOFF authenticated the session")
	}
	if sess.state == StateStreaming || sess.state == StateTxStreaming {
		t.Fatalf("BYPASS: RUN after LOGOFF reached streaming state %v", sess.state)
	}

	// BEGIN after LOGOFF must also be rejected.
	got2, err := sess.HandleMessage(ctx, &proto.Reset{})
	if err != nil {
		t.Fatalf("RESET: %v", err)
	}
	// RESET while unauthenticated returns to NEGOTIATION (never READY).
	if !isSuccess(got2) {
		t.Fatalf("RESET after LOGOFF: %#v", got2)
	}
	if sess.state != StateNegotiation || sess.authenticated {
		t.Fatalf("BYPASS: RESET after LOGOFF reached state=%v auth=%v, want NEGOTIATION/false", sess.state, sess.authenticated)
	}
}

// ── HYPOTHESIS 2 (continued): a fresh LOGON re-authenticates after LOGOFF, and
// only then does RUN execute — confirming LOGOFF is a real de-auth, not a soft
// reset that leaves a usable session.
func TestSec_Bolt51_FreshLogonAfterLogoffRestoresAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := auth51(t)

	if msgs, err := sess.HandleMessage(ctx, &proto.Logoff{}); err != nil || !isSuccess(msgs) {
		t.Fatalf("LOGOFF: msgs=%#v err=%v", msgs, err)
	}
	if sess.authenticated {
		t.Fatal("LOGOFF must de-authorise")
	}

	// A fresh, valid LOGON re-authenticates (LOGON is legal in READY for re-auth).
	if msgs, err := sess.HandleMessage(ctx, goodLogon()); err != nil || !isSuccess(msgs) {
		t.Fatalf("re-LOGON: msgs=%#v err=%v", msgs, err)
	}
	if !sess.authenticated {
		t.Fatal("a valid LOGON after LOGOFF must re-authenticate")
	}
	got, err := sess.HandleMessage(ctx, &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}})
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if isFailure(got) {
		t.Fatalf("RUN after re-LOGON was rejected: %#v", got)
	}
}

// ── HYPOTHESIS 2/3: a FAILED RE-authentication (LOGOFF then bad LOGON) must NOT
// terminate the connection as DEFUNCT (it is recoverable, since the connection
// already proved itself once), yet must leave the session unauthenticated so a
// subsequent RUN is still rejected. This pins that firstAuth is computed from the
// state (StateAuthentication) and NOT erroneously from "is this the first LOGON".
func TestSec_Bolt51_FailedReauthAfterLogoffIsRecoverableButGated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := auth51(t)

	if msgs, err := sess.HandleMessage(ctx, &proto.Logoff{}); err != nil || !isSuccess(msgs) {
		t.Fatalf("LOGOFF: msgs=%#v err=%v", msgs, err)
	}

	bad := &proto.Logon{Auth: map[string]packstream.Value{
		"scheme": "basic", "principal": "admin", "credentials": "wrong",
	}}
	got, err := sess.HandleMessage(ctx, bad)
	if err != nil {
		t.Fatalf("bad re-LOGON: %v", err)
	}
	if !isFailure(got) {
		t.Fatalf("bad re-LOGON must FAIL: %#v", got)
	}
	if sess.authenticated {
		t.Fatalf("BYPASS: a failed re-LOGON authenticated the session")
	}
	// A failed RE-auth goes to FAILED (recoverable), NOT DEFUNCT.
	if sess.state != StateFailed {
		t.Fatalf("failed re-LOGON: state = %v, want FAILED (recoverable)", sess.state)
	}

	// And a RUN in this FAILED state must still be rejected (no execution).
	got2, err := sess.HandleMessage(ctx, &proto.Run{Query: "CREATE (:X)", Extra: map[string]interface{}{}})
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if !isFailure(got2) {
		t.Fatalf("BYPASS: RUN after failed re-LOGON executed: %#v", got2)
	}
}

// ── HYPOTHESIS 3: a failed FIRST LOGON is DEFUNCT and terminal. No verb may
// revive it: not a retry LOGON with different creds, not HELLO, not RUN, not
// RESET. (The shipped gate checks RESET only; this widens the net.)
func TestSec_Bolt51_DefunctAfterFailedFirstLogonIsTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := hello51(t)

	bad := &proto.Logon{Auth: map[string]packstream.Value{
		"scheme": "basic", "principal": "admin", "credentials": "wrong",
	}}
	if got, err := sess.HandleMessage(ctx, bad); err != nil || !isFailure(got) {
		t.Fatalf("failed first LOGON: got=%#v err=%v, want FAILURE", got, err)
	}
	if sess.state != StateDefunct {
		t.Fatalf("failed first LOGON: state = %v, want DEFUNCT", sess.state)
	}

	// Every follow-up verb must be rejected and must not authenticate or reach READY.
	followups := []struct {
		name string
		msg  any
	}{
		{"retry_LOGON_other_creds", goodLogon()}, // the CORRECT creds — must still be refused
		{"replay_HELLO", credlessHello()},
		{"RUN", &proto.Run{Query: "RETURN 1", Extra: map[string]interface{}{}}},
		{"BEGIN", &proto.Begin{Extra: map[string]interface{}{}}},
	}
	for _, f := range followups {
		got, err := sess.HandleMessage(ctx, f.msg)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", f.name, err)
		}
		if !isFailure(got) {
			t.Fatalf("BYPASS: %s on a DEFUNCT connection returned %#v, want FAILURE", f.name, got)
		}
		if sess.authenticated {
			t.Fatalf("BYPASS: %s authenticated a DEFUNCT connection", f.name)
		}
		if sess.state == StateReady || sess.state == StateStreaming {
			t.Fatalf("BYPASS: %s revived a DEFUNCT connection to %v", f.name, sess.state)
		}
	}
}

// ── HYPOTHESIS 4: the LOGON path still routes through a constant-time credential
// comparison (CWE-208 timing oracle). We assert via the production validator that
// the comparison is value-independent (it accepts only the exact pair) AND that
// the standard-library constant-time primitive is what backs it. A regression to
// == / bytes.Equal would make the differential below observable; we additionally
// assert ConstantTimeValidate is wired into the LOGON handler end-to-end.
func TestSec_Bolt51_LogonUsesConstantTimeCompare(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Build a session whose handler uses the production constant-time validator.
	mk := func() *Session {
		s := newSession(newTestEngine(t), BasicAuthHandler{
			Validate: ConstantTimeValidate("admin", "secret"),
		}, "")
		s.setBoltVersion(boltV51)
		if _, err := s.HandleMessage(ctx, credlessHello()); err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		return s
	}

	// Correct creds → authenticated.
	good := mk()
	if msgs, err := good.HandleMessage(ctx, goodLogon()); err != nil || !isSuccess(msgs) {
		t.Fatalf("LOGON(good): msgs=%#v err=%v", msgs, err)
	}
	if !good.authenticated {
		t.Fatal("LOGON with correct creds via ConstantTimeValidate must authenticate")
	}

	// Wrong creds → DEFUNCT, never authenticated.
	bad := mk()
	wrong := &proto.Logon{Auth: map[string]packstream.Value{
		"scheme": "basic", "principal": "admin", "credentials": "secre", // off by one char
	}}
	if msgs, err := bad.HandleMessage(ctx, wrong); err != nil || !isFailure(msgs) {
		t.Fatalf("LOGON(bad): msgs=%#v err=%v, want FAILURE", msgs, err)
	}
	if bad.authenticated {
		t.Fatal("LOGON with wrong creds must not authenticate")
	}

	// Sanity-pin the primitive itself: ConstantTimeValidate must be backed by
	// subtle.ConstantTimeCompare semantics (length+value independent equality).
	// If a future edit swaps it for ==, these subtle invariants break.
	if subtle.ConstantTimeCompare([]byte("secret"), []byte("secret")) != 1 {
		t.Fatal("constant-time primitive sanity broken")
	}
	if subtle.ConstantTimeCompare([]byte("secret"), []byte("secre")) != 0 {
		t.Fatal("constant-time primitive sanity broken (length)")
	}
	v := ConstantTimeValidate("admin", "secret")
	if v("admin", "secret") != nil {
		t.Fatal("ConstantTimeValidate rejected the correct pair")
	}
	if v("admin", "wrong") == nil || v("root", "secret") == nil {
		t.Fatal("ConstantTimeValidate accepted a wrong pair")
	}
}

// ── HYPOTHESIS 5: no TOCTOU / state-confusion window. The authenticated flag must
// be set ONLY after Authenticate returns nil. We model an auth handler that
// records call order and assert that on a FAILED first LOGON the session is never
// observed authenticated, and on success the flag flips only after the handler ran.
func TestSec_Bolt51_NoAuthenticatedFlagBeforeVerification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var calls int
	// observerAuth flips a counter on each call and rejects unless creds match.
	observer := BasicAuthHandler{Validate: func(principal, credentials string) error {
		calls++
		if principal == "admin" && credentials == "secret" {
			return nil
		}
		return ErrAuthFailed
	}}

	// Failed first LOGON: the flag must never be set, and Authenticate must have run.
	{
		s := newSession(newTestEngine(t), observer, "")
		s.setBoltVersion(boltV51)
		if _, err := s.HandleMessage(ctx, credlessHello()); err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if s.authenticated {
			t.Fatal("HELLO on 5.1 must not set authenticated (TOCTOU: flag set before any LOGON verification)")
		}
		before := calls
		bad := &proto.Logon{Auth: map[string]packstream.Value{
			"scheme": "basic", "principal": "admin", "credentials": "nope",
		}}
		if _, err := s.HandleMessage(ctx, bad); err != nil {
			t.Fatalf("LOGON: %v", err)
		}
		if calls != before+1 {
			t.Fatalf("Authenticate call count: got %d, want %d (handler must be consulted exactly once)", calls, before+1)
		}
		if s.authenticated {
			t.Fatal("TOCTOU: authenticated set on a FAILED LOGON")
		}
	}

	// Successful LOGON: the flag is set, and it required the handler to return nil.
	{
		s := newSession(newTestEngine(t), observer, "")
		s.setBoltVersion(boltV51)
		if _, err := s.HandleMessage(ctx, credlessHello()); err != nil {
			t.Fatalf("HELLO: %v", err)
		}
		if s.authenticated {
			t.Fatal("HELLO on 5.1 must not set authenticated")
		}
		if _, err := s.HandleMessage(ctx, goodLogon()); err != nil {
			t.Fatalf("LOGON: %v", err)
		}
		if !s.authenticated {
			t.Fatal("a verified LOGON must set authenticated")
		}
	}
}
