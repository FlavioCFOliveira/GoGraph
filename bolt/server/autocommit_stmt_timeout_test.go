package server

// Regression lock-ins for the 2026-07-01 hostility+load audit finding F1
// (#1828): the Bolt autocommit RUN path applied NO default statement-timeout
// floor. When a client supplied no `timeout` metadata and MaxStatementTimeout
// was left at zero (the default server configuration), the effective bound was
// zero and runCtx carried no deadline — so an authenticated client could pin a
// CPU core indefinitely with a super-linear-runtime, single-output-row query
// (e.g. a disconnected multi-pattern Cartesian product whose result-row/byte
// caps never fire). Explicit BEGIN transactions already received a mandatory
// DefaultTxTimeout floor; autocommit was the sole unbounded-runtime path.
//
// The fix added a symmetric DefaultStatementTimeout applied via
// resolveStmtTimeout. These tests pin the policy deterministically (no timing
// dependence): the precedence helper, and the session-carried default that
// together bound an autocommit statement whenever one is configured.
//
// WHAT rmp #2807 CHANGED. The floor is no longer applied BY DEFAULT:
// DefaultStatementTimeout is now 0, matching PostgreSQL's statement_timeout, so
// a default-configured server once again runs an autocommit statement with no
// wall-clock bound. That reversal is deliberate and is documented on the
// constant; the gates that pin the new defaults live in default_timeouts_test.go
// and default_timeouts_reach_test.go. What survives here is the MECHANISM: when
// a bound IS configured, it is carried onto the session, resolved with the right
// precedence, and stays armed across the RUN -> PULL boundary. Each test below
// therefore configures the bound explicitly rather than inheriting a default
// that no longer exists.

import (
	"context"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestSession_AutocommitWriteCommitsAcrossPull is the regression guard for the
// #1828 follow-up: the autocommit default statement deadline must remain armed
// across the RUN -> PULL boundary. A write (or DDL) statement commits during the
// PULL/DISCARD drain, not on RUN, so cancelling the deadline at handleRun's
// return (a defer) killed the context before the commit and surfaced a spurious
// "context canceled" failure. drainResult now fires the cancel when the cursor
// closes instead. This drives an autocommit CREATE through RUN then PULL and
// asserts neither leg fails.
//
// The bound is set EXPLICITLY. Since rmp #2807 the default is 0 — no deadline at
// all — and a session with no deadline cannot exercise a
// deadline-cancelled-too-early regression, so the test would pass for the wrong
// reason. The precondition below is what stops that happening silently.
func TestSession_AutocommitWriteCommitsAcrossPull(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)
	sess.setDefaultStmtTimeout(30 * time.Second)
	if sess.defaultStmtTimeout <= 0 {
		t.Fatalf("precondition: session has no default statement deadline (%v); this regression needs an ARMED deadline to be able to fail", sess.defaultStmtTimeout)
	}

	runMsgs, err := sess.HandleMessage(context.Background(), &proto.Run{
		Query: "CREATE (:AutoWrite)",
		Extra: map[string]interface{}{}, // no client timeout -> default floor applies
	})
	if err != nil {
		t.Fatalf("RUN: %v", err)
	}
	if f, ok := runMsgs[0].(*proto.Failure); ok {
		t.Fatalf("RUN returned FAILURE: %s / %s", f.Code, f.Message)
	}

	pullMsgs, err := sess.HandleMessage(context.Background(), &proto.Pull{N: -1, QID: -1})
	if err != nil {
		t.Fatalf("PULL: %v", err)
	}
	for _, m := range pullMsgs {
		if f, ok := m.(*proto.Failure); ok {
			t.Fatalf("PULL returned FAILURE (deadline cancelled before the write committed?): %s / %s", f.Code, f.Message)
		}
	}
}

// TestResolveStmtTimeout pins the precedence the helper implements. The
// "configured-default-no-client" case is the F1 fix; the "no-bound-anywhere"
// case is what a DEFAULT-configured server produces since rmp #2807, and is no
// longer reachable only by an explicit opt-out.
func TestResolveStmtTimeout(t *testing.T) {
	t.Parallel()
	const (
		def      = 30 * time.Second
		capLimit = 10 * time.Second
	)
	cases := []struct {
		name   string
		client time.Duration
		def    time.Duration
		max    time.Duration
		want   time.Duration
	}{
		// THE F1 FIX: a configured server default applies when the client
		// supplies no timeout of its own.
		{"configured-default-no-client", 0, def, 0, def},
		{"client-timeout-wins", 5 * time.Second, def, 0, 5 * time.Second},
		{"cap-clamps-default", 0, def, capLimit, capLimit}, // 30s default clamped to 10s cap
		{"cap-above-default-no-clamp", 0, def, time.Minute, def},
		{"cap-clamps-client", 50 * time.Second, def, capLimit, capLimit}, // client clamped by cap
		{"client-below-cap-unclamped", 3 * time.Second, def, capLimit, 3 * time.Second},
		// No client timeout, no server default, no cap → UNBOUNDED. This is what
		// a default-configured server produces since rmp #2807; before it, only
		// an explicit operator opt-out reached this row.
		{"no-bound-anywhere", 0, 0, 0, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveStmtTimeout(tc.client, tc.def, tc.max); got != tc.want {
				t.Fatalf("resolveStmtTimeout(%v, %v, %v) = %v; want %v",
					tc.client, tc.def, tc.max, got, tc.want)
			}
		})
	}
}

// TestNewServer_DefaultStatementTimeoutHonoursTheOperator asserts that an
// operator-supplied autocommit bound reaches the server unchanged.
//
// This test was named ...SecureByDefault and asserted that a zero-value Options
// was FILLED with a finite floor. rmp #2807 reversed that default, so the name
// would now describe behaviour the server does not have. What a zero-value
// Options produces is pinned instead by TestTimeoutDefaults_AreDisabled in
// default_timeouts_test.go, alongside the negative-value refusal; the operator
// half is what remains this file's, because it is the half the #1828 mechanism
// depends on.
func TestNewServer_DefaultStatementTimeoutHonoursTheOperator(t *testing.T) {
	t.Parallel()

	t.Run("explicit-value-preserved", func(t *testing.T) {
		t.Parallel()
		want := 90 * time.Second
		srv, err := NewServer(newTestEngine(t), Options{Auth: NoAuthHandler{}, DefaultStatementTimeout: want})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		if srv.opts.DefaultStatementTimeout != want {
			t.Fatalf("DefaultStatementTimeout = %v; want %v (explicit preserved)",
				srv.opts.DefaultStatementTimeout, want)
		}
	})
}

// TestSession_CarriesTheConfiguredStmtTimeout chains the #1828 mechanism
// end-to-end at the session level: an operator-configured bound is carried onto
// the session and, with no client-supplied timeout and no server cap, is what
// resolveStmtTimeout returns.
//
// The control arm is the same session WITHOUT the configured bound, which must
// resolve to zero — the default-configured server since rmp #2807. Without it,
// the first arm could not distinguish "the configured value was carried" from
// "some finite value was already there".
func TestSession_CarriesTheConfiguredStmtTimeout(t *testing.T) {
	t.Parallel()

	const configured = 30 * time.Second

	t.Run("configured", func(t *testing.T) {
		t.Parallel()
		sess := newReadySession(t)
		sess.setDefaultStmtTimeout(configured)
		if sess.defaultStmtTimeout != configured {
			t.Fatalf("session defaultStmtTimeout = %v; want the configured %v", sess.defaultStmtTimeout, configured)
		}
		if got := resolveStmtTimeout(sess.stmtTimeout, sess.defaultStmtTimeout, sess.maxStmtTimeout); got != configured {
			t.Fatalf("effective autocommit bound = %v; want the configured %v", got, configured)
		}
	})

	t.Run("unconfigured-is-unbounded", func(t *testing.T) {
		t.Parallel()
		sess := newReadySession(t)
		if got := resolveStmtTimeout(sess.stmtTimeout, sess.defaultStmtTimeout, sess.maxStmtTimeout); got != 0 {
			t.Fatalf("effective autocommit bound on an UNconfigured session = %v; want 0. "+
				"rmp #2807 set DefaultStatementTimeout to 0, matching PostgreSQL's statement_timeout; "+
				"see the constant's godoc for what that costs", got)
		}
	})
}
