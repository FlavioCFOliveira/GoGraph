package server_test

// security_reader_panic_boundary_test.go — regression for the per-connection
// READER goroutine panic boundary (SEC-2026-06-14c finding #1491). The reader
// goroutine that loops on ChunkedReader.ReadMessage owns the connection read
// side. handleConn installs a defer/recover boundary on its own goroutine, but
// the reader had none: a recoverable panic on the reader path would unwind
// uncaught and crash the WHOLE process, taking down every other live
// connection. No adversarial-byte path reaches such a panic today (the
// read/framing path is panic-free), so this is defence-in-depth.
//
// The panic is injected through a test-only hook (SetReaderPanicHookForTest)
// invoked at the top of the reader's read loop — the only deterministic,
// non-flaky way to drive the reader goroutine into a recoverable panic without
// a production-only seam on the hot read path. The test asserts the process
// survives, the connection is torn down, and bolt.server.conn.panics fires.

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// readerPanicProbe records how many times the bolt connection-panic counter
// fired. The global metrics backend is swapped atomically; this test must not
// run in parallel with anything else that installs a backend.
type readerPanicProbe struct {
	connPanics atomic.Uint64
}

func (p *readerPanicProbe) IncCounter(name string, delta uint64) {
	if name == "bolt.server.conn.panics" {
		p.connPanics.Add(delta)
	}
}

func (p *readerPanicProbe) ObserveLatency(string, time.Duration) {}

// TestBoltServer_RecoversReaderGoroutinePanic verifies that a recoverable panic
// raised on the per-connection READER goroutine is recovered inside that
// goroutine: the test process survives, the connection is closed, and the
// bolt.server.conn.panics counter is incremented. Without the reader's recover
// boundary the panic would unwind past the goroutine and crash the whole test
// binary (#1491).
func TestBoltServer_RecoversReaderGoroutinePanic(t *testing.T) {
	probe := &readerPanicProbe{}
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	// Inject a one-shot panic on the reader goroutine's first read iteration.
	var fired atomic.Bool
	restore := server.SetReaderPanicHookForTest(func() {
		if fired.CompareAndSwap(false, true) {
			panic("readerPanicProbe: injected test panic on reader goroutine")
		}
	})
	t.Cleanup(restore)

	addr := startTestServer(t, server.Options{
		Auth:        server.NoAuthHandler{},
		ConnTimeout: 5 * time.Second,
		// Discard the panic stack-trace log the boundary emits; the test still
		// exercises the logging path via the configured handler.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)

	// The reader goroutine starts after the handshake; its first read-loop
	// iteration invokes the hook and panics. The recover tears the connection
	// down, so the client's next read must fail rather than return a message —
	// and crucially, the process is still running to observe it.
	if _, readErr := c.cr.ReadMessage(); readErr == nil {
		t.Fatal("expected connection to be closed after reader panic, but a message was received")
	}

	// The recover incremented the panic counter. Poll briefly: the reader
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
