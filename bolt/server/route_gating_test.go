package server_test

// route_gating_test.go — #1225 (security fix L4, LOW).
//
// Two related hardening behaviours of the Bolt serve loop are exercised here:
//
//  1. ROUTE is rejected before HELLO. An unauthenticated client in
//     StateNegotiation must not elicit a routing-table response; ROUTE is only
//     accepted once the session has completed HELLO (and LOGON on Bolt >= 5.1),
//     i.e. from READY/TX_READY. After a successful HELLO, ROUTE still returns
//     the routing table (regression guard for legitimate drivers).
//
//  2. A malformed/undecodable request yields a sanitised FAILURE — a generic
//     message under the Neo.ClientError.Request.Invalid code — rather than the
//     raw internal framing error string (e.g. "proto: unknown request tag 0x..").
//
// All tests run over the wire via the shared boltTestClient helpers so the full
// serve loop (decode + dispatch + sanitise) is exercised, and they respect the
// goleak TestMain by closing every connection they open.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// TestServe_RouteBeforeHello_Rejected verifies that a ROUTE sent in
// StateNegotiation (before HELLO) is rejected with a FAILURE and does NOT
// return a routing table to an unauthenticated client.
func TestServe_RouteBeforeHello_Rejected(t *testing.T) {
	t.Parallel()

	c := newBoltTestClient(t, sharedServerAddr)
	defer c.close(t)

	c.negotiate(t)

	// Send ROUTE before HELLO. The server is in StateNegotiation and must reject
	// it as an illegal transition rather than serving the routing table.
	c.sendRequest(t, &proto.Route{
		Routing:   map[string]packstream.Value{},
		Bookmarks: nil,
		DB:        nil,
	})

	f := c.recvFailure(t)
	if f.Code != "Neo.ClientError.Request.Invalid" {
		t.Errorf("pre-HELLO ROUTE failure code: got %q, want Neo.ClientError.Request.Invalid", f.Code)
	}
	// The FAILURE must not carry a routing table; recvFailure already asserts the
	// message is a FAILURE (not a SUCCESS with "rt"), so reaching here means the
	// client received no routing-table response.
	t.Logf("pre-HELLO ROUTE rejected: code=%q message=%q", f.Code, f.Message)
}

// TestServe_RouteAfterHello_ReturnsTable is the regression guard: ROUTE sent
// after a successful HELLO still returns the routing table, so routing keeps
// working for legitimate drivers (which complete HELLO/LOGON before ROUTE).
func TestServe_RouteAfterHello_ReturnsTable(t *testing.T) {
	t.Parallel()

	c := newBoltTestClient(t, sharedServerAddr)
	defer c.close(t)

	c.negotiate(t)
	c.hello(t)

	succ := c.route(t)
	if succ == nil {
		t.Fatal("post-HELLO ROUTE returned nil SUCCESS")
	}
	rt, ok := succ.Metadata["rt"]
	if !ok {
		t.Fatalf("post-HELLO ROUTE SUCCESS metadata missing 'rt': %#v", succ.Metadata)
	}
	if _, ok := rt.(map[string]packstream.Value); !ok {
		t.Fatalf("post-HELLO ROUTE 'rt' type: %T, want map[string]packstream.Value", rt)
	}
	t.Log("post-HELLO ROUTE returned a routing table")
}

// TestServe_DecodeError_Sanitised verifies that an undecodable request (a
// well-framed PackStream struct carrying an unknown Bolt message tag) yields a
// sanitised FAILURE: a generic message under Neo.ClientError.Request.Invalid,
// NOT the raw internal "proto: unknown request tag 0x.." string. The connection
// is not torn down by the decode-error path, so a follow-up GOODBYE still
// completes cleanly.
func TestServe_DecodeError_Sanitised(t *testing.T) {
	t.Parallel()

	c := newBoltTestClient(t, sharedServerAddr)
	defer c.close(t)

	c.negotiate(t)
	c.hello(t)

	// Craft a structurally valid PackStream message with an unknown Bolt request
	// tag (0x55 is not assigned to any request message), so DecodeRequest fails
	// with "proto: unknown request tag 0x55" without any packstream framing
	// error. This drives the serve-loop decode-error branch.
	const unknownTag = 0x55
	c.sendRawStruct(t, unknownTag)

	f := c.recvFailure(t)
	if f.Code != "Neo.ClientError.Request.Invalid" {
		t.Errorf("decode-error failure code: got %q, want Neo.ClientError.Request.Invalid", f.Code)
	}
	// The raw decoder error text must not leak to the client.
	if strings.Contains(f.Message, "proto:") || strings.Contains(f.Message, "unknown request tag") {
		t.Errorf("decode-error message leaks internal framing detail: %q", f.Message)
	}
	// A decode failure is a CLIENT fault (a malformed/undecodable frame), so the
	// message must be honest about that rather than the generic internal-error
	// text, which wrongly implies a server bug (task #1435). The fixed string
	// names the fault without leaking framing internals.
	if strings.Contains(f.Message, "An internal error occurred") {
		t.Errorf("decode-error message wrongly uses the internal-error text: %q", f.Message)
	}
	if f.Message != "malformed Bolt message" {
		t.Errorf("decode-error message: got %q, want %q", f.Message, "malformed Bolt message")
	}
	t.Logf("decode-error sanitised: code=%q message=%q", f.Code, f.Message)

	// The serve loop continues after a decode error: GOODBYE still works.
	c.goodbye(t)
}

// sendRawStruct writes a PackStream struct header with the given tag and zero
// fields onto the client's chunked writer, producing a well-framed but
// semantically unknown Bolt request message. It bypasses proto.EncodeRequest
// (which only encodes known message types) so the test can drive the
// serve-loop decode-error path.
func (c *boltTestClient) sendRawStruct(t *testing.T, tag byte) {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	if err := enc.WriteStructHeader(tag, 0); err != nil {
		t.Fatalf("sendRawStruct WriteStructHeader(0x%02X): %v", tag, err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("sendRawStruct flush: %v", err)
	}
	if err := c.cw.WriteMessage(buf.Bytes()); err != nil {
		t.Fatalf("sendRawStruct write: %v", err)
	}
}
