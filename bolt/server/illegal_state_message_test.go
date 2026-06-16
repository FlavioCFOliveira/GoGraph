package server

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestFailTransition_ReportsOriginatingState is the regression gate for the
// illegal-message diagnostic surfaced by the DST BoltAbuser (#1549): the FAILURE
// for an illegal transition must name the state the message was illegal IN, not
// the FAILED state the session lands in. Before the fix every illegal transition
// reported "in state FAILED" regardless of where it occurred, which is
// uninformative; a PULL sent in READY must read "in state READY" so the client
// learns it needed a RUN first.
func TestFailTransition_ReportsOriginatingState(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t) // HELLO already applied; session is READY

	// PULL is illegal in READY (no active stream). The FAILURE must name READY.
	msgs, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("response count: %d", len(msgs))
	}
	f, ok := msgs[0].(*proto.Failure)
	if !ok {
		t.Fatalf("expected *proto.Failure, got %T", msgs[0])
	}
	if !strings.Contains(f.Message, "in state READY") {
		t.Errorf("illegal-transition message names the wrong state: %q (want it to mention the originating state READY)", f.Message)
	}
	if strings.Contains(f.Message, "in state FAILED") {
		t.Errorf("illegal-transition message reports the post-transition FAILED state instead of the originating state: %q", f.Message)
	}
	// The behaviour is unchanged: the session still moves to FAILED.
	if sess.state != StateFailed {
		t.Fatalf("state after illegal message: got %v, want FAILED", sess.state)
	}
}
