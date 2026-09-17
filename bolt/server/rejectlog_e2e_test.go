package server_test

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// rejectlog_e2e_test.go — rmp #2835, end to end through Serve's accept loop.
//
// These tests exercise the real reject branch: a real listener, a real
// semaphore, real refused TCP connections. They assert the two things the task
// separates — the log is bounded, and the COUNTER is not.

// lineCountingHandler is a slog.Handler that counts the records it is given and
// keeps their messages and attributes, so a test can assert both how many lines
// a flood produced and what they said.
type lineCountingHandler struct {
	level slog.Level

	mu    sync.Mutex
	lines []string
}

func (h *lineCountingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

//nolint:gocritic // hugeParam: slog.Handler fixes this signature; Handle must take slog.Record by value
func (h *lineCountingHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.String())
		return true
	})
	h.mu.Lock()
	h.lines = append(h.lines, b.String())
	h.mu.Unlock()
	return nil
}

func (h *lineCountingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *lineCountingHandler) WithGroup(string) slog.Handler      { return h }

// rejectLines returns the lines this handler captured that came from the accept
// loop's reject branch.
func (h *lineCountingHandler) rejectLines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, l := range h.lines {
		if strings.Contains(l, "max connections reached") {
			out = append(out, l)
		}
	}
	return out
}

// addrCountingConn counts how many times the server asks a connection for its
// remote address. On the reject path the only caller is the log line, so this
// counter measures directly whether the argument is evaluated eagerly.
type addrCountingConn struct {
	net.Conn
	addrCalls atomic.Int64
}

func (c *addrCountingConn) RemoteAddr() net.Addr {
	c.addrCalls.Add(1)
	return c.Conn.RemoteAddr()
}

// addrCountingListener wraps every accepted connection and keeps them all, so a
// test can sum the RemoteAddr calls across the refused ones.
type addrCountingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []*addrCountingConn
}

func (l *addrCountingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	ac := &addrCountingConn{Conn: c}
	l.mu.Lock()
	l.conns = append(l.conns, ac)
	l.mu.Unlock()
	return ac, nil
}

// addrCallsAfterFirst sums the RemoteAddr calls over every accepted connection
// except the first. With MaxConnections:1 the first is the admitted one — whose
// handler legitimately resolves the address once — and all the rest are refused.
func (l *addrCountingListener) addrCallsAfterFirst() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for i, c := range l.conns {
		if i == 0 {
			continue
		}
		n += c.addrCalls.Load()
	}
	return n
}

// startFloodServer starts a Server with room for exactly one connection, the
// given logger, and a listener that counts RemoteAddr calls.
func startFloodServer(t *testing.T, log *slog.Logger) (string, *addrCountingListener) {
	t.Helper()
	srv, err := server.NewServer(newEngine(t), server.Options{
		MaxConnections: 1,
		ConnTimeout:    5 * time.Second,
		Auth:           server.NoAuthHandler{},
		Logger:         log,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := &addrCountingListener{Listener: raw}
	addr := raw.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("startFloodServer: Serve goroutine did not exit in cleanup")
		}
	})
	time.Sleep(10 * time.Millisecond)
	return addr, ln
}

// flood opens and immediately closes n connections against addr, all of which
// the server refuses because the single semaphore slot is already held.
func flood(t *testing.T, addr string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			// A refused TCP connect is itself a valid outcome under a flood; the
			// counter assertion below is what decides the test, and it counts only
			// what the accept loop actually saw.
			continue
		}
		_ = c.Close()
	}
}

// TestServe_RejectLogIsBoundedAndCounterIsExact is the core assertion of
// rmp #2835, and it deliberately makes both halves in one test because they are
// a pair: the log may lose detail only because the counter does not.
//
// The two floods differ by 4x in size and fall inside the same rate-limit
// interval, so a line count that tracked the rejection count would be plainly
// visible. Before this change the server wrote ONE LINE PER REFUSAL — 500 of
// them, here.
func TestServe_RejectLogIsBoundedAndCounterIsExact(t *testing.T) {
	const small, large = 100, 400

	probe := newServerMetricsProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	h := &lineCountingHandler{level: slog.LevelDebug}
	addr, _ := startFloodServer(t, slog.New(h))

	// Hold the one slot, so every later connection is refused.
	held, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial the admitted connection: %v", err)
	}
	defer held.Close()
	if !waitFor(func() bool { return probe.get("bolt.server.conn.accepted") == 1 }, 2*time.Second) {
		t.Fatalf("the first connection was not admitted: accepted=%d", probe.get("bolt.server.conn.accepted"))
	}

	flood(t, addr, small)
	flood(t, addr, large)

	total := uint64(small + large)
	if !waitFor(func() bool { return probe.get("bolt.server.conn.rejected") >= total }, 10*time.Second) {
		t.Fatalf("bolt.server.conn.rejected = %d after %d refused connections; the counter must see every one",
			probe.get("bolt.server.conn.rejected"), total)
	}
	// Exactly, not merely at least: no refusal may be counted twice either.
	if got := probe.get("bolt.server.conn.rejected"); got != total {
		t.Errorf("bolt.server.conn.rejected = %d, want exactly %d", got, total)
	}

	lines := h.rejectLines()
	t.Logf("%d refused connections produced %d log lines", total, len(lines))
	for _, l := range lines {
		t.Logf("  %s", l)
	}

	// The bound. Both floods run far faster than rejectLogInterval, so one or two
	// lines is the expected outcome; the ceiling is set at 4 so that a slow host
	// crossing an interval boundary cannot make this flaky, while still being
	// two orders of magnitude below the 500 lines the old build wrote.
	if len(lines) == 0 {
		t.Fatal("no line was written for 500 refused connections; the first refusal of a burst must always be logged")
	}
	if len(lines) > 4 {
		t.Errorf("%d refused connections produced %d log lines, want at most 4. "+
			"A line count that tracks the rejection count means the log is unbounded again", total, len(lines))
	}

	// The operator must be able to tell that what they are reading is sampled,
	// and the line must carry the refusals it stands for.
	first := lines[0]
	if !strings.Contains(first, "rate-limited") {
		t.Errorf("log line does not say it is rate-limited: %q", first)
	}
	if !strings.Contains(first, "suppressed_since_last_line") {
		t.Errorf("log line does not carry the suppressed count: %q", first)
	}
}

// TestServe_RejectDoesNotResolveRemoteAddrWhenDiscarded is the laziness
// assertion. conn.RemoteAddr().String() cost 45.38 ns, 32 B and 3 allocations
// per refusal and was paid before the call, so lowering the log level did not
// remove it.
//
// The two arms are the whole claim: at LevelError the address is never resolved
// on a refused connection at all, and at LevelWarn it is resolved only for the
// line that is actually written — never for a refusal the limiter swallows.
func TestServe_RejectDoesNotResolveRemoteAddrWhenDiscarded(t *testing.T) {
	const refusals = 100

	probe := newServerMetricsProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	run := func(t *testing.T, level slog.Level, wantAddrCalls int64) {
		t.Helper()
		h := &lineCountingHandler{level: level}
		addr, ln := startFloodServer(t, slog.New(h))

		held, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial the admitted connection: %v", err)
		}
		defer held.Close()

		base := probe.get("bolt.server.conn.rejected")
		if !waitFor(func() bool { return probe.get("bolt.server.conn.accepted") >= 1 }, 2*time.Second) {
			t.Fatal("the first connection was not admitted")
		}

		flood(t, addr, refusals)
		if !waitFor(func() bool { return probe.get("bolt.server.conn.rejected")-base >= refusals }, 10*time.Second) {
			t.Fatalf("only %d of %d refusals were counted", probe.get("bolt.server.conn.rejected")-base, refusals)
		}

		got := ln.addrCallsAfterFirst()
		t.Logf("level=%v: %d refusals resolved RemoteAddr %d time(s), %d log line(s)",
			level, refusals, got, len(h.rejectLines()))
		if got != wantAddrCalls {
			t.Errorf("level=%v: RemoteAddr resolved %d times across %d refused connections, want %d",
				level, got, refusals, wantAddrCalls)
		}
	}

	// At LevelError the record is discarded, so nothing of it may be built.
	t.Run("level-error-resolves-nothing", func(t *testing.T) { run(t, slog.LevelError, 0) })

	// At LevelWarn exactly one line is written for a burst this fast, and only
	// that line's argument may be evaluated.
	t.Run("level-warn-resolves-once", func(t *testing.T) { run(t, slog.LevelWarn, 1) })
}
