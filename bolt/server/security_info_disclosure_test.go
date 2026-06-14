package server

// security_info_disclosure_test.go — DEFENSE LOCK-IN against internal-state
// disclosure on the client-facing error path (security audit, info-disclosure
// cluster).
//
// Every error that reaches a Bolt client passes through [Session.sanitiseErr].
// Its contract: auth failures collapse to a fixed string; client-fault errors
// (parse/semantic/type errors, constraint violations, resource caps) forward
// their own diagnostic verbatim because that text describes the CLIENT's
// request, not server internals; everything else is replaced with a generic
// "internal error … (session: …)" line. The security property the audit
// requires is that NO client-visible message ever leaks server internals: a
// filesystem path, a Go type/package token, a source location, or a goroutine
// dump.
//
// This file pins that property two ways: (1) a direct sweep of sanitiseErr over
// representative induced errors, asserting the output is free of disclosure
// markers; and (2) an end-to-end check that a RUN whose evaluation fails inside
// the engine produces a FAILURE whose message is equally clean. Property-based
// fuzzing over synthetic internal errors (pgregory.net/rapid) widens the sweep
// so a future error wrapped with an embedded path is caught.
//
// Layer: short; handlers are driven directly, no sockets.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"

	"pgregory.net/rapid"
)

// secBoltDisclosureMarkers are substrings whose presence in a client-visible
// message would betray server internals. The session ID embedded in the generic
// message is a deliberate, safe correlation token and is excluded by
// construction (it is hex, contains none of these markers).
var secBoltDisclosureMarkers = []struct {
	name   string
	marker string
}{
	{"filesystem_path", "/"},
	{"windows_path", `\`},
	{"go_source_location", ".go:"},
	{"go_pointer_type", "*github.com/"},
	{"go_package_path", "github.com/FlavioCFOliveira/GoGraph"},
	{"goroutine_dump", "goroutine "},
	{"stack_marker", "runtime."},
	{"panic_marker", "panic:"},
}

// secBoltAssertClean fails if msg contains any disclosure marker. where names
// the call site for the failure message.
func secBoltAssertClean(t *testing.T, where, msg string) {
	t.Helper()
	for _, m := range secBoltDisclosureMarkers {
		if strings.Contains(msg, m.marker) {
			t.Errorf("%s: client-visible message leaks %s (%q): %q", where, m.name, m.marker, msg)
		}
	}
}

// TestSec_Bolt_SanitiseErrNoInternalDisclosure sweeps sanitiseErr over a set of
// errors that imitate the shapes the engine and storage layers really produce —
// each carrying a path, a type name, or a stack-like token in its raw text —
// and asserts the sanitised output discloses none of them. An internal error
// must collapse to the generic message; an auth error to the fixed line.
func TestSec_Bolt_SanitiseErrNoInternalDisclosure(t *testing.T) {
	t.Parallel()

	sess := newSession(newTestEngine(t), NoAuthHandler{}, "")

	cases := []struct {
		name        string
		err         error
		wantGeneric bool // true: must become the generic internal-error line
	}{
		{
			name:        "internal_error_with_path",
			err:         errors.New("store/wal: fsync /var/lib/gograph/data/000123.wal: input/output error"),
			wantGeneric: true,
		},
		{
			name:        "internal_error_with_type_and_source",
			err:         fmt.Errorf("recovery: %w", errors.New("*store.snapshotReader at recovery.go:858: short read")),
			wantGeneric: true,
		},
		{
			name:        "internal_error_with_goroutine_dump",
			err:         errors.New("goroutine 42 [running]:\nruntime.gopanic(...)\n\t/usr/local/go/src/runtime/panic.go:914"),
			wantGeneric: true,
		},
		{
			name:        "auth_failure_collapses",
			err:         fmt.Errorf("validate alice against /etc/gograph/passwd: %w", ErrAuthFailed),
			wantGeneric: false, // becomes the fixed "Authentication failed." line
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sess.sanitiseErr(tc.err)
			secBoltAssertClean(t, tc.name, got)
			if errors.Is(tc.err, ErrAuthFailed) {
				if got != "Authentication failed." {
					t.Fatalf("auth error: got %q, want the fixed \"Authentication failed.\" line", got)
				}
				return
			}
			if tc.wantGeneric {
				// The generic line names only a session ID for log correlation.
				if !strings.Contains(got, "internal error") || !strings.Contains(got, sess.id) {
					t.Fatalf("internal error: got %q, want the generic internal-error line carrying the session ID", got)
				}
			}
		})
	}
}

// TestSec_Bolt_RunFailureMessageClean drives a real failing RUN end-to-end at
// the handler level and asserts the resulting FAILURE message carries no
// disclosure marker. A parse error is a client fault, so its own (safe)
// diagnostic is forwarded; the test confirms even that forwarded text contains
// no path/type/stack token.
func TestSec_Bolt_RunFailureMessageClean(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sess := newSession(newTestEngine(t), NoAuthHandler{}, "")
	if msgs, err := sess.HandleMessage(ctx, helloMsg()); err != nil || !isSuccess(msgs) {
		t.Fatalf("HELLO: msgs=%#v err=%v", msgs, err)
	}

	cases := []struct {
		name  string
		query string
	}{
		{"syntax_error", "THIS IS NOT CYPHER"},
		{"semantic_undefined_var", "RETURN x"},
		{"type_error_property_on_int", "RETURN (5).foo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh authenticated session per case: a failed RUN moves the
			// session to FAILED, so reusing it would reject the next RUN for the
			// wrong reason.
			s := newSession(newTestEngine(t), NoAuthHandler{}, "")
			if msgs, err := s.HandleMessage(ctx, helloMsg()); err != nil || !isSuccess(msgs) {
				t.Fatalf("HELLO: msgs=%#v err=%v", msgs, err)
			}
			got, err := s.HandleMessage(ctx, &proto.Run{Query: tc.query, Extra: map[string]packstream.Value{}})
			if err != nil {
				t.Fatalf("RUN(%q): unexpected transport error %v", tc.query, err)
			}
			if !isFailure(got) {
				t.Fatalf("RUN(%q): got %#v, want a FAILURE", tc.query, got)
			}
			f := got[0].(*proto.Failure)
			secBoltAssertClean(t, tc.name, f.Message)
			// The code must be a Neo.* status, never an internal token.
			if !strings.HasPrefix(f.Code, "Neo.") {
				t.Errorf("%s: failure code %q is not a Neo.* status", tc.name, f.Code)
			}
		})
	}
}

// TestSec_Bolt_SanitiseInternalErrorPropertyClean is the property-based widening
// of the sweep: for any synthetic internal error built by embedding a random
// path-, type-, and source-like fragment, sanitiseErr must produce the generic
// line with no disclosure marker. rapid shrinks any counterexample to a minimal
// offending fragment.
func TestSec_Bolt_SanitiseInternalErrorPropertyClean(t *testing.T) {
	t.Parallel()
	sess := newSession(newTestEngine(t), NoAuthHandler{}, "")

	rapid.Check(t, func(rt *rapid.T) {
		// A fragment that mixes disclosure-shaped tokens; sanitiseErr must hide it.
		frag := rapid.StringMatching(`[a-zA-Z0-9_./:\\*-]{0,40}`).Draw(rt, "frag")
		internal := errors.New("store internal failure at " + frag + ".go:" + frag + " /tmp/" + frag)
		got := sess.sanitiseErr(internal)

		for _, m := range secBoltDisclosureMarkers {
			if strings.Contains(got, m.marker) {
				rt.Fatalf("sanitised internal error leaked %s (%q): input=%q output=%q",
					m.name, m.marker, internal.Error(), got)
			}
		}
		if !strings.Contains(got, "internal error") {
			rt.Fatalf("sanitised internal error is not the generic line: %q", got)
		}
	})
}
