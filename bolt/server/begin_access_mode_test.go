package server

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// beginWithMode sends a BEGIN carrying the given `mode` extra (or none when mode
// is nil) and returns the reply.
func beginWithMode(t *testing.T, s *Session, mode any) []any {
	t.Helper()
	extra := map[string]interface{}{}
	if mode != nil {
		extra["mode"] = mode
	}
	msgs, err := s.HandleMessage(context.Background(), &proto.Begin{Extra: extra})
	if err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	return msgs
}

// failureOf returns the reply's *proto.Failure, or nil when the reply is not one.
func failureOf(msgs []any) *proto.Failure {
	if len(msgs) != 1 {
		return nil
	}
	f, _ := msgs[0].(*proto.Failure)
	return f
}

// TestBegin_UnrecognisedAccessModeIsRefused guards rmp #2564.
//
// The coercion read `if modeStr == "r" { mode = "r" }`, so EVERY other value
// fell through to the write default: a client that asked for read-only silently
// received WRITE authority and its subsequent writes succeeded. That is a
// fail-open on a field this server treats as a capability restriction — mode "r"
// makes the transaction read-only and refuses writes — and the project's
// fail-stop-never-fail-silent rule forbids resolving an unknown token in the
// MORE privileged direction.
//
// The Bolt specification is silent on invalid mode values and frames `mode` as a
// routing hint rather than authorisation, so the contract is GoGraph's to
// choose; refusing is the choice, recorded on the ticket with its sources.
func TestBegin_UnrecognisedAccessModeIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mode any
		// needle must appear in the refusal message, so the message names the
		// offending value rather than merely reporting that something was wrong.
		needle string
	}{
		{"uppercase R", "R", `"R"`},
		{"long form read", "read", `"read"`},
		{"long form write", "write", `"write"`},
		{"empty string", "", `""`},
		{"non-string integer", int64(0), "int64"},
		{"non-string bool", true, "bool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newReadySession(t)
			msgs := beginWithMode(t, s, tc.mode)

			f := failureOf(msgs)
			if f == nil {
				t.Fatalf("BEGIN with mode %#v returned %#v, want a FAILURE. Falling through to "+
					"the write default grants write authority to a client that did not ask for "+
					"it (rmp #2564)", tc.mode, msgs)
			}
			if f.Code != "Neo.ClientError.Request.Invalid" {
				t.Errorf("failure code = %q, want %q", f.Code, "Neo.ClientError.Request.Invalid")
			}
			if !strings.Contains(f.Message, tc.needle) {
				t.Errorf("failure message %q does not contain %q: it must NAME the offending "+
					"value so a client can fix its own request without guessing",
					f.Message, tc.needle)
			}
			// And no transaction may have been opened.
			if s.tx != nil {
				t.Errorf("a refused BEGIN left a transaction open")
			}
			if s.txActive {
				t.Errorf("a refused BEGIN left txActive set")
			}
		})
	}
}

// TestBegin_CanonicalAccessModesAreUnchanged is the control the negatives need.
// A guard that refused every BEGIN would satisfy every case above, so the two
// canonical spellings and the absent key are pinned to their present behaviour.
func TestBegin_CanonicalAccessModesAreUnchanged(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		mode     any
		wantMode string
	}{
		{"canonical r", "r", "r"},
		{"canonical w", "w", "w"},
		{"absent key defaults to write", nil, "w"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newReadySession(t)
			msgs := beginWithMode(t, s, tc.mode)

			if f := failureOf(msgs); f != nil {
				t.Fatalf("BEGIN with mode %#v was REFUSED (%s: %s); the canonical spellings and "+
					"the absent key must keep their behaviour", tc.mode, f.Code, f.Message)
			}
			if s.tx == nil {
				t.Fatalf("BEGIN with mode %#v opened no transaction", tc.mode)
			}
			if s.tx.mode != tc.wantMode {
				t.Errorf("transaction mode = %q, want %q", s.tx.mode, tc.wantMode)
			}
		})
	}
}
