package server

// security_bolt_txtimeout_overflow_test.go —
// [SEC-2026-06-14b][BOLT] tx_timeout integer overflow bypasses the writer-lock reaper.
//
// handleBegin computes the effective explicit-transaction timeout as
//
//	effective = time.Duration(ms) * time.Millisecond
//
// from the client-supplied tx_timeout (in milliseconds), guarded only by
// `ms > 0`. time.Duration is an int64 of NANOSECONDS, so any ms above
// ~9.22e12 (≈106 days) overflows the int64 multiply. A client can pick a value
// that wraps the product to exactly 0 (e.g. 1<<62 ms) or to a negative value
// (e.g. math.MaxInt64 ms). When the wrapped `effective` is <= 0 AND the
// operator has not set MaxStatementTimeout (the production default — see
// NewServer, which sets only DefaultTxTimeout), the clamp
//
//	if s.maxStmtTimeout > 0 && (effective <= 0 || effective > s.maxStmtTimeout)
//
// is skipped (its guard requires maxStmtTimeout > 0), so `effective` stays <= 0.
// handleBegin then takes the `else` branch and leaves s.txDeadline as the zero
// Time, and newTx is called with timeout <= 0, which roots the engine
// transaction at the bare connection context with a NO-OP cancel (no engine
// deadline either).
//
// The serve loop arms its wall-clock reaper ONLY when sess.txDeadline is
// non-zero. With txDeadline zero, NO reaper is armed. The reaper (#1346) and the
// DefaultTxTimeout bound (#1302) exist precisely to guarantee that an explicit
// transaction can never hold the engine's single global writer serialisation
// indefinitely while the client keeps the connection alive. This overflow
// DEFEATS that guarantee on a default-configured server: a client BEGINs with
// the overflow timeout, then refreshes the idle ConnTimeout with a trivial
// RUN/PULL every <ConnTimeout, holding the writer lock effectively forever and
// blocking every other writer on the server — a liveness denial of service.
//
// CWE-190 (Integer Overflow) leading to CWE-400 / CWE-667
// (uncontrolled resource consumption / writer-lock starvation).
//
// Root cause: bolt/server/session.go handleBegin, the
// `time.Duration(ms) * time.Millisecond` multiply with no overflow check.
// Fix: detect the overflow — when ms > 0 but the product is <= 0 (or when ms
// exceeds the largest value representable as a positive Duration in ms,
// math.MaxInt64 / int64(time.Millisecond)), clamp `effective` to a safe finite
// bound (defaultTxTimeout, or a hard server maximum) regardless of whether
// maxStmtTimeout is set. The identical multiply in handleRun (the per-statement
// "timeout" key) should get the same guard.

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// beginWithTxTimeout HELLOs, BEGINs with the given tx_timeout ms on a session
// configured like the production default (DefaultTxTimeout set, no
// MaxStatementTimeout), and returns the session for inspection.
func beginWithTxTimeout(t *testing.T, ms int64) *Session {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	sess := newSession(eng, NoAuthHandler{}, "")
	sess.setDefaultTxTimeout(DefaultTxTimeout) // production default; no MaxStatementTimeout
	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	resp, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{"tx_timeout": ms},
	})
	if err != nil {
		t.Fatalf("BEGIN(ms=%d): %v", ms, err)
	}
	if len(resp) != 1 {
		t.Fatalf("BEGIN(ms=%d): %d responses, want 1", ms, len(resp))
	}
	if _, ok := resp[0].(*proto.Success); !ok {
		t.Fatalf("BEGIN(ms=%d): reply %T, want Success", ms, resp[0])
	}
	if !sess.txActive {
		t.Fatalf("BEGIN(ms=%d): expected txActive", ms)
	}
	return sess
}

// TestSec_TxTimeoutOverflow_ReaperBypassed asserts that an explicit transaction
// opened with a hostile tx_timeout still arms the wall-clock reaper (a non-zero
// txDeadline). A zero txDeadline means the reaper is NOT armed and the
// writer-lock safety net is bypassed.
//
// FIXED #1484: handleBegin now converts the client tx_timeout overflow-safely
// (clientMillisToDuration). A non-positive or overflowing tx_timeout is treated
// as "unset" and the server DEFAULT (defaultTxTimeout) is applied, so every
// overflow case below arms a finite, bounded txDeadline (the reaper is armed)
// even on a default-configured server with no MaxStatementTimeout. This test is
// now a hard regression gate.
func TestSec_TxTimeoutOverflow_ReaperBypassed(t *testing.T) {
	const msPerMilli = int64(time.Millisecond) // nanoseconds per ms
	maxSafeMs := int64(math.MaxInt64) / msPerMilli

	cases := []struct {
		name string
		ms   int64
	}{
		{"overflow_to_zero_1<<62", int64(1) << 62},       // product == 0
		{"overflow_to_negative_MaxInt64", math.MaxInt64}, // product < 0
		{"just_over_max_safe_ms", maxSafeMs + 1},         // first ms that overflows
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := beginWithTxTimeout(t, tc.ms)
			defer func() { _, _ = sess.handleRollback() }()

			if sess.txDeadline.IsZero() {
				// Hard regression gate: an overflowing tx_timeout MUST arm the
				// default reaper, never leave txDeadline zero (#1484).
				t.Errorf("tx_timeout=%d overflowed time.Duration math to a non-positive value and handleBegin left txDeadline ZERO; the overflow must fall back to the server default so the #1302/#1346 finite writer-lock reaper is armed",
					tc.ms)
				return
			}
			// Conformant post-fix behaviour: a finite, bounded deadline is
			// armed, no further in the future than the default bound.
			maxAllowed := time.Now().Add(DefaultTxTimeout + time.Second)
			if sess.txDeadline.After(maxAllowed) {
				t.Errorf("tx_timeout=%d produced txDeadline %v, further in the future than the default bound %v; the overflow clamp must not admit an unbounded deadline",
					tc.ms, sess.txDeadline, DefaultTxTimeout)
			}
		})
	}
}

// TestSec_TxTimeoutOverflow_CaughtWhenMaxStmtTimeoutSet documents the partial
// mitigation: when the operator DOES set MaxStatementTimeout, the existing
// clamp catches the overflow (effective <= 0 → maxStmtTimeout). This passes
// today and must keep passing after the fix. It demonstrates that the gap is
// specific to the default configuration that relies on DefaultTxTimeout alone.
func TestSec_TxTimeoutOverflow_CaughtWhenMaxStmtTimeoutSet(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	sess := newSession(eng, NoAuthHandler{}, "")
	sess.setDefaultTxTimeout(DefaultTxTimeout)
	sess.setMaxStmtTimeout(10 * time.Second) // operator-configured cap

	if _, err := sess.HandleMessage(context.Background(), helloMsg()); err != nil {
		t.Fatalf("HELLO: %v", err)
	}
	resp, err := sess.HandleMessage(context.Background(), &proto.Begin{
		Extra: map[string]interface{}{"tx_timeout": int64(1) << 62},
	})
	if err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, ok := resp[0].(*proto.Success); !ok {
		t.Fatalf("BEGIN reply %T, want Success", resp[0])
	}
	defer func() { _, _ = sess.handleRollback() }()

	if sess.txDeadline.IsZero() {
		t.Fatal("with MaxStatementTimeout set, the existing clamp should catch the overflow and arm a finite reaper; got zero txDeadline")
	}
}
