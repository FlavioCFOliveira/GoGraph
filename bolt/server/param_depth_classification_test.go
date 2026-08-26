package server_test

// param_depth_classification_test.go — rmp #2570: a parameter nested deeper than
// the engine's bind cap is a CLIENT fault and must not be reported as an internal
// server bug.
//
// Before the fix the cap returned a bare fmt.Errorf that FailureCode could not
// recognise, so it fell through to Neo.DatabaseError.General.UnknownError and the
// sanitiser replaced the text with "An internal error occurred. See server logs
// for details". The payload is entirely client-supplied and entirely malformed
// from the server's point of view, so that code told an operator GoGraph had a
// defect when the client had sent something invalid.
//
// # Why ArgumentError, and why the session still ends FAILED
//
// The CODE is Neo.ClientError.Statement.ArgumentError, reached through the
// module's own TCK-pinned convention: an engine error whose message carries
// "cypher: ArgumentError." is classified by FailureCode without any bolt/server
// change (errors.go). It is the Statement family rather than the Request family
// because the message DECODED correctly and the statement was dispatched — what
// is invalid is the statement's argument, not the form of the request. The
// packstream wire nesting cap answers Neo.ClientError.Request.Invalid for a frame
// that will not decode at all, which is a different layer and a different fault.
//
// The SESSION still ends FAILED, and that is not an oversight. Transition's
// documented exception for staying READY is back-pressure — "the slot frees when
// another of the principal's transactions closes, so the right response is to
// retry the same BEGIN" (state.go). A depth refusal is deterministic: retrying the
// identical RUN fails identically forever. It is a genuine failure to carry out
// the request, so FAILED is what the Bolt state machine specifies. The two
// neighbouring caps stay READY for the OTHER reason — they refuse during decode in
// the serve loop, before the message ever reaches a session, so there is no
// request to fail.
//
// The live three-vector comparison — that the aggregate decode pool, the wire
// nesting cap and this cap all answer DIFFERENTLY — is owned by the rmp #2487 DST
// scenario (internal/sim/bolt_decode_pressure.go), which drives all three against
// a real server. This file owns the bind cap: both chain shapes, the exact
// boundary, the code, the message, and the session state.

import (
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// paramBindDepthCap mirrors cypher's maxParamBindDepth. It is restated rather
// than imported because the constant is unexported: a change to it moves the
// boundary these tests drive, and this mirror makes that change fail here by name
// instead of silently shifting what is being measured.
const paramBindDepthCap = 32

// The reply a client must receive. The message is pinned in FULL: the code alone
// would be satisfied by an ArgumentError that said something useless, and the
// whole point of the fix is that the client is told what is wrong. It also pins
// the ABSENCE of the accumulated bind path — before the fix the wrapped error was
// 405 bytes for a 33-deep map chain, of which 396 were `map["k"]:` framing, and a
// ClientError code bypasses the sanitiser, so all of it would have been forwarded.
const (
	wantParamDepthCode    = "Neo.ClientError.Statement.ArgumentError"
	wantParamDepthMessage = `cypher: ArgumentError.ParameterNestedTooDeep: parameter "p" is nested deeper than the supported limit of 32 levels`
)

// nestListChain returns a chain of `depth` nested lists around a scalar.
func nestListChain(depth int) packstream.Value {
	var v packstream.Value = int64(1)
	for i := 0; i < depth; i++ {
		v = []packstream.Value{v}
	}
	return v
}

// nestMapChain returns a chain of `depth` nested maps around a scalar. Both shapes
// are driven because the binder reaches the cap through two distinct code paths —
// the concrete []any / map[string]any type switch and the reflected fallback — and
// the cap is checked in all three of them.
func nestMapChain(depth int) packstream.Value {
	var v packstream.Value = int64(1)
	for i := 0; i < depth; i++ {
		v = map[string]packstream.Value{"k": v}
	}
	return v
}

// runNestedParam sends one RUN carrying a nested parameter under the key "p" and
// returns the reply, plus what the session answers to a SECOND message — which is
// how the session's state is read from the client side: IGNORED means FAILED.
func runNestedParam(t *testing.T, addr string, v packstream.Value) (reply, second any) {
	t.Helper()
	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)
	c.sendRequest(t, &proto.Run{
		Query:      "RETURN $p AS p",
		Parameters: map[string]packstream.Value{"p": v},
	})
	reply = c.recvResponse(t)
	c.sendRequest(t, &proto.Run{Query: "RETURN 1"})
	return reply, c.recvResponse(t)
}

// TestParamDepth_OverNestedParameterIsAClientFault drives both chain shapes one
// level past the cap and pins the whole reply.
func TestParamDepth_OverNestedParameterIsAClientFault(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t, server.Options{ConnTimeout: 30 * time.Second})

	for _, tc := range []struct {
		name  string
		build func(int) packstream.Value
	}{
		{"list chain", nestListChain},
		{"map chain", nestMapChain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, second := runNestedParam(t, addr, tc.build(paramBindDepthCap+1))
			f, ok := reply.(*proto.Failure)
			if !ok {
				t.Fatalf("a %d-deep parameter drew %T, want a FAILURE", paramBindDepthCap+1, reply)
			}
			if f.Code != wantParamDepthCode {
				t.Errorf("code = %q, want %q — a client-supplied payload the engine will not bind is a\n"+
					"CLIENT fault, and a DatabaseError tells an operator GoGraph has a defect (rmp #2570)",
					f.Code, wantParamDepthCode)
			}
			if f.Message != wantParamDepthMessage {
				t.Errorf("message = %q,\nwant %q", f.Message, wantParamDepthMessage)
			}
			// The accumulated bind path must NOT be forwarded. Asserted separately
			// from the exact-message check so a future message change cannot
			// reintroduce the leak while still looking like a deliberate edit.
			if strings.Contains(f.Message, `map["k"]`) || strings.Contains(f.Message, "list[") {
				t.Errorf("the message forwards the internal bind path: %q", f.Message)
			}
			// FAILED, read from the client side: a second request-phase message is
			// IGNORED until RESET.
			if _, ok := second.(*proto.Ignored); !ok {
				t.Errorf("the second message drew %T, want *proto.Ignored — a deterministic refusal is a\n"+
					"failure to carry out the request, not back-pressure, so the Bolt state machine puts the\n"+
					"session in FAILED", second)
			}
		})
	}
}

// TestParamDepth_TheBoundaryIsUnchanged brackets the cap. A fix that classified
// correctly but moved the limit would pass the test above and break every client
// whose payload sits at the boundary.
func TestParamDepth_TheBoundaryIsUnchanged(t *testing.T) {
	t.Parallel()
	addr := startTestServer(t, server.Options{ConnTimeout: 30 * time.Second})

	for _, tc := range []struct {
		name  string
		build func(int) packstream.Value
	}{
		{"list chain", nestListChain},
		{"map chain", nestMapChain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// AT the cap: accepted.
			reply, _ := runNestedParam(t, addr, tc.build(paramBindDepthCap))
			if _, ok := reply.(*proto.Success); !ok {
				t.Errorf("a parameter at exactly the cap (%d) drew %T, want SUCCESS: the boundary moved",
					paramBindDepthCap, reply)
			}
			// One PAST the cap: refused. Both halves are needed — a server that
			// refused everything would satisfy the refusal half alone.
			reply, _ = runNestedParam(t, addr, tc.build(paramBindDepthCap+1))
			if _, ok := reply.(*proto.Failure); !ok {
				t.Errorf("a parameter one past the cap (%d) drew %T, want a FAILURE",
					paramBindDepthCap+1, reply)
			}
		})
	}
}

// TestParamDepth_ItIsNotTheDecodeLayerCode pins the bind cap apart from the two
// decode-layer refusals it must not be confused with. It asserts the codes DIFFER
// rather than re-driving those caps, which the rmp #2487 DST scenario does against
// a live server: the wire nesting cap cannot be driven from here at all, because
// the test client's own encoder enforces the same symmetric bound and refuses to
// serialise a 129-deep payload.
func TestParamDepth_ItIsNotTheDecodeLayerCode(t *testing.T) {
	t.Parallel()
	for _, other := range []struct{ name, code string }{
		{"the wire nesting cap", "Neo.ClientError.Request.Invalid"},
		{"the aggregate decode pool", "Neo.TransientError.General.OutOfMemoryError"},
		{"the pre-fix answer", "Neo.DatabaseError.General.UnknownError"},
	} {
		if wantParamDepthCode == other.code {
			t.Errorf("the parameter bind cap answers %q, the same as %s — the three vectors must stay\n"+
				"distinguishable, since only one of them is retryable and only one is a server fault",
				wantParamDepthCode, other.name)
		}
	}
}
