package server_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"gograph/bolt/proto"
	"gograph/bolt/server"
)

// writeRawChunkedMessage writes payload as a Bolt chunked message directly onto
// conn without relying on proto.ChunkedWriter so the test can produce payloads
// that exceed any writer-side limit.
func writeRawChunkedMessage(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	const maxChunk = 65535
	var hdr [2]byte
	rem := payload
	for len(rem) > 0 {
		chunk := rem
		if len(chunk) > maxChunk {
			chunk = chunk[:maxChunk]
		}
		binary.BigEndian.PutUint16(hdr[:], uint16(len(chunk)))
		if _, err := conn.Write(hdr[:]); err != nil {
			t.Fatalf("write chunk header: %v", err)
		}
		if _, err := conn.Write(chunk); err != nil {
			t.Fatalf("write chunk body: %v", err)
		}
		rem = rem[len(chunk):]
	}
	// end-of-message sentinel
	binary.BigEndian.PutUint16(hdr[:], 0)
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatalf("write EOM sentinel: %v", err)
	}
}

// connDropped returns true when err indicates the remote closed the connection.
func connDropped(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe")
}

// TestServe_OversizeMessage_ConnectionDropped is the end-to-end AC test for
// T728. It starts a server with a small MaxMessageBytes cap (512 B), performs a
// valid Bolt handshake, then sends a message that exceeds the cap and verifies
// that the server drops the connection without sending additional data.
//
// ACs verified:
//  1. Oversize input is rejected — server drops the connection
//     (no successful Bolt response echoed back).
//  2. No panic: the server continues to accept new connections after the drop.
//  3. After the oversize message the connection is dropped — subsequent read
//     returns a connection-closed error.
func TestServe_OversizeMessage_ConnectionDropped(t *testing.T) {
	t.Parallel()

	const msgCap = 512 // small cap so the test stays fast and allocation-light

	eng := newEngine(t)
	srv := server.NewServer(eng, server.Options{
		MaxMessageBytes: msgCap,
		ConnTimeout:     5 * time.Second,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("warning: Serve goroutine did not exit in cleanup")
		}
	})

	time.Sleep(10 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Complete the Bolt version handshake so the server enters the message loop.
	boltHandshake(t, conn)

	// Oversize payload: msgCap+1 bytes.
	payload := make([]byte, msgCap+1)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	// Send the oversize message. The server's ChunkedReader detects the excess
	// before returning the full payload and closes the connection.
	writeRawChunkedMessage(t, conn, payload)

	// AC3: subsequent read must return a connection-closed error.
	buf := make([]byte, 256)
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	n, readErr := conn.Read(buf)
	if !connDropped(readErr) {
		t.Fatalf("expected connection-closed error after oversize message; got %d bytes, err=%v", n, readErr)
	}

	// AC2: server must still accept a fresh connection after the drop.
	conn2, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("server rejected new connection after oversize drop: %v", err)
	}
	defer conn2.Close() //nolint:errcheck
	_ = conn2.SetDeadline(time.Now().Add(2 * time.Second))
	boltHandshake(t, conn2)

	// Clean shutdown.
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx) //nolint:errcheck
}

// Ensure proto package is imported (proto.Magic used by boltHandshake in serve_test.go).
var _ = proto.Magic
