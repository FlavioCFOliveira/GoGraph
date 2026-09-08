package server_test

// keepalive_test.go — rmp #2807.
//
// rmp #2807 disabled Options.ConnTimeout by default, so the server no longer has
// a read deadline standing in for "is the peer still there?". TCP keep-alive
// takes that job, and this file proves the server actually installs it — by
// reading the socket options back out of the kernel, not by reading the code.
//
// Layer: short. One dial, four getsockopt calls, no waiting.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// listenWithKeepAliveDisabled returns a TCP listener whose accepted connections
// arrive with SO_KEEPALIVE OFF.
//
// This is what makes the gate falsifiable. A listener built by net.Listen hands
// out connections on which the Go runtime has ALREADY enabled keep-alive (15 s
// idle, 15 s interval, 9 probes — measured on this project's development host),
// so a test that accepted from one would read SO_KEEPALIVE as set whether or not
// the server ever touched the socket: it could not fail. net.ListenConfig
// documents a negative KeepAlive as "disabled", and the control arm of
// TestKeepAlive_IsEnabledOnAcceptedConnections asserts that it really is.
func listenWithKeepAliveDisabled(t *testing.T) net.Listener {
	t.Helper()
	lc := net.ListenConfig{KeepAlive: -1}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// capturingListener hands every accepted connection to the test as well as to
// the server, so the test can inspect the SERVER-SIDE socket — the one the
// server configured — rather than its own end, which carries a different set of
// socket options entirely.
type capturingListener struct {
	net.Listener
	accepted chan net.Conn
}

func (l *capturingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	select {
	case l.accepted <- c:
	default:
	}
	return c, nil
}

// TestKeepAlive_IsEnabledOnAcceptedConnections is the acceptance gate for the
// liveness mechanism that replaced the read deadline.
//
// The control arm accepts a connection from the same keep-alive-disabled
// listener WITHOUT the server, and asserts the kernel reports keep-alive off.
// Without it the main arm would be vacuous, because on this platform a
// connection from a default listener already has keep-alive on.
//
// The main arm serves that same kind of listener with a real [server.Server],
// then reads the accepted connection's socket options: keep-alive must be ON,
// with the idle, interval and probe count [server.DefaultKeepAliveIdle],
// [server.DefaultKeepAliveInterval] and [server.DefaultKeepAliveCount] name.
// Those three also differ from every default the socket could have arrived with
// — Go's listener default (15 s / 15 s / 9) and the platform default
// (7200 s / 75 s / 8 on the development host) — so the values are attributable
// to the server and not to the environment.
func TestKeepAlive_IsEnabledOnAcceptedConnections(t *testing.T) {
	t.Parallel()

	t.Run("control-listener-hands-out-keepalive-off", func(t *testing.T) {
		t.Parallel()
		ln := listenWithKeepAliveDisabled(t)

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

		var srvConn net.Conn
		select {
		case srvConn = <-accepted:
		case <-time.After(5 * time.Second):
			t.Fatal("the listener accepted nothing within 5s")
		}
		defer srvConn.Close() // best-effort close on test teardown

		tcp, ok := srvConn.(*net.TCPConn)
		if !ok {
			t.Fatalf("accepted connection is %T, want *net.TCPConn", srvConn)
		}
		opts, supported, err := readKeepAliveSockopts(tcp)
		if err != nil {
			t.Fatalf("readKeepAliveSockopts: %v", err)
		}
		if !supported {
			t.Skip("this platform's keep-alive socket options are not known to the test suite; " +
				"add a keepalive_sockopt_<goos>_test.go to enable the gate")
		}
		if opts.Enabled {
			t.Fatalf("a connection accepted from net.ListenConfig{KeepAlive: -1} reports SO_KEEPALIVE ON (%+v); "+
				"the served arm below would then pass whether or not the server configured anything", opts)
		}
	})

	t.Run("served-connection-has-keepalive-on", func(t *testing.T) {
		t.Parallel()
		base := listenWithKeepAliveDisabled(t)
		ln := &capturingListener{Listener: base, accepted: make(chan net.Conn, 1)}

		srv, err := server.NewServer(newEngine(t), server.Options{Auth: server.NoAuthHandler{}})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		serveErr := make(chan error, 1)
		go func() { serveErr <- srv.Serve(ctx, ln) }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-serveErr:
			case <-time.After(5 * time.Second):
				t.Log("Serve did not exit within 5s")
			}
		})

		client, err := net.Dial("tcp", base.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer client.Close() // best-effort close on test teardown

		var srvConn net.Conn
		select {
		case srvConn = <-ln.accepted:
		case <-time.After(5 * time.Second):
			t.Fatal("the server accepted nothing within 5s")
		}

		tcp, ok := srvConn.(*net.TCPConn)
		if !ok {
			t.Fatalf("the server accepted a %T, want *net.TCPConn", srvConn)
		}

		// The server configures the socket after Accept returns and the capture
		// happens inside Accept, so poll rather than racing it. Poll on ALL FOUR
		// values, not just SO_KEEPALIVE: the standard library sets Enable first
		// and the three parameters after it, so a loop that stopped at Enable
		// would read the platform defaults for the rest (measured: idle 2h0m0s,
		// interval 1m15s) and fail for a reason that is purely a race.
		want := keepAliveSockopts{
			Enabled:  true,
			Idle:     server.DefaultKeepAliveIdle,
			Interval: server.DefaultKeepAliveInterval,
			Count:    server.DefaultKeepAliveCount,
		}
		var (
			opts      keepAliveSockopts
			supported bool
			readErr   error
		)
		deadline := time.Now().Add(5 * time.Second)
		for {
			opts, supported, readErr = readKeepAliveSockopts(tcp)
			if readErr != nil {
				t.Fatalf("readKeepAliveSockopts: %v", readErr)
			}
			if !supported {
				t.Skip("this platform's keep-alive socket options are not known to the test suite; " +
					"add a keepalive_sockopt_<goos>_test.go to enable the gate")
			}
			if opts == want || time.Now().After(deadline) {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}

		if !opts.Enabled {
			t.Fatalf("the server-side accepted socket reports SO_KEEPALIVE OFF (%+v); since rmp #2807 disabled "+
				"the read deadline, keep-alive is the ONLY thing that reclaims a connection whose peer has vanished", opts)
		}
		if opts.Idle != server.DefaultKeepAliveIdle {
			t.Errorf("keep-alive idle = %v; want DefaultKeepAliveIdle %v", opts.Idle, server.DefaultKeepAliveIdle)
		}
		if opts.Interval != server.DefaultKeepAliveInterval {
			t.Errorf("keep-alive interval = %v; want DefaultKeepAliveInterval %v", opts.Interval, server.DefaultKeepAliveInterval)
		}
		if opts.Count != server.DefaultKeepAliveCount {
			t.Errorf("keep-alive probe count = %d; want DefaultKeepAliveCount %d", opts.Count, server.DefaultKeepAliveCount)
		}
	})
}
