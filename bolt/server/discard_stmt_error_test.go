package server

// discard_stmt_error_test.go — gate test for #1378 (handleDiscard
// defense-in-depth): a DISCARD on a cursor that already carries a deferred
// statement error must route through FAILURE, not SUCCESS.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestHandleDiscard_StatementError_RoutesFailure is the #1378 gate for the
// Bolt layer: if the open result cursor already carries an error when DISCARD
// arrives, handleDiscard must return a FAILURE and leave the session in FAILED.
//
// We trigger the cursor error via a result-row cap (MaxResultRows:1) on a
// query that returns 3 rows. materialize() runs at RUN time and sets rowsErr
// immediately, so the session enters STREAMING with a cursor whose Err() is
// non-nil. Sending DISCARD instead of PULL exercises the new guard.
func TestHandleDiscard_StatementError_RoutesFailure(t *testing.T) {
	t.Parallel()

	// Build an engine that caps results at 1 row; the query returns 3.
	eng := cypher.NewEngineWithOptions(
		lpg.New[string, float64](adjlist.Config{}),
		cypher.EngineOptions{MaxResultRows: 1},
	)
	sess := newSession(eng, NoAuthHandler{}, "")

	// HELLO → READY.
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	if sess.state != StateReady {
		t.Fatalf("pre-condition: state after HELLO = %v, want READY", sess.state)
	}

	// RUN — materialise returns a capped cursor (rowsErr set) but handleRun does
	// not check rowsErr; it only checks the run-level error. The session
	// transitions to STREAMING with a cursor whose Err() is non-nil.
	runMsgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "UNWIND [1,2,3] AS x RETURN x",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("RUN: unexpected transport error: %v", err)
	}
	if len(runMsgs) == 0 {
		t.Fatal("RUN: no response messages")
	}
	if _, ok := runMsgs[0].(*proto.Success); !ok {
		t.Fatalf("RUN: expected *proto.Success, got %T (want success so session enters STREAMING)", runMsgs[0])
	}
	if sess.state != StateStreaming {
		t.Fatalf("pre-condition: state after RUN = %v, want STREAMING", sess.state)
	}
	// Verify the pre-condition: the cursor already carries an error.
	if sess.result == nil {
		t.Fatal("pre-condition: sess.result is nil after RUN")
	}
	if sess.result.Err() == nil {
		t.Fatal("pre-condition: sess.result.Err() is nil — no deferred error to test against")
	}

	// DISCARD — must return FAILURE (not SUCCESS) because the cursor has an error.
	discardMsgs, err := sess.HandleMessage(context.Background(), &proto.Discard{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("DISCARD: unexpected transport error: %v", err)
	}
	if len(discardMsgs) != 1 {
		t.Fatalf("DISCARD response count: %d, want 1", len(discardMsgs))
	}
	if _, ok := discardMsgs[0].(*proto.Failure); !ok {
		t.Fatalf("DISCARD with errored cursor: got %T, want *proto.Failure", discardMsgs[0])
	}

	// Session must be in FAILED after the deferred-error DISCARD.
	if sess.state != StateFailed {
		t.Fatalf("state after DISCARD with error: %v, want FAILED", sess.state)
	}
}

// TestHandleDiscard_NoError_Succeeds confirms the happy path is unchanged: a
// DISCARD on a clean cursor returns SUCCESS and leaves the session in READY.
// This is a regression guard for Fix 2 — the new guard must not affect the
// normal flow.
func TestHandleDiscard_NoError_Succeeds(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	sess := newSession(eng, NoAuthHandler{}, "")

	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}

	// RUN → STREAMING with a clean (zero-row) result.
	if _, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query:      "MATCH (n) RETURN n",
		Parameters: nil,
		Extra:      map[string]interface{}{},
	}); err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if sess.state != StateStreaming {
		t.Fatalf("pre-condition: state after RUN = %v, want STREAMING", sess.state)
	}

	// DISCARD on a clean cursor must return SUCCESS.
	discardMsgs, err := sess.HandleMessage(context.Background(), &proto.Discard{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("DISCARD: %v", err)
	}
	if len(discardMsgs) != 1 {
		t.Fatalf("DISCARD response count: %d, want 1", len(discardMsgs))
	}
	if _, ok := discardMsgs[0].(*proto.Success); !ok {
		t.Fatalf("DISCARD on clean cursor: got %T, want *proto.Success", discardMsgs[0])
	}
	if sess.state != StateReady {
		t.Fatalf("state after DISCARD: %v, want READY", sess.state)
	}
}
