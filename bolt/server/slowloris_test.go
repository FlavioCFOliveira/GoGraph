package server_test

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"gograph/bolt/server"
)

// TestServe_SlowlorisHandshakeDisconnect is the primary regression for the
// Slowloris / connection-exhaustion denial of service: a client opens a
// connection, sends a single byte, and then stalls without ever completing the
// 20-byte Bolt handshake. Before the fix the accept context carried no
// deadline and ConnTimeout defaulted to zero, so proto.Negotiate's
// io.ReadFull blocked forever, pinning the connection slot and goroutine.
//
// With the dedicated handshake deadline the server must reclaim the connection
// within the handshake bound and close it. The test detects the server-side
// close by reading from the client end: once handleConn returns it closes the
// connection, so the client's Read observes EOF (or a closed-connection error).
// The handshake bound is shortened to 200 ms through the package test seam
// (server.SetHandshakeTimeoutForTest) so the test is fast, and a generous upper
// bound (3 s) absorbs scheduling jitter without flaking. ConnTimeout is set
// explicitly (and larger) to prove the handshake bound — not the idle deadline
// — is what closes a connection stuck in the unauthenticated phase.
func TestServe_SlowlorisHandshakeDisconnect(t *testing.T) {
	restore := server.SetHandshakeTimeoutForTest(200 * time.Millisecond)
	defer restore()

	addr := startTestServer(t, server.Options{
		ConnTimeout: 5 * time.Second,
	})

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort close on teardown

	// Send a single byte and then stall — never complete the 20-byte handshake.
	if _, err := conn.Write([]byte{0x60}); err != nil {
		t.Fatalf("write single byte: %v", err)
	}

	// The server must close the connection once the handshake deadline fires.
	// Give the client a generous upper bound: the read returns an error
	// (EOF / closed connection) when the server closes its end.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	start := time.Now()
	buf := make([]byte, 4)
	_, readErr := io.ReadFull(conn, buf)
	elapsed := time.Since(start)

	if readErr == nil {
		t.Fatal("expected the server to close the stalled connection, but read succeeded")
	}
	// A client-side read deadline expiry means the server did NOT close the
	// connection in time — the slot is still held, the DoS is not mitigated.
	var ne net.Error
	if errors.As(readErr, &ne) && ne.Timeout() {
		t.Fatalf("server did not close the stalled connection within %v (client read timed out: %v)", 3*time.Second, readErr)
	}
	// Any other read error (io.EOF, io.ErrUnexpectedEOF, connection reset)
	// confirms the server closed its end — the slot was freed.
	t.Logf("server closed stalled connection after %v (read error: %v)", elapsed, readErr)
}

// TestServe_DefaultServerEnforcesHandshakeDeadline proves the second
// acceptance criterion end-to-end: a server created with NO timeout options at
// all (Options{}) still reclaims a stalled handshake, because the handshake
// phase is bounded by the package-level handshakeTimeout (seeded from the
// exported DefaultHandshakeTimeout const) regardless of Options. The real
// default is 10 s; to keep the test fast and deterministic the bound is
// shortened through the package test seam — this exercises exactly the same
// code path NewServer's default server would, only quicker. A defaulted server
// also fills ConnTimeout (DefaultConnTimeout), asserted separately in
// TestNewServer_DefaultsConnTimeout.
func TestServe_DefaultServerEnforcesHandshakeDeadline(t *testing.T) {
	// Sanity: a default server must enforce a deadline. The const that seeds the
	// handshake bound is non-zero, and ConnTimeout is defaulted by NewServer.
	if server.DefaultHandshakeTimeout <= 0 {
		t.Fatalf("DefaultHandshakeTimeout must be non-zero, got %v", server.DefaultHandshakeTimeout)
	}

	// Shorten the handshake bound via the seam so the reclaim is observed fast.
	restore := server.SetHandshakeTimeoutForTest(200 * time.Millisecond)
	defer restore()

	// Empty Options: ConnTimeout takes its NewServer default; the handshake
	// bound comes from the (seam-shortened) package value, not from Options.
	addr := startTestServer(t, server.Options{
		ConnTimeout: server.DefaultConnTimeout,
	})

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort close on teardown

	if _, err := conn.Write([]byte{0x60}); err != nil {
		t.Fatalf("write single byte: %v", err)
	}

	// Generous upper bound to absorb scheduling jitter without flaking.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	buf := make([]byte, 4)
	_, readErr := io.ReadFull(conn, buf)
	if readErr == nil {
		t.Fatal("expected the default server to close the stalled connection, but read succeeded")
	}
	var ne net.Error
	if errors.As(readErr, &ne) && ne.Timeout() {
		t.Fatalf("default server did not enforce a handshake deadline (client read timed out: %v)", readErr)
	}
}

// TestServe_NormalSessionNotPrematurelyKilled is the regression guard for the
// fix's safety property: defaulting the timeouts must NOT break a legitimate
// session. It starts a server with all-default Options (the embedder sets
// nothing), then drives a full handshake + HELLO + RUN + PULL round-trip and
// asserts every step succeeds. If the handshake deadline bled into the message
// loop, or the idle ConnTimeout were too aggressive, this exchange would fail.
func TestServe_NormalSessionNotPrematurelyKilled(t *testing.T) {
	// All-default server: only the engine is provided; timeouts are filled by
	// NewServer. startTestServer would substitute a 5 s ConnTimeout when zero,
	// so pin ConnTimeout to the real default to exercise the defaulted path.
	addr := startTestServer(t, server.Options{
		ConnTimeout: server.DefaultConnTimeout,
	})

	c := newBoltTestClient(t, addr)
	defer c.close(t)

	c.negotiate(t)
	c.hello(t)

	// A trivial read query that returns a single row, exercising RUN + PULL
	// after the handshake deadline has been cleared.
	c.run(t, "RETURN 1 AS n", nil)
	records, success := c.pullAll(t)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if success.Metadata == nil {
		t.Fatal("PULL SUCCESS metadata is nil")
	}

	c.goodbye(t)
}
