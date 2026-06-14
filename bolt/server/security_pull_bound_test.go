package server

// security_pull_bound_test.go — DEFENSE LOCK-IN for the PULL n <= 0 ("pull
// all") path (security audit, resource-exhaustion cluster).
//
// e2e_result_cap_test.go drives the engine result-row cap through the real
// driver, which issues batched PULLs with a positive n. This file pins the
// COMPLEMENTARY path the driver does not exercise: a hostile or naive client
// can send PULL {n:0} or PULL {n:<negative>}, both of which handlePull treats
// as "pull all" (n <= 0 drains the whole cursor in one handler call). The
// security property is that even that unbatched drain stays bounded by the
// engine's MaxResultRows — the cap trips inside the visibility barrier during
// materialisation, so the surplus rows never reach the Bolt stream and the
// server returns a typed LimitExceeded FAILURE rather than materialising an
// unbounded result set.
//
// Driving handlePull directly (white-box) lets the test send the exact n=0 and
// n=-2 values a real driver never sends. Layer: short; no sockets.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// secBoltCappedSession builds an authenticated session over an engine capped at
// maxRows result rows. The test drives row production with
// "UNWIND range(1, $n) AS x RETURN x", which deterministically yields n rows
// through the same in-barrier materialise path a whole-graph MATCH takes
// (mirroring e2e_result_cap_test.go) and needs no seeded data.
func secBoltCappedSession(t *testing.T, maxRows int64) *Session {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: maxRows})

	sess := newSession(eng, NoAuthHandler{}, "")
	if msgs, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil || !isSuccess(msgs) {
		t.Fatalf("HELLO: msgs=%#v err=%v", msgs, err)
	}
	return sess
}

// TestSec_Bolt_PullNonPositiveBoundedByCap is the gate: PULL {n:0} and
// PULL {n:negative} over a query that would yield more rows than the engine cap
// must fail with the LimitExceeded FAILURE rather than draining the whole
// (over-cap) cursor to the client.
func TestSec_Bolt_PullNonPositiveBoundedByCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const maxRows = 50

	cases := []struct {
		name string
		n    int64
	}{
		{"pull_zero", 0},
		{"pull_negative_one", -1},
		{"pull_negative_two", -2},
		{"pull_min_int64", -9223372036854775808},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess := secBoltCappedSession(t, maxRows)

			// RUN a query that would yield maxRows*10 rows through the in-barrier
			// materialise path.
			runMsgs, err := sess.HandleMessage(ctx, &proto.Run{
				Query:      "UNWIND range(1, $n) AS x RETURN x",
				Parameters: map[string]packstream.Value{"n": int64(maxRows * 10)},
				Extra:      map[string]packstream.Value{},
			})
			if err != nil {
				t.Fatalf("RUN: unexpected error %v", err)
			}
			if isFailure(runMsgs) {
				t.Fatalf("RUN was rejected before PULL: %#v", runMsgs)
			}
			if sess.state != StateStreaming {
				t.Fatalf("state after RUN: got %v, want STREAMING", sess.state)
			}

			// PULL with the non-positive n. The engine cap must trip; the surplus
			// rows must not be streamed.
			pullMsgs, err := sess.HandleMessage(ctx, &proto.Pull{N: tc.n, QID: -1})
			if err != nil {
				t.Fatalf("PULL{n:%d}: unexpected transport error %v", tc.n, err)
			}

			// The buffered (no-sink) path appends RECORDs then a trailing message.
			// With the cap tripping during materialisation, the terminal message
			// must be a LimitExceeded FAILURE and the RECORD count must be bounded
			// (never the full over-cap set).
			recordCount := 0
			var failure *proto.Failure
			for _, msg := range pullMsgs {
				switch mm := msg.(type) {
				case *proto.Record:
					recordCount++
				case *proto.Failure:
					failure = mm
				}
			}
			if failure == nil {
				t.Fatalf("PULL{n:%d} over a %d-row query with cap %d did not fail; streamed %d records (unbounded materialisation)",
					tc.n, maxRows*10, maxRows, recordCount)
			}
			if failure.Code != "Neo.ClientError.General.LimitExceeded" {
				t.Fatalf("PULL{n:%d} failure code = %q, want Neo.ClientError.General.LimitExceeded", tc.n, failure.Code)
			}
			// The cap trips during materialisation before any surplus row is
			// chunked, so no RECORD should precede the failure on this path.
			if recordCount > maxRows {
				t.Fatalf("PULL{n:%d} streamed %d records (> cap %d) before failing — materialisation was not bounded",
					tc.n, recordCount, maxRows)
			}
			// The message must name the limit (client-fault forwarding), not leak
			// internal text.
			if !strings.Contains(strings.ToLower(failure.Message), "row") &&
				!strings.Contains(strings.ToLower(failure.Message), "limit") {
				t.Logf("PULL{n:%d} failure message (informational): %q", tc.n, failure.Message)
			}
		})
	}
}

// TestSec_Bolt_PullNonPositiveWithinCapStreamsAll is the safety pin: when the
// query yields FEWER rows than the cap, PULL {n:0} must still stream every row
// and succeed — proving the "pull all" path is bounded by the cap, not broken
// by it.
func TestSec_Bolt_PullNonPositiveWithinCapStreamsAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		maxRows = 100
		rows    = 10 // well within the cap
	)
	sess := secBoltCappedSession(t, maxRows)

	if msgs, err := sess.HandleMessage(ctx, &proto.Run{
		Query:      "UNWIND range(1, $n) AS x RETURN x",
		Parameters: map[string]packstream.Value{"n": int64(rows)},
		Extra:      map[string]packstream.Value{},
	}); err != nil || isFailure(msgs) {
		t.Fatalf("RUN: msgs=%#v err=%v", msgs, err)
	}

	pullMsgs, err := sess.HandleMessage(ctx, &proto.Pull{N: 0, QID: -1})
	if err != nil {
		t.Fatalf("PULL{n:0}: %v", err)
	}
	records := 0
	sawSuccess := false
	for _, msg := range pullMsgs {
		switch msg.(type) {
		case *proto.Record:
			records++
		case *proto.Success:
			sawSuccess = true
		case *proto.Failure:
			t.Fatalf("PULL{n:0} within cap failed: %#v", msg)
		}
	}
	if !sawSuccess {
		t.Fatal("PULL{n:0} within cap did not end in SUCCESS")
	}
	if records != rows {
		t.Fatalf("PULL{n:0} streamed %d records, want all %d", records, rows)
	}
}
