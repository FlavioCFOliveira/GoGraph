package server

import (
	"testing"
	"time"
)

// wantTimeoutDefault is the value the three server-side timeout defaults were
// raised to by rmp #2806, written as a LITERAL on purpose.
//
// Writing it as `DefaultTxTimeout` and friends would make the assertions below
// compare each constant with itself, which cannot fail: the test would stay green
// against the values that preceded this change (DefaultMaxTxIdleTime 5 s,
// DefaultTxTimeout 30 s, DefaultStatementTimeout 30 s) and against any later
// unintended edit. The literal is what gives the gate teeth — verified by
// reverting each constant in turn, which fails both the constant clause and the
// resolution clause for that field.
const wantTimeoutDefault = 30 * time.Minute

// timeoutDefaultCase names one of the three defaults, the constant that carries
// it, and the two halves of the resolution block in [NewServer] that must apply
// it: the zero-value fill and the explicit-value passthrough.
type timeoutDefaultCase struct {
	// name is the Options field, which is also the subtest name.
	name string
	// constName is the exported constant that supplies the field's default.
	constName string
	// constant is the exported package constant under test.
	constant time.Duration
	// set writes an explicit value into an Options about to be handed to
	// NewServer, so the passthrough half can be exercised.
	set func(*Options, time.Duration)
	// resolved reads back what NewServer stored on the server.
	resolved func(*Server) time.Duration
}

func timeoutDefaultCases() []timeoutDefaultCase {
	return []timeoutDefaultCase{
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

// TestTimeoutDefaults_AreThirtyMinutes pins the three server-side timeout
// defaults raised by rmp #2806 and proves that [NewServer]'s resolution block
// actually installs them.
//
// Three clauses per field, because pinning the constant alone would not prove
// the value ever reaches a server: a resolution block that filled the field from
// some other constant, or filled it unconditionally, would pass a constant-only
// test. So each field asserts
//
//   - the constant equals 30 minutes;
//   - a zero-value Options is resolved to that same 30 minutes (the fill half);
//   - an explicit operator value survives resolution unchanged (the passthrough
//     half — which is also what makes the fill clause meaningful, since a block
//     that overwrote everything would fail here).
//
// The godoc on each constant states what the raised bound costs; this test is
// what stops the code and that godoc from drifting apart silently.
func TestTimeoutDefaults_AreThirtyMinutes(t *testing.T) {
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

			t.Run("constant", func(t *testing.T) {
				t.Parallel()
				if tc.constant != wantTimeoutDefault {
					t.Fatalf("%s constant = %v; want %v (rmp #2806 raised the three server timeout defaults to 30 minutes)",
						tc.constName, tc.constant, wantTimeoutDefault)
				}
			})

			t.Run("zero-options-resolved-to-default", func(t *testing.T) {
				t.Parallel()
				srv, err := NewServer(newTestEngine(t), Options{Auth: NoAuthHandler{}})
				if err != nil {
					t.Fatalf("NewServer: %v", err)
				}
				if got := tc.resolved(srv); got != wantTimeoutDefault {
					t.Fatalf("resolved %s from a zero-value Options = %v; want %v (NewServer's resolution block must install the raised default)",
						tc.name, got, wantTimeoutDefault)
				}
			})

			t.Run("explicit-value-preserved", func(t *testing.T) {
				t.Parallel()
				opts := Options{Auth: NoAuthHandler{}}
				tc.set(&opts, explicit)
				srv, err := NewServer(newTestEngine(t), opts)
				if err != nil {
					t.Fatalf("NewServer: %v", err)
				}
				if got := tc.resolved(srv); got != explicit {
					t.Fatalf("resolved %s from an explicit %v = %v; want the operator value preserved",
						tc.name, explicit, got)
				}
			})
		})
	}
}
