package server_test

// terminate_reason_test.go — rmp #2560: an operator-initiated termination and a
// genuine timeout must be distinguishable by the client FROM THE FAILURE ALONE.
//
// Before the fix a single teardown (Session.reapTimedOutTx) chose the reason for
// all three server-initiated terminations, so an operator termination told the
// client "Neo.ClientError.Transaction.TransactionTimedOut" with the message "the
// transaction has been terminated because it exceeded its timeout; the writer
// lock was released". Both halves were false: no timeout elapsed, and the writer
// serialisation the clause refers to was retired by rmp #2305/#2306.
//
// The same teardown also incremented metricTxTimedOut unconditionally, which made
// that counter a superset of the idle reaps and the operator terminations and so
// erased the separation metricTxIdleReaped was added to draw (rmp #2175). Both
// halves are asserted here, because the wire reason and the metric are the same
// defect seen from two sides: the operator path borrowing the timeout path's
// identity.
//
// These tests install the process-global metrics backend, so they must not run in
// parallel (no t.Parallel) and they restore the no-op default on cleanup.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// The reasons as the CLIENT must see them. Mirrored here rather than exported
// from the server package: these are wire strings a driver matches on, so the
// test's job is to pin the literal text, and a shared constant would let a
// rename of the value pass unnoticed on both sides at once.
const (
	wantTimedOutCode    = "Neo.ClientError.Transaction.TransactionTimedOut"
	wantTimedOutMessage = "the transaction has been terminated because it exceeded its timeout"

	wantTerminatedCode    = "Neo.ClientError.Transaction.Terminated"
	wantTerminatedMessage = "the transaction has been terminated by an operator request"
)

// installMetricsProbe swaps in a capturing backend for the duration of the test.
func installMetricsProbe(t *testing.T) *serverMetricsProbe {
	t.Helper()
	probe := newServerMetricsProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })
	return probe
}

// TestTerminateReason_OperatorTerminationIsNotReportedAsATimeout drives
// Server.TerminateTransaction and reads the failure the client's next
// request-phase message receives.
func TestTerminateReason_OperatorTerminationIsNotReportedAsATimeout(t *testing.T) {
	probe := installMetricsProbe(t)
	srv, addr := startTestServerHandle(t, server.Options{
		ConnTimeout: 30 * time.Second,
		// Both bounds far longer than the test, so nothing but the operator can
		// end the transaction. If a bound could fire, a green result would not
		// distinguish the fix from a race won by the reaper.
		DefaultTxTimeout: 5 * time.Minute,
		MaxTxIdleTime:    5 * time.Minute,
	})

	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)
	c.begin(t)
	c.run(t, "CREATE (:Terminated2560)", nil)
	c.pullAll(t)

	infos := waitForTransactions(t, srv, 1)
	if err := srv.TerminateTransaction(infos[0].ID); err != nil {
		t.Fatalf("TerminateTransaction: %v", err)
	}
	waitForTransactions(t, srv, 0)

	c.sendRequest(t, &proto.Run{Query: "RETURN 1"})
	f := c.recvFailure(t)
	if f.Code != wantTerminatedCode {
		t.Errorf("operator termination reported code %q, want %q — the client cannot tell a\n"+
			"deliberate intervention from an expired bound (rmp #2560)", f.Code, wantTerminatedCode)
	}
	if f.Message != wantTerminatedMessage {
		t.Errorf("operator termination reported message %q, want %q", f.Message, wantTerminatedMessage)
	}

	// The counter for the event that DID happen fires; the one for the event that
	// did not must stay at zero. Asserting only the first would pass on the old
	// build, which incremented both.
	if got := probe.get("bolt.server.tx.terminated"); got != 1 {
		t.Errorf("bolt.server.tx.terminated = %d, want 1", got)
	}
	if got := probe.get("bolt.server.tx.timedout"); got != 0 {
		t.Errorf("bolt.server.tx.timedout = %d, want 0 — an operator termination is not a\n"+
			"timeout, and counting it as one makes the counter a superset of all three\n"+
			"termination events (rmp #2560)", got)
	}
	if got := probe.get("bolt.server.tx.idlereaped"); got != 0 {
		t.Errorf("bolt.server.tx.idlereaped = %d, want 0", got)
	}
}

// TestTerminateReason_IdleReapIsStillReportedAsATimeout is the other half of the
// pair: the timeout reason must SURVIVE the split. A fix that gave the operator
// path its own reason by changing the shared one would pass the test above and
// fail here.
//
// It also pins the metric separation from the idle side: an idle reap increments
// metricTxIdleReaped and NOT metricTxTimedOut, which is what
// metricTxIdleReaped's own godoc has always promised and what the old code did
// not deliver.
func TestTerminateReason_IdleReapIsStillReportedAsATimeout(t *testing.T) {
	probe := installMetricsProbe(t)
	const idleBound = 300 * time.Millisecond
	addr := startTestServer(t, server.Options{
		ConnTimeout: 30 * time.Second,
		// The TOTAL bound is far away, so the bound that fires is unambiguously
		// the idle one.
		DefaultTxTimeout: 5 * time.Minute,
		MaxTxIdleTime:    idleBound,
	})

	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)
	c.begin(t)
	c.run(t, "CREATE (:Idle2560)", nil)
	c.pullAll(t)
	// From here the client sends nothing, so the idle bound is what elapses.

	if !waitFor(func() bool { return probe.get("bolt.server.tx.idlereaped") > 0 }, 10*time.Second) {
		t.Fatal("the idle reaper never fired; the test measured nothing")
	}

	c.sendRequest(t, &proto.Run{Query: "RETURN 1"})
	f := c.recvFailure(t)
	if f.Code != wantTimedOutCode {
		t.Errorf("idle reap reported code %q, want %q", f.Code, wantTimedOutCode)
	}
	if f.Message != wantTimedOutMessage {
		t.Errorf("idle reap reported message %q, want %q — the stale writer-lock clause was\n"+
			"dropped by rmp #2560 (rmp #2305/#2306 retired the hold it named)", f.Message, wantTimedOutMessage)
	}

	if got := probe.get("bolt.server.tx.idlereaped"); got != 1 {
		t.Errorf("bolt.server.tx.idlereaped = %d, want 1", got)
	}
	if got := probe.get("bolt.server.tx.timedout"); got != 0 {
		t.Errorf("bolt.server.tx.timedout = %d, want 0 — an idle reap and a total-lifetime\n"+
			"timeout are the two events metricTxIdleReaped exists to separate, so counting\n"+
			"the idle one in BOTH counters erases that separation (rmp #2175/#2560)", got)
	}
	if got := probe.get("bolt.server.tx.terminated"); got != 0 {
		t.Errorf("bolt.server.tx.terminated = %d, want 0", got)
	}
}

// TestTerminateReason_TheTwoReasonsAreDistinct is the acceptance criterion stated
// directly: the two events must not merely each be right, they must DIFFER. A
// future edit that collapses them back onto one code or one message — which is
// precisely the defect rmp #2560 fixed — fails here even if both constants are
// individually plausible.
func TestTerminateReason_TheTwoReasonsAreDistinct(t *testing.T) {
	t.Parallel()
	if wantTimedOutCode == wantTerminatedCode {
		t.Errorf("the timeout and the operator-termination codes are the same string (%q), so a\n"+
			"client cannot tell the two events apart from the failure alone (rmp #2560)", wantTimedOutCode)
	}
	if wantTimedOutMessage == wantTerminatedMessage {
		t.Errorf("the timeout and the operator-termination messages are the same string (%q)",
			wantTimedOutMessage)
	}
}
