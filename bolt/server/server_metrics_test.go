package server_test

// server_metrics_test.go — regression test for the Bolt server-level
// observability metrics (#1314).
//
// The Bolt server emits, through the shared internal/metrics backend, the
// signals an operator needs to correlate a connection flood or a transaction
// leak:
//
//   - bolt.server.conn.accepted / .conn.closed — paired counters whose
//     difference is the live-connection gauge (accepted − closed).
//   - bolt.server.conn.rejected — connections refused because the
//     MaxConnections semaphore was full.
//   - bolt.server.tx.opened / .tx.closed — paired counters whose difference is
//     the open-transaction gauge (opened − closed).
//   - bolt.server.tx.abandoned — transactions still open at an abnormal
//     disconnect (no COMMIT/ROLLBACK/RESET before the socket dropped).
//
// Both gauge derivations must return to zero once the server is quiescent (no
// phantom live connection, no leaked open transaction). The two ACs are:
//
//   (a) MaxConnections:1 — a second concurrent connection is rejected, so
//       bolt.server.conn.rejected increments.
//   (b) BEGIN + RUN write then disconnect without COMMIT — the teardown rollback
//       counts the transaction abandoned (bolt.server.tx.abandoned) and the
//       open-transaction gauge (opened − closed) returns to zero.
//
// The metrics backend is a process-global swapped via metrics.SetBackend, so
// this test must not run in parallel with anything else that installs a backend
// (it does not call t.Parallel) and restores the no-op default on cleanup.

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// serverMetricsProbe captures every bolt.server.* counter so a test can read
// the raw counters and the two derived gauges. It implements metrics.Backend.
type serverMetricsProbe struct {
	mu     sync.Mutex
	counts map[string]uint64
}

func newServerMetricsProbe() *serverMetricsProbe {
	return &serverMetricsProbe{counts: map[string]uint64{}}
}

func (p *serverMetricsProbe) IncCounter(name string, delta uint64) {
	p.mu.Lock()
	p.counts[name] += delta
	p.mu.Unlock()
}

func (p *serverMetricsProbe) ObserveLatency(string, time.Duration) {}

// get returns the current value of a counter (0 if never emitted).
func (p *serverMetricsProbe) get(name string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[name]
}

// connActive is the live-connection gauge derivation (accepted − closed).
func (p *serverMetricsProbe) connActive() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(p.counts["bolt.server.conn.accepted"]) - int64(p.counts["bolt.server.conn.closed"])
}

// txOpen is the open-transaction gauge derivation (opened − closed).
func (p *serverMetricsProbe) txOpen() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(p.counts["bolt.server.tx.opened"]) - int64(p.counts["bolt.server.tx.closed"])
}

// waitFor polls until pred() is true or the deadline elapses. It returns
// whether pred() became true. Used to wait for an asynchronous lifecycle event
// (a connection goroutine's deferred close counter, a teardown rollback) to
// land before asserting on the derived gauge.
func waitFor(pred func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return pred()
}

// TestServerMetrics_RejectedConnection is AC (a): with MaxConnections:1 the
// server admits exactly one connection and rejects a second concurrent one. The
// accepted connection increments bolt.server.conn.accepted; the rejected one
// increments bolt.server.conn.rejected (and never becomes a live connection, so
// it does not touch the accepted counter). After both connections close, the
// live-connection gauge (accepted − closed) returns to zero.
//
// Pre-fix the rejection site only logged a warning with no counter, so
// bolt.server.conn.rejected was absent (0) however many connections were
// refused.
func TestServerMetrics_RejectedConnection(t *testing.T) {
	probe := newServerMetricsProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	addr := startTestServer(t, server.Options{
		MaxConnections: 1,
		ConnTimeout:    5 * time.Second,
	})

	// conn1 takes the single semaphore slot. Complete the handshake so the slot
	// is firmly held while conn2 is attempted.
	conn1, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial conn1: %v", err)
	}
	defer conn1.Close() //nolint:errcheck
	_ = conn1.SetDeadline(time.Now().Add(3 * time.Second))
	boltHandshake(t, conn1)

	// The accepted counter must have fired for conn1.
	if !waitFor(func() bool { return probe.get("bolt.server.conn.accepted") >= 1 }, 2*time.Second) {
		t.Fatalf("bolt.server.conn.accepted = %d, want >= 1 after conn1 accepted", probe.get("bolt.server.conn.accepted"))
	}

	// conn2: the semaphore is full, so the server closes it immediately and must
	// count the rejection. Read to observe the typed close.
	conn2, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close() //nolint:errcheck
	_ = conn2.SetDeadline(time.Now().Add(2 * time.Second))
	var buf [4]byte
	_, _ = io.ReadFull(conn2, buf[:]) //nolint:errcheck // expect EOF/typed close on the rejected connection

	if !waitFor(func() bool { return probe.get("bolt.server.conn.rejected") >= 1 }, 2*time.Second) {
		t.Fatalf("bolt.server.conn.rejected = %d, want >= 1 after a connection was refused", probe.get("bolt.server.conn.rejected"))
	}

	// Close both connections and verify the live-connection gauge drains to zero
	// (every accepted connection's goroutine exited and incremented closed).
	_ = conn1.Close() //nolint:errcheck
	_ = conn2.Close() //nolint:errcheck
	if !waitFor(func() bool { return probe.connActive() == 0 }, 3*time.Second) {
		t.Fatalf("live-connection gauge (accepted − closed) = %d, want 0 after all connections closed; accepted=%d closed=%d",
			probe.connActive(), probe.get("bolt.server.conn.accepted"), probe.get("bolt.server.conn.closed"))
	}
}

// TestServerMetrics_AbandonedTransaction is AC (b): a client completes the
// handshake, opens an explicit write transaction (BEGIN + RUN CREATE), then
// drops the socket WITHOUT COMMIT/ROLLBACK/RESET. The connection-teardown
// rollback (#1309) must count the transaction abandoned
// (bolt.server.tx.abandoned) and the open-transaction gauge (opened − closed)
// must return to zero. The accepted connection's gauge must likewise drain to
// zero once its goroutine exits.
//
// A WAL-backed engine is used so the open transaction genuinely holds the engine
// writer serialisation — the resource whose accounting this test asserts.
//
// Pre-fix the teardown reclaimed the transaction (#1309) but emitted no metric,
// so bolt.server.tx.abandoned was absent (0) and an operator had no signal that
// a transaction had leaked into the teardown path.
func TestServerMetrics_AbandonedTransaction(t *testing.T) {
	probe := newServerMetricsProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{})

	c := newBoltTestClient(t, addr)
	c.negotiate(t)
	c.hello(t)
	c.begin(t)
	c.run(t, "CREATE (:AbandonedTx {v:1})", nil)

	// The transaction is now open. The open-transaction gauge must read 1.
	if !waitFor(func() bool { return probe.txOpen() == 1 }, 2*time.Second) {
		t.Fatalf("open-transaction gauge (opened − closed) = %d, want 1 while a transaction is open; opened=%d closed=%d",
			probe.txOpen(), probe.get("bolt.server.tx.opened"), probe.get("bolt.server.tx.closed"))
	}
	// No transaction has been abandoned yet.
	if got := probe.get("bolt.server.tx.abandoned"); got != 0 {
		t.Fatalf("bolt.server.tx.abandoned = %d before any disconnect, want 0", got)
	}

	// Drop the socket abruptly — no COMMIT, ROLLBACK, or RESET. The server's
	// connection teardown must roll the open transaction back and count it
	// abandoned.
	c.close(t)

	if !waitFor(func() bool { return probe.get("bolt.server.tx.abandoned") >= 1 }, 3*time.Second) {
		t.Fatalf("bolt.server.tx.abandoned = %d, want >= 1 after an abnormal disconnect with an open transaction", probe.get("bolt.server.tx.abandoned"))
	}
	// The open-transaction gauge must drain back to zero (the abandoned tx was
	// counted closed by the teardown rollback).
	if !waitFor(func() bool { return probe.txOpen() == 0 }, 3*time.Second) {
		t.Fatalf("open-transaction gauge (opened − closed) = %d, want 0 after teardown; opened=%d closed=%d",
			probe.txOpen(), probe.get("bolt.server.tx.opened"), probe.get("bolt.server.tx.closed"))
	}
	// And the live-connection gauge must drain back to zero too.
	if !waitFor(func() bool { return probe.connActive() == 0 }, 3*time.Second) {
		t.Fatalf("live-connection gauge (accepted − closed) = %d, want 0 after disconnect; accepted=%d closed=%d",
			probe.connActive(), probe.get("bolt.server.conn.accepted"), probe.get("bolt.server.conn.closed"))
	}
}

// TestServerMetrics_OrderlyTransaction_NotAbandoned is the negative companion to
// the abandoned-transaction AC: a transaction ended in an orderly way (COMMIT)
// must NOT be counted abandoned, and both gauges must drain to zero. This guards
// against a future change that wires the abandoned counter onto the shared
// teardown funnel (abortTx) rather than the disconnect-only Close path, which
// would mis-count every orderly transaction whose connection later tears down.
func TestServerMetrics_OrderlyTransaction_NotAbandoned(t *testing.T) {
	probe := newServerMetricsProbe()
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{})

	c := newBoltTestClient(t, addr)
	c.negotiate(t)
	c.hello(t)
	c.begin(t)
	c.run(t, "CREATE (:OrderlyTx {v:1})", nil)
	c.pullAll(t)
	c.commit(t)

	// After COMMIT the open-transaction gauge must already read zero.
	if !waitFor(func() bool { return probe.txOpen() == 0 }, 2*time.Second) {
		t.Fatalf("open-transaction gauge (opened − closed) = %d, want 0 after COMMIT; opened=%d closed=%d",
			probe.txOpen(), probe.get("bolt.server.tx.opened"), probe.get("bolt.server.tx.closed"))
	}

	// A clean GOODBYE + close: the teardown finds no open transaction, so nothing
	// is counted abandoned.
	c.goodbye(t)
	c.close(t)

	// Give the teardown a moment to run, then assert no abandoned count and a
	// drained live-connection gauge.
	if !waitFor(func() bool { return probe.connActive() == 0 }, 3*time.Second) {
		t.Fatalf("live-connection gauge (accepted − closed) = %d, want 0 after orderly close; accepted=%d closed=%d",
			probe.connActive(), probe.get("bolt.server.conn.accepted"), probe.get("bolt.server.conn.closed"))
	}
	if got := probe.get("bolt.server.tx.abandoned"); got != 0 {
		t.Fatalf("bolt.server.tx.abandoned = %d after an orderly COMMIT + GOODBYE, want 0", got)
	}
	// The transaction was counted exactly once opened and once closed.
	if op, cl := probe.get("bolt.server.tx.opened"), probe.get("bolt.server.tx.closed"); op != 1 || cl != 1 {
		t.Fatalf("tx.opened=%d tx.closed=%d, want 1 and 1 (exactly one balanced open/close)", op, cl)
	}
}
