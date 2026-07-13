package server

// commit_auth_gate_test.go — regression for the COMMIT/ROLLBACK auth-gating
// hardening (2026-07-13 audit, security F5, CWE-306). A session that sent
// LOGOFF while a transaction was open is left in TX_READY but unauthenticated;
// COMMIT and ROLLBACK were gated only on state, so such a session could still
// finalise the transaction. Both transitions now also require authentication.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestSec_CommitRequiresAuth pins that COMMIT from an unauthenticated TX_READY
// session is rejected (never finalises the transaction).
func TestSec_CommitRequiresAuth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := auth51(t)

	// The exact vulnerable state: inside a transaction, but de-authorised.
	sess.state = StateTxReady
	sess.authenticated = false

	got, err := sess.HandleMessage(ctx, &proto.Commit{})
	if err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if !isFailure(got) {
		t.Fatalf("BYPASS: COMMIT from an unauthenticated session was accepted: %#v", got)
	}
}

// TestSec_RollbackRequiresAuth pins the same for ROLLBACK.
func TestSec_RollbackRequiresAuth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := auth51(t)

	sess.state = StateTxReady
	sess.authenticated = false

	got, err := sess.HandleMessage(ctx, &proto.Rollback{})
	if err != nil {
		t.Fatalf("ROLLBACK: %v", err)
	}
	if !isFailure(got) {
		t.Fatalf("BYPASS: ROLLBACK from an unauthenticated session was accepted: %#v", got)
	}
}

// TestSec_CommitStillWorksAuthenticated guards against a false positive: an
// authenticated session in TX_READY commits normally.
func TestSec_CommitStillWorksAuthenticated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := auth51(t)

	if msgs, err := sess.HandleMessage(ctx, &proto.Begin{Extra: map[string]interface{}{}}); err != nil || !isSuccess(msgs) {
		t.Fatalf("BEGIN: msgs=%#v err=%v", msgs, err)
	}
	if sess.state != StateTxReady {
		t.Fatalf("after BEGIN state = %v, want TxReady", sess.state)
	}
	got, err := sess.HandleMessage(ctx, &proto.Commit{})
	if err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	if isFailure(got) {
		t.Fatalf("COMMIT from an authenticated session was rejected: %#v", got)
	}
}
