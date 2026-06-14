package server_test

// security_bolt_noop_test.go — [SEC-2026-06-14b][BOLT] NOOP keep-alive handling.
//
// Bolt 4.1+ defines a standalone `00 00` chunk (a zero-length chunk that is NOT
// terminating an in-progress message) as a NOOP keep-alive. Per the Bolt
// Protocol Manual (neo4j.com/docs/bolt/current — Messaging/Chunking/NOOP) a
// conformant server MUST silently discard a NOOP: it carries zero PackStream
// bytes, it must never be decoded, and it must never be answered with RECORD /
// SUCCESS / FAILURE. Both peers may emit NOOPs at any time on a 4.1+ connection
// to keep idle intermediaries from dropping the TCP connection.
//
// GoGraph negotiates Bolt 4.4 and 5.0–5.6 (all >= 4.1), so NOOPs are in scope.
// The serve loop currently hands the empty reassembled message straight to
// proto.DecodeRequest, which fails on the empty buffer; the loop then replies
// with a FAILURE (Neo.ClientError.Request.Invalid, "malformed Bolt message").
// For a legitimate driver this FAILURE is unsolicited and is classified as a
// fatal, non-retryable ClientError: the driver moves the connection to FAILED
// and evicts it from the pool — so the keep-alive mechanism destroys the very
// idle connections it is meant to preserve. It is also a (minor) DoS
// amplification: one 2-byte NOOP coerces one full FAILURE response, and each
// ReadMessage refreshes the idle deadline, so the connection lives forever.
//
// CWE-20 (improper input validation / protocol non-conformance), with a
// CWE-400 (uncontrolled resource amplification) aspect.

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// recvWithin reads one chunked message within d, returning (msg, true) on a
// decoded response or (nil, false) on timeout / read error (i.e. the server
// correctly sent nothing).
func recvWithin(conn net.Conn, cr *proto.ChunkedReader, d time.Duration) (any, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(d))
	raw, err := cr.ReadMessage()
	if err != nil {
		return nil, false
	}
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, derr := proto.DecodeResponse(dec)
	if derr != nil {
		return nil, true // server sent bytes that did not decode as a response
	}
	return msg, true
}

// TestSec_NOOP_SilentlyIgnored asserts that a bare NOOP elicits no response and
// leaves the connection usable.
//
// #1485 (FIXED): a standalone 00 00 chunk is a Bolt 4.1+ NOOP keep-alive that
// the framing layer (proto.ChunkedReader.ReadMessage) now silently discards, so
// the serve loop never sees a spurious zero-length message to (mis)decode. The
// server must therefore send nothing in reply to the NOOP, and the connection
// must remain READY for a real query.
func TestSec_NOOP_SilentlyIgnored(t *testing.T) {
	addr := startTestServer(t, server.Options{ConnTimeout: 5 * time.Second})
	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)

	// A standalone NOOP: a single 00 00 with no preceding payload chunk.
	if _, err := c.conn.Write([]byte{0x00, 0x00}); err != nil {
		t.Fatalf("write NOOP: %v", err)
	}

	if msg, got := recvWithin(c.conn, c.cr, 1*time.Second); got {
		t.Errorf("server replied %T to a NOOP keep-alive; a conformant server MUST silently discard a NOOP and send nothing.", msg)
		return
	}
	// recvWithin left a (now-elapsed) read deadline on the connection; clear it
	// so the subsequent real read is not killed by the stale deadline.
	if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
	// Conformant behaviour: nothing came back. Confirm the connection is still
	// usable for a real query (the NOOP did not poison the session).
	c.run(t, "RETURN 1 AS n", nil)
}

// TestSec_NOOP_MidStreamDoesNotInjectFailure verifies that a NOOP injected
// between RUN(success) and PULL is not turned into a spurious FAILURE in the
// response stream (which a driver would read as the query failing).
//
// #1485 (FIXED): the NOOP is discarded at the framing layer, so the PULL streams
// its RECORD + SUCCESS with no FAILURE injected ahead of it.
func TestSec_NOOP_MidStreamDoesNotInjectFailure(t *testing.T) {
	addr := startTestServer(t, server.Options{ConnTimeout: 5 * time.Second})
	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)
	c.run(t, "RETURN 1 AS n", nil)

	// Inject a NOOP between RUN-success and PULL.
	if _, err := c.conn.Write([]byte{0x00, 0x00}); err != nil {
		t.Fatalf("write NOOP: %v", err)
	}
	c.sendRequest(t, &proto.Pull{N: -1, QID: -1})

	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	sawFailure := false
	for i := 0; i < 5; i++ {
		raw, err := c.cr.ReadMessage()
		if err != nil {
			break
		}
		dec := packstream.NewDecoder(bytes.NewReader(raw))
		msg, _ := proto.DecodeResponse(dec)
		if f, ok := msg.(*proto.Failure); ok {
			sawFailure = true
			t.Logf("observed FAILURE in stream: code=%s msg=%q", f.Code, f.Message)
		}
		if s, ok := msg.(*proto.Success); ok {
			if hm, _ := s.Metadata["has_more"].(bool); !hm {
				break
			}
		}
	}
	if sawFailure {
		t.Errorf("#1485: a NOOP injected mid-stream produced a spurious FAILURE in the response stream; a NOOP must be silently discarded.")
	}
}
