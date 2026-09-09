package server

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"
)

// keepalive_internal_test.go — rmp #2807.
//
// The socket-option evidence that keep-alive is really installed lives in the
// external keepalive_test.go, which can read the kernel's view of a served
// connection. This file covers the branch that has no socket to read: a
// connection that is not a *net.TCPConn.
//
// Layer: short.

// TestConfigureKeepAlive_NonTCPConnIsLeftAloneSilently pins that a non-TCP
// connection — a [net.Pipe] from a test, a Unix socket, an embedder's own
// transport — is neither refused nor complained about. Keep-alive is a TCP-level
// concept, so there is nothing to configure and nothing to warn about; a warning
// on every such connection would be noise on a path the test suite itself takes.
//
// The control arm is a real TCP connection, which must configure cleanly and
// therefore also log nothing. Together they say: this function is silent when it
// succeeds and silent when there is nothing to do, so any log line it produces
// is a genuine failure.
func TestConfigureKeepAlive_NonTCPConnIsLeftAloneSilently(t *testing.T) {
	t.Parallel()

	newServerLoggingTo := func(t *testing.T, buf *bytes.Buffer) *Server {
		t.Helper()
		srv, err := NewServer(newTestEngine(t), Options{
			Auth:   NoAuthHandler{},
			Logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		return srv
	}

	t.Run("net.Pipe", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		srv := newServerLoggingTo(t, &buf)
		// Drain the constructor's own warnings; only what configureKeepAlive
		// writes is under test.
		buf.Reset()

		a, b := net.Pipe()
		defer a.Close() // best-effort close on test teardown
		defer b.Close() // best-effort close on test teardown

		srv.configureKeepAlive(a) // must not panic
		if got := buf.String(); strings.Contains(got, "keep-alive") {
			t.Fatalf("configureKeepAlive logged about a net.Pipe, which has no keep-alive to configure:\n%s", got)
		}
	})

	t.Run("tcp-control", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		srv := newServerLoggingTo(t, &buf)
		buf.Reset()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close() // best-effort close on test teardown

		accepted := make(chan net.Conn, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}()
		client, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer client.Close() // best-effort close on test teardown
		srvConn := <-accepted
		defer srvConn.Close() // best-effort close on test teardown

		srv.configureKeepAlive(srvConn)
		if got := buf.String(); strings.Contains(got, "keep-alive") {
			t.Fatalf("configureKeepAlive failed on a real TCP connection:\n%s", got)
		}
	})
}
