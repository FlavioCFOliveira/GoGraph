package server_test

// panic_boundary_test.go — test for the per-connection panic boundary
// (security fix H7). A recoverable panic raised while handling one Bolt
// connection must be recovered inside the connection goroutine: the process
// must NOT crash, the offending connection must be closed, and the
// bolt.server.conn.panics counter must be incremented.
//
// The panic is injected through a test-only AuthHandler whose Authenticate
// panics. Authenticate is invoked from Session.handleHello, which runs inside
// Server.handleConn's message loop — exactly the path the recover guards.

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// panickingAuth is a test-only AuthHandler whose Authenticate always panics.
// It is the least-invasive seam for driving the connection handler into a
// recoverable panic without adding any production-only hook.
type panickingAuth struct{}

func (panickingAuth) Authenticate(_, _, _ string) (server.Identity, error) {
	panic("panickingAuth: injected test panic in connection handler")
}

// boltPanicProbe records how many times the bolt connection-panic counter
// fired. The global metrics backend is swapped atomically; this test must not
// run in parallel with anything else that installs a backend.
type boltPanicProbe struct {
	connPanics atomic.Uint64
}

func (p *boltPanicProbe) IncCounter(name string, delta uint64) {
	if name == "bolt.server.conn.panics" {
		p.connPanics.Add(delta)
	}
}

func (p *boltPanicProbe) ObserveLatency(string, time.Duration) {}

// TestBoltServer_RecoversConnectionHandlerPanic verifies that a recoverable
// panic raised while handling one connection (here, from the auth handler) is
// recovered inside the connection goroutine: the test process survives, the
// connection is closed by the server, and the bolt.server.conn.panics counter
// is incremented. If the recover were missing, the panic would unwind past the
// connection goroutine and crash the whole test binary.
func TestBoltServer_RecoversConnectionHandlerPanic(t *testing.T) {
	probe := &boltPanicProbe{}
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	addr := startTestServer(t, server.Options{
		Auth:        panickingAuth{},
		ConnTimeout: 5 * time.Second,
		// Discard the panic stack-trace log the boundary emits; the test still
		// exercises the logging path via the configured handler.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)

	// Send HELLO without waiting for a reply. handleHello calls
	// panickingAuth.Authenticate, which panics inside the connection goroutine.
	c.sendRequest(t, &proto.Hello{
		Extra: map[string]interface{}{
			"scheme":      "none",
			"principal":   "test",
			"credentials": "",
			"agent":       "test/1.0",
		},
	})

	// The server recovers the panic and closes the connection. The client's
	// next read must therefore fail (EOF / connection reset) rather than
	// returning a response — and crucially, the process is still running to
	// observe it.
	if _, readErr := c.cr.ReadMessage(); readErr == nil {
		t.Fatal("expected connection to be closed after handler panic, but a message was received")
	}

	// The recover incremented the panic counter. Poll briefly: the connection
	// goroutine's deferred recover runs slightly after our read observes the
	// close, so allow a short window before asserting.
	deadline := time.Now().Add(2 * time.Second)
	for probe.connPanics.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := probe.connPanics.Load(); got != 1 {
		t.Fatalf("bolt.server.conn.panics = %d, want 1", got)
	}
}
