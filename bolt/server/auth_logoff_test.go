package server

// auth_logoff_test.go — regression gate for task #1347 (P5/S5):
// LOGOFF did not de-authorize the connection. handleLogoff cleared the identity
// but kept the session in READY/TX_READY, and the query-bearing handlers never
// re-checked authentication, so RUN/BEGIN were still accepted after LOGOFF with
// no principal bound. Bolt semantics require a fresh LOGON before further work.
//
// The fix (shared with task #1345) clears Session.authenticated on LOGOFF and
// gates handleRun/handleBegin/handleRoute on it.
//
// GATE: RUN after LOGOFF is rejected, and a fresh LOGON re-authenticates so RUN
// works again. Fails on the unfixed code (RUN after LOGOFF executed).
//
// Layer: short. Goroutine cleanliness is enforced by the package goleak
// TestMain; this test drives handlers directly and opens no sockets.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// goodLogon is a LOGON carrying the admin credentials acceptAdminAuthHandler
// accepts (re-authentication after LOGOFF).
func goodLogon() *proto.Logon {
	return &proto.Logon{Auth: map[string]packstream.Value{
		"scheme":      "basic",
		"principal":   "admin",
		"credentials": "secret",
	}}
}

// TestLogoff_DeauthorizesUntilFreshLogon is the regression gate for #1347.
func TestLogoff_DeauthorizesUntilFreshLogon(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("run_after_logoff_is_rejected", func(t *testing.T) {
		t.Parallel()
		sess := newSession(newTestEngine(t), acceptAdminAuthHandler(), "")

		// Authenticate, then LOGOFF.
		if msgs, err := sess.HandleMessage(ctx, goodHello()); err != nil || !isSuccess(msgs) {
			t.Fatalf("HELLO: msgs=%#v err=%v", msgs, err)
		}
		if !sess.authenticated {
			t.Fatal("pre-condition: HELLO must authenticate the session")
		}
		if msgs, err := sess.HandleMessage(ctx, &proto.Logoff{}); err != nil || !isSuccess(msgs) {
			t.Fatalf("LOGOFF: msgs=%#v err=%v", msgs, err)
		}
		if sess.authenticated {
			t.Fatal("LOGOFF must de-authorize the session")
		}

		// RUN after LOGOFF must be rejected (no execution).
		got, err := sess.HandleMessage(ctx, &proto.Run{Query: "CREATE (:X)", Extra: map[string]interface{}{}})
		if err != nil {
			t.Fatalf("RUN after LOGOFF: %v", err)
		}
		if !isFailure(got) {
			t.Fatalf("RUN after LOGOFF returned %#v, want a FAILURE (logged-off connection executed a query)", got)
		}
		if sess.state == StateStreaming {
			t.Fatal("RUN after LOGOFF reached STREAMING")
		}
	})

	t.Run("logon_after_logoff_restores_access", func(t *testing.T) {
		t.Parallel()
		sess := newSession(newTestEngine(t), acceptAdminAuthHandler(), "")

		if msgs, err := sess.HandleMessage(ctx, goodHello()); err != nil || !isSuccess(msgs) {
			t.Fatalf("HELLO: msgs=%#v err=%v", msgs, err)
		}
		if msgs, err := sess.HandleMessage(ctx, &proto.Logoff{}); err != nil || !isSuccess(msgs) {
			t.Fatalf("LOGOFF: msgs=%#v err=%v", msgs, err)
		}
		if sess.state != StateReady {
			t.Fatalf("state after LOGOFF: got %v, want READY", sess.state)
		}

		// A fresh LOGON (legal in READY) re-authenticates.
		msgs, err := sess.HandleMessage(ctx, goodLogon())
		if err != nil || !isSuccess(msgs) {
			t.Fatalf("LOGON after LOGOFF: msgs=%#v err=%v", msgs, err)
		}
		if !sess.authenticated {
			t.Fatal("LOGON must re-authenticate the session")
		}

		// RUN now works again.
		got, err := sess.HandleMessage(ctx, &proto.Run{Query: "MATCH (n) RETURN n", Extra: map[string]interface{}{}})
		if err != nil {
			t.Fatalf("RUN after re-LOGON: %v", err)
		}
		if isFailure(got) {
			t.Fatalf("RUN after a fresh LOGON was rejected: %#v", got)
		}
		if sess.state != StateStreaming {
			t.Fatalf("state after RUN: got %v, want STREAMING", sess.state)
		}
	})
}
