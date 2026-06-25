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

// TestDiscard_IgnoresN_DocumentedLimitation pins the documented #1783 limitation
// that DISCARD ignores its n field and always discards the whole stream
// (has_more=false). If partial discard is implemented (backlog), this assertion
// should flip and docs/bolt.md must be updated.
func TestDiscard_IgnoresN_DocumentedLimitation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sess := newReadySession(t)
	if _, err := sess.HandleMessage(ctx, &proto.Run{Query: "UNWIND [1,2,3,4,5] AS x RETURN x"}); err != nil {
		t.Fatalf("RUN: %v", err)
	}
	msgs, err := sess.HandleMessage(ctx, &proto.Discard{N: 2, QID: -1})
	if err != nil {
		t.Fatalf("DISCARD: %v", err)
	}
	succ, ok := msgs[0].(*proto.Success)
	if !ok {
		t.Fatalf("DISCARD: got %T, want *proto.Success", msgs[0])
	}
	if hm, _ := succ.Metadata["has_more"].(bool); hm {
		t.Errorf("DISCARD{n:2} reported has_more=true; current behaviour discards all (#1783 documented limitation)")
	}
}
