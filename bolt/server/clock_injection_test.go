package server_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// startTestServerWithClock builds a NoAuth test server, injects the given clock
// for the explicit-transaction timeout reaper, and starts serving. It mirrors
// startTestServer but injects the clock before Serve so every session captures
// it. It returns the listen address.
//
//nolint:gocritic // hugeParam: test helper takes Options by value to mirror the public NewServer signature; not a hot path.
func startTestServerWithClock(t *testing.T, opts server.Options, clk clock.Clock) string {
	t.Helper()
	eng := newEngine(t)
	if opts.ConnTimeout == 0 {
		opts.ConnTimeout = 30 * time.Second // long: never let idle-close mask the reaper
	}
	if opts.Auth == nil {
		opts.Auth = server.NoAuthHandler{}
	}
	srv, err := server.NewServer(eng, opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetClockForTest(clk)

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
			t.Log("Serve goroutine did not exit in cleanup")
		}
	})
	time.Sleep(10 * time.Millisecond)
	return addr
}

// TestTxTimeout_FakeClockDrivesReap proves the explicit-transaction timeout
// reaper is governed by the injected clock, not wall time: with a finite
// DefaultTxTimeout and a fake clock that never advances on its own, an open
// transaction is NOT reaped until the fake clock is advanced past the deadline.
// Advancing the fake then reaps it deterministically.
func TestTxTimeout_FakeClockDrivesReap(t *testing.T) {
	t.Parallel()

	const txTimeout = 500 * time.Millisecond
	// Real-time epoch for the fake; advances are virtual.
	fake := clock.NewFake(time.Unix(0, 0))
	addr := startTestServerWithClock(t, server.Options{
		DefaultTxTimeout: txTimeout,
		ConnTimeout:      30 * time.Second,
	}, fake)

	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)
	c.begin(t) // opens an explicit transaction; reaper armed at fake.Now()+txTimeout

	// Phase 1: the fake clock has not advanced, so the reaper must NOT fire even
	// after real time passes. A no-op ping must still succeed (session healthy).
	time.Sleep(50 * time.Millisecond) // real time the reaper must ignore
	resp, err := c.pingLogon()
	if err != nil {
		t.Fatalf("session reaped before the virtual deadline (ping err: %v)", err)
	}
	if _, isFail := resp.(*proto.Failure); isFail {
		t.Fatal("session reaped before the virtual deadline (ping returned FAILURE)")
	}

	// Phase 2: advance the fake clock past the transaction deadline. The reaper
	// timer fires and rolls back the idle transaction; the next ping observes the
	// FAILED session (gentle reap) or a closed connection (hard reap).
	fake.Advance(txTimeout + time.Millisecond)

	reaped := false
	deadline := time.Now().Add(3 * time.Second) // real-time safety net only
	for time.Now().Before(deadline) {
		resp, err := c.pingLogon()
		if err != nil {
			reaped = true // hard reap: connection closed
			break
		}
		// Gentle reap: session moved to FAILED. In FAILED a non-RESET/GOODBYE
		// message (the no-op LOGON ping) is answered with IGNORED per the Bolt
		// v5 spec (#1781); older builds replied FAILURE. Either signals the reap.
		switch resp.(type) {
		case *proto.Failure, *proto.Ignored:
			reaped = true
		}
		if reaped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !reaped {
		t.Fatal("transaction was not reaped after advancing the fake clock past the deadline")
	}
}

// TestTxTimeout_DefaultClockStillReaps guards the behaviour-preserving default:
// with no injected clock the real-clock reaper still fires within a real-time
// budget.
func TestTxTimeout_DefaultClockStillReaps(t *testing.T) {
	t.Parallel()

	const txTimeout = 200 * time.Millisecond
	addr := startTestServer(t, server.Options{
		DefaultTxTimeout: txTimeout,
		ConnTimeout:      10 * time.Second,
	})

	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)
	c.begin(t)

	reaped := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.pingLogon()
		if err != nil {
			reaped = true
			break
		}
		// Gentle reap → FAILED → LOGON ping answered with IGNORED (or FAILURE on
		// older builds) per the Bolt v5 spec (#1781). Either signals the reap.
		switch resp.(type) {
		case *proto.Failure, *proto.Ignored:
			reaped = true
		}
		if reaped {
			break
		}
		time.Sleep(txTimeout / 4)
	}
	if !reaped {
		t.Fatal("default real-clock reaper never fired")
	}
}
