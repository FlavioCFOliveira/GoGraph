package server_test

// named_client_faults_wire_test.go — regression gate for rmp #2819.
//
// Three conditions the server could NAME reached the Bolt client as
// "An internal error occurred. See server logs for details (session: …)".
// The server wrote the real cause to its own stderr and sent the client a
// session id, so the one party able to fix the statement was the one party not
// told what was wrong with it. Reported by an external application on
// 2026-09-15 against the over-long label:
//
//	engine: field too long for its WAL length prefix: label is 65536 bytes, maximum 65535
//	client: An internal error occurred. See server logs for details (session: c5d92384…)
//
// The three, each measured over a real Bolt socket at df0b1866 before the fix:
//
//	long label / property key  txn.ErrFieldTooLong          -> UnknownError, masked
//	unsupported DDL            "cypher: DDL parse: …"       -> UnknownError, masked
//	presence constraint        exec.ErrConstraintViolation  -> UnknownError, masked
//
// And the asymmetry that made it unpredictable rather than merely unhelpful: a
// UNIQUE violation already arrived intact, under
// Neo.ClientError.Schema.ConstraintValidationFailed, so a client could not tell
// from the outside which refusals explain themselves. TestNamedClientFault_
// UniquenessControlIsUnchanged pins that arm so the fix is not mistaken for
// what made the other three work.
//
// Layer: short. Driven over the wire rather than through FailureCode directly,
// because the claim is about what the CLIENT receives: the code AND the message
// together, after Session.sanitiseErr has decided whether to forward it.
//
// The engine is WAL-backed (newWALEngine): txn.ErrFieldTooLong is raised by
// store/txn as a field is staged, so a store-less engine cannot reach it.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// maskedErrFragment is the leading text of the generic message sanitiseErr
// substitutes for a SERVER fault. Its presence in a FAILURE is exactly the
// defect this file gates: the engine knew the cause and the client was told
// nothing.
const maskedErrFragment = "An internal error occurred"

// runForFailure sends query on a fresh session and returns the FAILURE it
// earns, whether that arrives in answer to the RUN or to the following PULL.
// Which of the two carries it is not part of any claim here — a DDL refusal
// fails the RUN, while a constraint violation detected at commit fails the PULL
// — so the helper accepts either and the tests assert on the FAILURE itself.
func runForFailure(t *testing.T, addr, query string, setup ...string) *proto.Failure {
	t.Helper()
	c := newBoltTestClient(t, addr)
	t.Cleanup(func() { c.close(t) })
	c.negotiate(t)
	c.hello(t)
	for _, s := range setup {
		c.run(t, s, nil)
		c.pullAll(t)
	}
	c.sendRequest(t, &proto.Run{
		Query:      query,
		Parameters: map[string]packstream.Value{},
		Extra:      map[string]packstream.Value{},
	})
	for i := 0; i < 3; i++ {
		switch m := c.recvResponse(t).(type) {
		case *proto.Failure:
			return m
		case *proto.Success:
			c.sendRequest(t, &proto.Pull{N: -1, QID: -1})
		case *proto.Record:
			// keep draining
		default:
			t.Fatalf("%q: unexpected response %T, want FAILURE", query, m)
		}
	}
	t.Fatalf("%q: no FAILURE after RUN and PULL; the statement was accepted", query)
	return nil
}

// assertDisclosesNothingInternal is the guard that justifies forwarding a
// message at all. sanitiseErr's generic fallback exists to keep internal detail
// off the wire, so every message this change newly forwards must be checked
// against the same standard — and checked here rather than by reading it once,
// so a later edit to any of these engine messages cannot quietly widen what the
// server discloses.
//
// The tokens are the shapes sanitiseErr's own doc names: a Go type name, a file
// path, stack detail, and the session correlation id that belongs only to the
// masked message. A package-qualified prefix ("txn: ", "exec: ", "ir: ") is NOT
// on the list and is deliberately allowed: it names the component that refused,
// carries no runtime state, and is already forwarded by the UNIQUE arm this
// module has shipped since task #1353.
func assertDisclosesNothingInternal(t *testing.T, label, msg string) {
	t.Helper()
	for _, bad := range []struct{ token, why string }{
		{maskedErrFragment, "the message was masked, not forwarded"},
		{"session: ", "a session correlation id belongs only to the masked message"},
		{".go", "a Go source file name"},
		{"/", "a path separator"},
		{"*", "a Go pointer-type name"},
		{"0x", "an address or raw pointer"},
		{"goroutine", "stack detail"},
		{"panic", "stack detail"},
		{"\n", "a multi-line message is a stack or a dump, not a diagnostic"},
	} {
		if strings.Contains(msg, bad.token) {
			t.Errorf("%s: forwarded message discloses %s (%s)\n  message: %q",
				label, bad.why, bad.token, msg)
		}
	}
}

// TestNamedClientFault_ReachesTheClientNamed is the acceptance for rmp #2819:
// each of the three conditions arrives under a Neo.ClientError.* code carrying
// the engine's own diagnostic, not the generic internal-error text.
func TestNamedClientFault_ReachesTheClientNamed(t *testing.T) {
	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{})

	// A schema string one byte past the uint16 length prefix every WAL op body
	// reserves for one. 65535 is the cap, so 65536 is the smallest refusal.
	overLong := strings.Repeat("L", 65536)

	cases := []struct {
		name     string
		setup    []string
		query    string
		wantCode string
		// wantFragment is a distinctive piece of the ENGINE's own message. Its
		// presence is what proves the message was forwarded rather than
		// replaced; asserting only "not the generic text" would pass on any
		// other substitution.
		wantFragment string
	}{
		{
			name:         "over-long node label",
			query:        "CREATE (n:" + overLong + ")",
			wantCode:     "Neo.ClientError.Statement.ArgumentError",
			wantFragment: "field too long for its WAL length prefix",
		},
		{
			name:         "over-long node property key",
			query:        "CREATE (n:Ok {`" + overLong + "`: 1})",
			wantCode:     "Neo.ClientError.Statement.ArgumentError",
			wantFragment: "field too long for its WAL length prefix",
		},
		{
			name:         "composite index",
			query:        "CREATE INDEX ci FOR (n:L) ON (n.a, n.b)",
			wantCode:     "Neo.ClientError.Statement.SyntaxError",
			wantFragment: "composite indexes (multiple properties) are not supported",
		},
		{
			name:         "index on a relationship property",
			query:        "CREATE INDEX ri FOR ()-[r:R]-() ON (r.p)",
			wantCode:     "Neo.ClientError.Statement.SyntaxError",
			wantFragment: "expected ':' in node pattern",
		},
		{
			name:         "IS NODE KEY constraint",
			query:        "CREATE CONSTRAINT nk FOR (n:L) REQUIRE (n.a, n.b) IS NODE KEY",
			wantCode:     "Neo.ClientError.Statement.SyntaxError",
			wantFragment: "composite constraints (multiple properties) are not supported",
		},
		{
			name:         "IS KEY constraint",
			query:        "CREATE CONSTRAINT kc FOR (n:L) REQUIRE n.a IS KEY",
			wantCode:     "Neo.ClientError.Statement.SyntaxError",
			wantFragment: "key constraints are not supported",
		},
		{
			name:         "constraint on a relationship property",
			query:        "CREATE CONSTRAINT rc FOR ()-[r:R]-() REQUIRE r.p IS NOT NULL",
			wantCode:     "Neo.ClientError.Statement.SyntaxError",
			wantFragment: "relationship constraints are not supported",
		},
		{
			// The presence-constraint arm. CREATE CONSTRAINT validates the
			// pre-existing data first, and its NOT NULL refusal wraps only
			// exec.ErrConstraintViolation — no typed error for errors.As to
			// recover, which is why this one fell through while the UNIQUE arm
			// below did not.
			name:         "presence constraint refused over pre-existing null data",
			setup:        []string{"CREATE (n:Q)"},
			query:        "CREATE CONSTRAINT nn2 FOR (n:Q) REQUIRE n.p IS NOT NULL",
			wantCode:     "Neo.ClientError.Schema.ConstraintValidationFailed",
			wantFragment: "cannot create NOT NULL constraint",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := runForFailure(t, addr, tc.query, tc.setup...)
			if f.Code != tc.wantCode {
				t.Errorf("code = %q, want %q\n  message: %q", f.Code, tc.wantCode, f.Message)
			}
			if !strings.HasPrefix(f.Code, "Neo.ClientError.") {
				t.Errorf("code = %q: a fault the client caused must not be reported "+
					"as a server fault", f.Code)
			}
			if !strings.Contains(f.Message, tc.wantFragment) {
				t.Errorf("message does not carry the engine's own diagnostic\n"+
					"  want fragment: %q\n  got message:   %q", tc.wantFragment, f.Message)
			}
			assertDisclosesNothingInternal(t, tc.name, f.Message)
		})
	}
}

// TestNamedClientFault_UniquenessControlIsUnchanged is the control. A UNIQUE
// violation already reached the client intact before rmp #2819; if the fix had
// worked by loosening what sanitiseErr forwards in general, this arm would move
// too. It must not.
func TestNamedClientFault_UniquenessControlIsUnchanged(t *testing.T) {
	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{})

	f := runForFailure(t, addr, "CREATE (n:U {p: 1})",
		"CREATE CONSTRAINT uq FOR (n:U) REQUIRE n.p IS UNIQUE",
		"CREATE (n:U {p: 1})")

	if f.Code != "Neo.ClientError.Schema.ConstraintValidationFailed" {
		t.Errorf("code = %q, want Neo.ClientError.Schema.ConstraintValidationFailed", f.Code)
	}
	if !strings.Contains(f.Message, "UNIQUE constraint on (U).p") {
		t.Errorf("message = %q, want the engine's own UNIQUE violation text", f.Message)
	}
	assertDisclosesNothingInternal(t, "unique violation", f.Message)
}

// TestNamedClientFault_ServerFaultIsStillMasked is the other control, and the
// one that keeps the change honest. sanitiseErr's generic fallback is a
// disclosure boundary, not dead weight: an error the server cannot attribute to
// the client must still reach the client as the generic text with a session id
// and nothing else. A change that classified too widely would show up here.
func TestNamedClientFault_ServerFaultIsStillMasked(t *testing.T) {
	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{})

	// boltboom() panics inside statement execution (registered by
	// explicit_tx_regression_test.go's init). The engine's panic boundary turns
	// it into an error that names an internal condition, which is precisely what
	// must NOT be forwarded.
	f := runForFailure(t, addr, "RETURN boltboom()")

	if f.Code != "Neo.DatabaseError.General.UnknownError" {
		t.Errorf("code = %q, want Neo.DatabaseError.General.UnknownError: an "+
			"unattributable fault must not be reported as the client's\n  message: %q",
			f.Code, f.Message)
	}
	if !strings.Contains(f.Message, maskedErrFragment) {
		t.Errorf("message = %q, want the generic internal-error text: the "+
			"disclosure boundary must still hold for a server fault", f.Message)
	}
}
