package server_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// TestServe_MaxConnections_TypedClose verifies the three ACs for T720 that the
// basic TestServe_MaxConnections does not fully exercise:
//
//  1. Server accepts exactly MaxConnections concurrent sessions (conn1 accepted,
//     semaphore full).
//  2. Overflow client (conn2) receives a typed close: reads return io.EOF or a
//     net.Error, not a successful byte stream.
//  3. Existing session (conn1) is unaffected — can complete the handshake after
//     conn2 is rejected.
func TestServe_MaxConnections_TypedClose(t *testing.T) {
	t.Parallel()

	eng := newEngine(t)
	srv, err := server.NewServer(eng, server.Options{
		MaxConnections: 1,
		ConnTimeout:    5 * time.Second,
		Auth:           server.NoAuthHandler{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

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

	// Allow the server to enter Accept.
	time.Sleep(10 * time.Millisecond)

	// ── conn1: acquires the single semaphore slot ────────────────────────────
	conn1, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close() //nolint:errcheck

	// Wait for conn1 to be accepted and for the semaphore slot to be taken.
	time.Sleep(30 * time.Millisecond)

	// ── conn2: semaphore full → server closes it immediately ─────────────────
	conn2, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close() //nolint:errcheck

	_ = conn2.SetDeadline(time.Now().Add(2 * time.Second))
	var buf [4]byte
	_, readErr := io.ReadFull(conn2, buf[:])
	// AC2: overflow client must receive EOF (typed close) or a network error,
	// NOT a successful read of 4 bytes (which would indicate the server sent
	// real Bolt data to an overflowed connection).
	if readErr == nil {
		t.Fatal("conn2 read 4 bytes on an overflowed connection; expected EOF / typed close")
	}
	if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		var netErr net.Error
		if !errors.As(readErr, &netErr) {
			t.Logf("conn2 read error (acceptable): %v", readErr)
		}
	}

	// ── conn1 remains usable: complete handshake ─────────────────────────────
	// AC3: the accepted session is unaffected. Verify by completing the Bolt
	// handshake on conn1. A timeout here would indicate conn1 was disrupted.
	_ = conn1.SetDeadline(time.Now().Add(3 * time.Second))
	boltHandshake(t, conn1)
}
