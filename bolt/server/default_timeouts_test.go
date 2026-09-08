package server

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// default_timeouts_test.go — rmp #2807.
//
// The four server-side timeout defaults are DISABLED. This file pins that, and
// pins the three-way contract the fields now carry:
//
//	0  → the bound is disabled (the default)
//	>0 → the bound is active, at exactly that value
//	<0 → invalid; NewServer refuses it with a *NegativeTimeoutError
//
// Layer: short. Every assertion is a constant comparison or one NewServer call.

// wantTimeoutDefault is the value the four server-side timeout defaults were set
// to by rmp #2807, written as a LITERAL on purpose.
//
// Writing it as `DefaultTxTimeout` and friends would make the constant clause
// below compare each constant with itself, which cannot fail: the test would
// stay green against every value that has preceded this change
// (DefaultMaxTxIdleTime 5 s then 30 min, DefaultTxTimeout and
// DefaultStatementTimeout 30 s then 30 min, DefaultConnTimeout 30 s then 30 min)
// and against any later unintended edit. The literal is what gives the gate
// teeth — verified by reverting each constant in turn, which fails both the
// constant clause and the resolution clause for that field.
const wantTimeoutDefault time.Duration = 0

// timeoutDefaultCase names one of the four timeout fields, the constant that
// documents its default, and the accessors the clauses need.
type timeoutDefaultCase struct {
	// name is the Options field, which is also the subtest name. It is also the
	// name a *NegativeTimeoutError must report.
	name string
	// constName is the exported constant that documents the field's default.
	constName string
	// constant is the exported package constant under test.
	constant time.Duration
	// set writes a value into an Options about to be handed to NewServer.
	set func(*Options, time.Duration)
	// resolved reads back what NewServer stored on the server.
	resolved func(*Server) time.Duration
}

func timeoutDefaultCases() []timeoutDefaultCase {
	return []timeoutDefaultCase{
		{
			name:      "ConnTimeout",
			constName: "DefaultConnTimeout",
			constant:  DefaultConnTimeout,
			set:       func(o *Options, d time.Duration) { o.ConnTimeout = d },
			resolved:  func(s *Server) time.Duration { return s.opts.ConnTimeout },
		},
		{
			name:      "MaxTxIdleTime",
			constName: "DefaultMaxTxIdleTime",
			constant:  DefaultMaxTxIdleTime,
			set:       func(o *Options, d time.Duration) { o.MaxTxIdleTime = d },
			resolved:  func(s *Server) time.Duration { return s.opts.MaxTxIdleTime },
		},
		{
			name:      "DefaultTxTimeout",
			constName: "DefaultTxTimeout",
			constant:  DefaultTxTimeout,
			set:       func(o *Options, d time.Duration) { o.DefaultTxTimeout = d },
			resolved:  func(s *Server) time.Duration { return s.opts.DefaultTxTimeout },
		},
		{
			name:      "DefaultStatementTimeout",
			constName: "DefaultStatementTimeout",
			constant:  DefaultStatementTimeout,
			set:       func(o *Options, d time.Duration) { o.DefaultStatementTimeout = d },
			resolved:  func(s *Server) time.Duration { return s.opts.DefaultStatementTimeout },
		},
	}
}

// TestTimeoutDefaults_AreDisabled pins the four timeout defaults set to zero by
// rmp #2807 and proves that [NewServer] leaves them alone.
//
// Three clauses per field, because pinning the constant alone would not prove
// what a server actually runs with: a resolution block that filled the field
// from some other constant, or filled it unconditionally, would pass a
// constant-only test. So each field asserts
//
//   - the constant is zero — the bound is disabled;
//   - a zero-value Options resolves to that same zero (no fill happens);
//   - an explicit operator value survives resolution UNCHANGED — the clause that
//     catches a resolution block which still overwrites a caller's value.
func TestTimeoutDefaults_AreDisabled(t *testing.T) {
	t.Parallel()

	// The sentinel must differ from the default, or the passthrough clause could
	// not tell a preserved value from a filled one.
	const explicit = 97 * time.Second
	if explicit == wantTimeoutDefault {
		t.Fatalf("test bug: explicit sentinel %v equals the default under test", explicit)
	}

	for _, tc := range timeoutDefaultCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			t.Run("constant-is-disabled", func(t *testing.T) {
				t.Parallel()
				if tc.constant != wantTimeoutDefault {
					t.Fatalf("%s constant = %v; want %v (rmp #2807 disabled the four server timeout defaults, matching PostgreSQL's statement_timeout / transaction_timeout / idle_in_transaction_session_timeout / idle_session_timeout, all 0)",
						tc.constName, tc.constant, wantTimeoutDefault)
				}
			})

			t.Run("zero-options-stays-zero", func(t *testing.T) {
				t.Parallel()
				srv, err := NewServer(newTestEngine(t), Options{Auth: NoAuthHandler{}})
				if err != nil {
					t.Fatalf("NewServer: %v", err)
				}
				if got := tc.resolved(srv); got != wantTimeoutDefault {
					t.Fatalf("resolved %s from a zero-value Options = %v; want %v (NewServer must install NO default for this field)",
						tc.name, got, wantTimeoutDefault)
				}
			})

			t.Run("explicit-value-preserved-verbatim", func(t *testing.T) {
				t.Parallel()
				opts := Options{Auth: NoAuthHandler{}}
				tc.set(&opts, explicit)
				srv, err := NewServer(newTestEngine(t), opts)
				if err != nil {
					t.Fatalf("NewServer: %v", err)
				}
				if got := tc.resolved(srv); got != explicit {
					t.Fatalf("resolved %s from an explicit %v = %v; want the operator value preserved verbatim",
						tc.name, explicit, got)
				}
			})
		})
	}
}

// TestTimeoutOptions_NegativeIsRefused pins the third arm of the contract: a
// negative duration is INVALID and [NewServer] refuses it.
//
// It used to be coerced. `if opts.X <= 0 { opts.X = DefaultX }` swallowed a
// negative and installed a bound the caller never asked for, which hid the
// caller's mistake twice over — the value was wrong, and the server then ran
// with a number that appeared nowhere in the caller's code. Now that zero
// already means "no bound", a negative has no remaining meaning to coerce it to.
func TestTimeoutOptions_NegativeIsRefused(t *testing.T) {
	t.Parallel()

	const negative = -1 * time.Millisecond

	for _, tc := range timeoutDefaultCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := Options{Auth: NoAuthHandler{}}
			tc.set(&opts, negative)
			srv, err := NewServer(newTestEngine(t), opts)
			if err == nil {
				t.Fatalf("NewServer accepted Options.%s = %v and returned a server; a negative timeout must be refused (srv=%p)",
					tc.name, negative, srv)
			}
			if srv != nil {
				t.Errorf("NewServer returned a non-nil server alongside the refusal for Options.%s", tc.name)
			}
			if !errors.Is(err, ErrNegativeTimeout) {
				t.Fatalf("NewServer error for Options.%s = %v is %v; want it to unwrap to ErrNegativeTimeout",
					tc.name, negative, err)
			}
			var nte *NegativeTimeoutError
			if !errors.As(err, &nte) {
				t.Fatalf("NewServer error for Options.%s is %v; want a *NegativeTimeoutError reachable with errors.As",
					tc.name, err)
			}
			if nte.Field != tc.name {
				t.Errorf("*NegativeTimeoutError.Field = %q; want %q (the error must name the offending field)", nte.Field, tc.name)
			}
			if nte.Value != negative {
				t.Errorf("*NegativeTimeoutError.Value = %v; want %v (the error must carry the value the caller supplied)", nte.Value, negative)
			}
		})
	}
}

// TestTimeoutOptions_EveryNegativeFieldIsReported pins that a caller who got
// more than one field wrong is told about ALL of them at once, rather than
// having to fix them one NewServer call at a time. The checks are independent
// and cost nothing, so reporting only the first would be a gratuitous round
// trip.
func TestTimeoutOptions_EveryNegativeFieldIsReported(t *testing.T) {
	t.Parallel()

	cases := timeoutDefaultCases()
	opts := Options{Auth: NoAuthHandler{}}
	for i, tc := range cases {
		// A distinct value per field, so a message that named the right field
		// with the wrong value would still be caught.
		tc.set(&opts, -time.Duration(i+1)*time.Millisecond)
	}

	_, err := NewServer(newTestEngine(t), opts)
	if err == nil {
		t.Fatal("NewServer accepted four negative timeouts; want a refusal")
	}
	msg := err.Error()
	for i, tc := range cases {
		want := -time.Duration(i+1) * time.Millisecond
		if !strings.Contains(msg, "Options."+tc.name) || !strings.Contains(msg, want.String()) {
			t.Errorf("the joined error does not report Options.%s = %v; got:\n%s", tc.name, want, msg)
		}
	}
}

// TestTimeoutOptions_ValidationPrecedesTheAuthCheck pins the order in which
// [NewServer] refuses a doubly-invalid Options. Both refusals are hard — no
// server is constructed either way, so nothing about the security posture turns
// on the order — but leaving it unpinned would let it drift silently, and a
// caller who sees "Options.ConnTimeout is -1ms" first should keep seeing it.
func TestTimeoutOptions_ValidationPrecedesTheAuthCheck(t *testing.T) {
	t.Parallel()

	// Auth deliberately left nil, which on its own yields ErrNoAuthHandler.
	_, err := NewServer(newTestEngine(t), Options{ConnTimeout: -time.Second})
	if !errors.Is(err, ErrNegativeTimeout) {
		t.Fatalf("NewServer(nil Auth, negative ConnTimeout) = %v; want the negative-timeout refusal to come first", err)
	}
	if errors.Is(err, ErrNoAuthHandler) {
		t.Fatalf("NewServer reported ErrNoAuthHandler as well as the timeout refusal: %v", err)
	}

	// Control: with the timeouts valid, the auth refusal is what surfaces —
	// without this, the clause above would pass on a NewServer that had stopped
	// checking Auth at all.
	if _, err := NewServer(newTestEngine(t), Options{}); !errors.Is(err, ErrNoAuthHandler) {
		t.Fatalf("NewServer(zero Options) = %v; want ErrNoAuthHandler", err)
	}
}
