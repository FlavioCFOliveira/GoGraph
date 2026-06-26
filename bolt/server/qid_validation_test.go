package server

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestPullDiscard_ForeignQID_Fails is the #1783 gate: PULL/DISCARD carrying an
// explicit qid >= 0 names a stream that does not exist (the single open stream
// always has qid -1), so it must FAIL with Neo.ClientError.Request.Invalid
// rather than be served against the current stream. A qid of -1 (the default /
// "current") still works.
func TestPullDiscard_ForeignQID_Fails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	foreignRejected := func(t *testing.T, mk func(qid int64) any) {
		sess := newReadySession(t)
		if _, err := sess.HandleMessage(ctx, &proto.Run{Query: "RETURN 1"}); err != nil {
			t.Fatalf("RUN: %v", err)
		}
		msgs, err := sess.HandleMessage(ctx, mk(999))
		if err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
		f, ok := msgs[0].(*proto.Failure)
		if !ok {
			t.Fatalf("foreign qid: got %T, want *proto.Failure", msgs[0])
		}
		if f.Code != "Neo.ClientError.Request.Invalid" {
			t.Errorf("foreign qid FAILURE code = %q, want Neo.ClientError.Request.Invalid", f.Code)
		}
	}

	t.Run("PULL", func(t *testing.T) {
		foreignRejected(t, func(qid int64) any { return &proto.Pull{N: -1, QID: qid} })
	})
	t.Run("DISCARD", func(t *testing.T) {
		foreignRejected(t, func(qid int64) any { return &proto.Discard{N: -1, QID: qid} })
	})

	// Control: the default current-stream qid (-1) is served normally.
	t.Run("PULL_currentQID_ok", func(t *testing.T) {
		sess := newReadySession(t)
		if _, err := sess.HandleMessage(ctx, &proto.Run{Query: "RETURN 1"}); err != nil {
			t.Fatalf("RUN: %v", err)
		}
		msgs, err := sess.HandleMessage(ctx, &proto.Pull{N: -1, QID: -1})
		if err != nil {
			t.Fatalf("PULL -1: %v", err)
		}
		if _, isFail := msgs[len(msgs)-1].(*proto.Failure); isFail {
			t.Fatalf("PULL with current qid -1 unexpectedly failed: %v", msgs)
		}
	})
}

// TestDiscard_PartialN is the #1787 gate: DISCARD honours its n (fetch-size)
// field — DISCARD{n:2} on a 5-row stream discards 2, reports has_more=true, and
// a follow-up PULL{n:-1} yields the remaining 3 rows; DISCARD-all (n<=0) drains.
func TestDiscard_PartialN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	countRecords := func(msgs []any) int {
		n := 0
		for _, m := range msgs {
			if _, ok := m.(*proto.Record); ok {
				n++
			}
		}
		return n
	}

	// Partial discard then pull the remainder.
	sess := newReadySession(t)
	if _, err := sess.HandleMessage(ctx, &proto.Run{Query: "UNWIND [1,2,3,4,5] AS x RETURN x"}); err != nil {
		t.Fatalf("RUN: %v", err)
	}
	msgs, err := sess.HandleMessage(ctx, &proto.Discard{N: 2, QID: -1})
	if err != nil {
		t.Fatalf("DISCARD{n:2}: %v", err)
	}
	succ, ok := msgs[0].(*proto.Success)
	if !ok {
		t.Fatalf("DISCARD{n:2}: got %T, want *proto.Success", msgs[0])
	}
	if hm, _ := succ.Metadata["has_more"].(bool); !hm {
		t.Errorf("DISCARD{n:2} on a 5-row stream: has_more=false, want true (#1787)")
	}
	pull, err := sess.HandleMessage(ctx, &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("PULL after partial DISCARD: %v", err)
	}
	if got := countRecords(pull); got != 3 {
		t.Errorf("PULL after DISCARD{n:2} yielded %d records, want 3 (the remainder)", got)
	}

	// DISCARD-all (n <= 0) drains the whole stream: has_more=false.
	sess2 := newReadySession(t)
	if _, err := sess2.HandleMessage(ctx, &proto.Run{Query: "UNWIND [1,2,3] AS x RETURN x"}); err != nil {
		t.Fatalf("RUN: %v", err)
	}
	msgs2, err := sess2.HandleMessage(ctx, &proto.Discard{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("DISCARD-all: %v", err)
	}
	if hm, _ := msgs2[0].(*proto.Success).Metadata["has_more"].(bool); hm {
		t.Errorf("DISCARD{n:-1} reported has_more=true, want false (drained)")
	}
}
