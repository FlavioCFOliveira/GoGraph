package server_test

// security_slowloris_drip_test.go — DEFENSE LOCK-IN for the post-handshake
// idle-read reclaim (security audit, DoS/slowloris cluster).
//
// slowloris_test.go pins the PRE-handshake stall (reclaimed by the fixed
// handshakeTimeout). This file pins the COMPLEMENTARY case: a connection that
// completes the handshake and HELLO, then opens a chunked message and stalls —
// or drips it so slowly that the whole-message read exceeds the idle bound. The
// serve loop sets the read deadline to now+ConnTimeout BEFORE each ReadMessage
// and does NOT extend it per chunk, so a message the client never finishes
// delivering within ConnTimeout must be reclaimed and the connection closed.
// Without that bound a thousand half-sent messages would pin a thousand reader
// goroutines and connection slots indefinitely.
//
// ConnTimeout is set to a small value (300 ms) so the reclaim is observed fast;
// a generous client-side upper bound (4 s) absorbs scheduling jitter without
// flaking.
//
// Layer: short. The server is torn down via t.Cleanup; the connection is closed
// by the test.

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// secBoltConnTimeout is the small idle bound used by the drip tests.
const secBoltConnTimeout = 300 * time.Millisecond

// TestSec_Bolt_SlowlorisMidMessageStallReclaimed opens a chunked message,
// sends a single chunk header announcing more bytes than it then delivers, and
// stalls. The server's whole-message read must hit the ConnTimeout deadline and
// close the connection.
func TestSec_Bolt_SlowlorisMidMessageStallReclaimed(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, server.Options{ConnTimeout: secBoltConnTimeout})

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	// Complete the handshake so the server enters the message loop (the idle
	// ConnTimeout, not the handshake bound, is what must reclaim this).
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	boltHandshake(t, conn)

	// Open a chunked message: a 2-byte header announcing a 32-byte chunk, then
	// deliver only 4 of those bytes and stall. ReadMessage blocks waiting for the
	// rest; the deadline set before the read must fire within ConnTimeout.
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], 32)
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatalf("write chunk header: %v", err)
	}
	if _, err := conn.Write([]byte{0xB1, 0x10, 0x00, 0x00}); err != nil {
		t.Fatalf("write partial chunk body: %v", err)
	}
	// Now stall: never deliver the remaining 28 bytes.

	secBoltAssertReclaimed(t, conn, 4*time.Second)
}

// TestSec_Bolt_SlowlorisSlowDripReclaimed drips a chunked message one byte at a
// time with sub-deadline gaps, but takes overall LONGER than ConnTimeout to
// deliver the whole message. Because the read deadline is set once before the
// whole-message read and not extended per byte, the server must still reclaim
// the connection: a client that cannot deliver a full message within the idle
// bound is treated as idle.
func TestSec_Bolt_SlowlorisSlowDripReclaimed(t *testing.T) {
	t.Parallel()

	addr := startTestServer(t, server.Options{ConnTimeout: secBoltConnTimeout})

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	boltHandshake(t, conn)

	// Clear the deadline so our own slow writes are not bounded by it.
	_ = conn.SetDeadline(time.Time{})

	// Announce a 200-byte chunk, then drip its body one byte per gap. Each gap is
	// shorter than ConnTimeout (so an individual read makes progress), but the
	// cumulative delivery time (200 * gap) far exceeds ConnTimeout, so the
	// whole-message read deadline — set once, before the read — must fire.
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], 200)
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatalf("write chunk header: %v", err)
	}

	// Drip a handful of bytes with small gaps; the server should close the
	// connection partway through, at which point our Write starts failing. We
	// stop dripping once the connection is gone.
	gap := secBoltConnTimeout / 5
	dripDone := make(chan struct{})
	go func() {
		defer close(dripDone)
		for i := 0; i < 200; i++ {
			if _, err := conn.Write([]byte{0x00}); err != nil {
				return // server closed its end — expected
			}
			time.Sleep(gap)
		}
	}()

	secBoltAssertReclaimed(t, conn, 4*time.Second)
	<-dripDone
}

// secBoltAssertReclaimed reads from conn and asserts the server closed its end
// (a connection-closed error) within upper, rather than the client read timing
// out (which would mean the server never reclaimed the slot). The read deadline
// is the client's own upper bound, distinct from the server's ConnTimeout.
func secBoltAssertReclaimed(t *testing.T, conn net.Conn, upper time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(upper)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 4)
	_, readErr := io.ReadFull(conn, buf)
	if readErr == nil {
		t.Fatal("expected the server to close the stalled connection, but read succeeded")
	}
	var ne net.Error
	if errors.As(readErr, &ne) && ne.Timeout() {
		t.Fatalf("server did not reclaim the stalled connection within %v (client read timed out: %v) — the idle ConnTimeout did not fire", upper, readErr)
	}
	// io.EOF / connection reset / closed-pipe all confirm the server closed its
	// end: the slot and reader goroutine were freed.
	t.Logf("server reclaimed stalled connection (read error: %v)", readErr)
}
