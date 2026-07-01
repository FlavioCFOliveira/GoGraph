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
// The fix adds a symmetric DefaultStatementTimeout applied via
// resolveStmtTimeout. These tests pin the policy deterministically (no timing
// dependence): the precedence helper, the NewServer secure-by-default, and the
// session-carried default that together guarantee every autocommit statement on
// a default server gets a finite wall-clock bound.

import (
	"testing"
	"time"
)

// TestResolveStmtTimeout is the core, non-vacuous proof of F1: with no
// client-supplied timeout and no server cap (the default configuration), the
// effective bound is now the finite default floor, not zero (unbounded).
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
		// THE FIX: default config, no client timeout → finite default floor
		// (was 0 = unbounded before #1828).
		{"default-config-no-client", 0, def, 0, def},
		{"client-timeout-wins", 5 * time.Second, def, 0, 5 * time.Second},
		{"cap-clamps-default", 0, def, capLimit, capLimit}, // 30s default clamped to 10s cap
		{"cap-above-default-no-clamp", 0, def, time.Minute, def},
		{"cap-clamps-client", 50 * time.Second, def, capLimit, capLimit}, // client clamped by cap
		{"client-below-cap-unclamped", 3 * time.Second, def, capLimit, 3 * time.Second},
		// Deliberate operator opt-out: non-positive default AND no cap → unbounded.
		{"explicit-unbounded-opt-out", 0, 0, 0, 0},
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

// TestNewServer_DefaultStatementTimeoutSecureByDefault asserts NewServer fills a
// zero DefaultStatementTimeout with the finite DefaultStatementTimeout constant
// (so a zero-value Options can never yield an unbounded autocommit default) and
// preserves an explicit operator-supplied value.
func TestNewServer_DefaultStatementTimeoutSecureByDefault(t *testing.T) {
	t.Parallel()

	t.Run("zero-filled-with-default", func(t *testing.T) {
		t.Parallel()
		srv, err := NewServer(newTestEngine(t), Options{Auth: NoAuthHandler{}})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		if srv.opts.DefaultStatementTimeout != DefaultStatementTimeout {
			t.Fatalf("default DefaultStatementTimeout = %v; want %v (finite floor)",
				srv.opts.DefaultStatementTimeout, DefaultStatementTimeout)
		}
	})

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

// TestSession_AutocommitCarriesDefaultStmtTimeout chains the fix end-to-end at
// the session level: a freshly constructed session carries a finite
// defaultStmtTimeout, and with no client-supplied timeout the resolved bound is
// finite — i.e. a default-config server bounds an autocommit statement.
func TestSession_AutocommitCarriesDefaultStmtTimeout(t *testing.T) {
	t.Parallel()
	sess := newReadySession(t)
	if sess.defaultStmtTimeout <= 0 {
		t.Fatalf("session defaultStmtTimeout = %v; want a finite floor", sess.defaultStmtTimeout)
	}
	// No client timeout (stmtTimeout==0), no server cap (maxStmtTimeout==0):
	// the effective autocommit bound must be finite.
	if got := resolveStmtTimeout(sess.stmtTimeout, sess.defaultStmtTimeout, sess.maxStmtTimeout); got <= 0 {
		t.Fatalf("autocommit statement on a default session is unbounded (effective=%v); want a finite bound", got)
	}
}
