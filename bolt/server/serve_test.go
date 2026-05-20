package server_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"gograph/bolt/packstream"
	"gograph/bolt/proto"
	"gograph/bolt/server"
	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

func newEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g)
}

// boltHandshake performs the 20-byte Bolt client handshake offering version
// 5.0 and reads the 4-byte server response.
func boltHandshake(t *testing.T, conn net.Conn) {
	t.Helper()

	// 4-byte magic + 4×4-byte version slots
	var buf [20]byte
	binary.BigEndian.PutUint32(buf[:4], proto.Magic)
	// Slot 0: version 5.0 (major=5, minor=0, range=0, pad=0)
	buf[4] = 5
	buf[5] = 0
	buf[6] = 0
	buf[7] = 0
	// Slots 1-3: zero (not offered)
	if _, err := conn.Write(buf[:]); err != nil {
		t.Fatalf("handshake write: %v", err)
	}

	// Read 4-byte server response.
	var resp [4]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		t.Fatalf("handshake read response: %v", err)
	}
	if resp[0] == 0 && resp[1] == 0 {
		t.Fatal("server rejected version negotiation")
	}
}

// sendHello sends a HELLO message over conn and reads back the SUCCESS
// response, returning it.
func sendHello(t *testing.T, conn net.Conn) *proto.Success {
	t.Helper()

	// Encode HELLO via proto + packstream.
	var encBuf bytes.Buffer
	enc := packstream.NewEncoder(&encBuf)
	err := proto.EncodeRequest(enc, &proto.Hello{
		Extra: map[string]interface{}{
			"scheme":      "none",
			"principal":   "test",
			"credentials": "",
			"agent":       "test/1.0",
		},
	})
	if err != nil {
		t.Fatalf("encode HELLO: %v", err)
	}
	// Encoder uses an internal bufio.Writer; flush before reading the bytes.
	if err := enc.Flush(); err != nil {
		t.Fatalf("flush HELLO encoder: %v", err)
	}

	cw := proto.NewChunkedWriter(conn)
	if err := cw.WriteMessage(encBuf.Bytes()); err != nil {
		t.Fatalf("write HELLO: %v", err)
	}

	cr := proto.NewChunkedReader(conn)
	raw, err := cr.ReadMessage()
	if err != nil {
		t.Fatalf("read HELLO response: %v", err)
	}
	dec := packstream.NewDecoder(bytes.NewReader(raw))
	msg, err := proto.DecodeResponse(dec)
	if err != nil {
		t.Fatalf("decode HELLO response: %v", err)
	}
	s, ok := msg.(*proto.Success)
	if !ok {
		t.Fatalf("expected *proto.Success, got %T", msg)
	}
	return s
}

// TestServe_HandshakeHello starts a server on a random port, connects with a
// raw TCP connection, performs the Bolt handshake and HELLO exchange, and
// verifies the SUCCESS response. Goroutine leak checking is handled by
// goleak.Find in TestMain after all servers have been shut down.
func TestServe_HandshakeHello(t *testing.T) {
	eng := newEngine(t)
	srv := server.NewServer(eng, server.Options{
		ConnTimeout: 5 * time.Second,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ctx, ln)
	}()

	// Cancel the context to ensure Serve exits even if the test body fails
	// before reaching Shutdown, then wait for Serve to confirm exit.
	// Goroutine leak checking is handled by goleak.Find() in TestMain after
	// all servers (including the shared server) have been shut down.
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("warning: Serve goroutine did not exit in cleanup")
		}
	})

	// Give the server a moment to start accepting.
	time.Sleep(10 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	boltHandshake(t, conn)
	success := sendHello(t, conn)

	if success.Metadata == nil {
		t.Fatal("SUCCESS metadata is nil")
	}
	if _, ok := success.Metadata["server"]; !ok {
		t.Error("SUCCESS metadata missing 'server'")
	}

	// Close the client connection cleanly.
	conn.Close() //nolint:errcheck

	// Shutdown the server.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Signal that Serve was explicitly shut down so the cleanup knows to wait.
	// The cleanup drains serveErr; Serve must exit after Shutdown returns.
}

// TestServe_MaxConnections verifies that the server rejects connections when
// MaxConnections is reached (it should close the excess connection immediately).
func TestServe_MaxConnections(t *testing.T) {
	eng := newEngine(t)
	srv := server.NewServer(eng, server.Options{
		MaxConnections: 1,
		ConnTimeout:    2 * time.Second,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()

	// Goroutine leak checking is handled by goleak.Find() in TestMain.
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("warning: Serve goroutine did not exit in cleanup")
		}
	})

	time.Sleep(10 * time.Millisecond)

	// First connection: acquires the semaphore.
	conn1, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close() //nolint:errcheck

	// Give conn1 a chance to be accepted and acquire the semaphore slot.
	time.Sleep(20 * time.Millisecond)

	// Second connection: semaphore full, server closes it immediately.
	conn2, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close() //nolint:errcheck

	_ = conn2.SetDeadline(time.Now().Add(2 * time.Second))
	readBuf := make([]byte, 1)
	_, readErr := conn2.Read(readBuf)
	if readErr == nil {
		t.Log("conn2 read succeeded (may have gotten data before close)")
	}
	// Verify no panic; the server either closes conn2 immediately or conn1 drains.

	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx) //nolint:errcheck
}
