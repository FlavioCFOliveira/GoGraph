package server_test

// streaming_backpressure_test.go — regression gate for task #1350: PULL must
// stream RECORD messages incrementally instead of materialising the whole
// page into a second in-memory copy before the first byte is written.
//
// The server is driven over a net.Pipe connection: a synchronous, unbuffered
// wire with no kernel socket buffering, so a write on the server side blocks
// until this test reads it. After RUN materialises a large engine Result, the
// test sends PULL {n:-1} and deliberately reads nothing, parking the server
// on its first record write. In that frozen state the live heap tells the two
// behaviours apart deterministically:
//
//   - pre-fix: handlePull has already iterated the ENTIRE cursor — releasing
//     the engine-side rows — and duplicated every record into the response
//     slice that the serve loop holds while blocked on the first write. The
//     live heap moves by the full result-set size (measured ~-55 MiB here:
//     the released engine rows outweigh the packstream duplicate).
//   - post-fix: handlePull is blocked inside the record sink on row one. The
//     engine Result is still intact (in the baseline) and no duplicate
//     exists, so the live heap stays flat.
//
// The gate therefore asserts the live-heap delta stays within a small bound
// in BOTH directions: a large negative delta betrays "cursor fully drained
// before the first byte" (rows were not streamed as produced) and a large
// positive delta betrays a second in-memory copy of the page. Either way the
// PULL was buffered, which is the audit finding.
//
// The test then drains the stream and verifies protocol integrity: every
// record arrives, followed by a single SUCCESS with has_more=false.

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// pullStreamRows is the result-set size for the streaming gate: large enough
// that a buffered PULL moves the live heap by an order of magnitude more than
// pullStreamMaxDelta in at least one direction.
const pullStreamRows = 200_000

// pullStreamMaxDelta bounds the tolerated live-heap movement, in bytes, in
// either direction between "engine Result materialised" and "server parked on
// its first blocked record write". A buffered PULL of pullStreamRows rows
// moves the live heap by tens of MiB; an incremental one leaves it flat.
const pullStreamMaxDelta = 8 << 20 // 8 MiB

func TestPullStreaming_BoundedMemoryUnderSlowReader(t *testing.T) {
	// NOT t.Parallel(): the assertion measures process-global live heap, so no
	// other test may run concurrently.

	cli := startPipeServer(t)

	c := &boltTestClient{
		conn: cli,
		cr:   proto.NewChunkedReader(cli),
		cw:   proto.NewChunkedWriter(cli),
	}
	defer c.close(t)
	if err := cli.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	c.negotiate(t)
	c.hello(t)

	// 128-byte literal per row: the payload bytes are shared server-side (one
	// string literal), so the baseline stays cheap while each row still has a
	// non-trivial wire footprint.
	pad := strings.Repeat("x", 128)
	c.run(t, fmt.Sprintf("UNWIND range(1, %d) AS i RETURN i, '%s' AS pad", pullStreamRows, pad), nil)

	// Live-heap baseline AFTER the engine Result is fully materialised.
	base := liveHeapBytes()

	// PULL everything, but do not read a single response byte: the pipe has no
	// buffering, so the server parks on its first record write.
	c.sendRequest(t, &proto.Pull{N: -1, QID: -1})

	// Wait for the server to park, then measure the live-heap movement.
	delta := waitStableHeapDelta(t, base)
	t.Logf("live-heap delta while server parked mid-PULL: %+.2f MiB", float64(delta)/(1<<20))
	if delta > pullStreamMaxDelta || delta < -pullStreamMaxDelta {
		t.Errorf("PULL buffered the page instead of streaming it: live-heap moved %+.2f MiB (bound ±%.0f MiB) for %d rows — the cursor was drained and duplicated in memory before the first byte was written",
			float64(delta)/(1<<20), float64(pullStreamMaxDelta)/(1<<20), pullStreamRows)
	}

	// Drain the stream and verify protocol integrity: pullStreamRows RECORDs
	// followed by exactly one SUCCESS with has_more=false.
	if err := cli.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	records := 0
	for {
		msg := c.recvResponse(t)
		switch m := msg.(type) {
		case *proto.Record:
			if len(m.Data) != 2 {
				t.Fatalf("record %d: got %d values, want 2", records, len(m.Data))
			}
			records++
		case *proto.Success:
			if records != pullStreamRows {
				t.Fatalf("drained %d records before SUCCESS, want %d", records, pullStreamRows)
			}
			if hasMore, _ := m.Metadata["has_more"].(bool); hasMore {
				t.Fatal("final SUCCESS reports has_more=true, want false")
			}
			return
		case *proto.Failure:
			t.Fatalf("PULL failed mid-stream after %d records: code=%s message=%s", records, m.Code, m.Message)
		default:
			t.Fatalf("unexpected message type %T after %d records", msg, records)
		}
	}
}

// startPipeServer starts a bolt server whose listener yields exactly one
// net.Pipe connection and returns the client end. Cleanup cancels the server
// context and waits for Serve to return (closing the pipe unblocks any
// parked read or write).
func startPipeServer(t *testing.T) net.Conn {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	ln := newSingleConnListener(srvConn)

	srv, err := server.NewServer(newEngine(t), server.Options{
		// ConnTimeout doubles as the per-write deadline; keep it well above
		// the measurement window so the parked write does not trip mid-test.
		ConnTimeout: 2 * time.Minute,
		Auth:        server.NoAuthHandler{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()

	t.Cleanup(func() {
		cancel()
		_ = cliConn.Close() //nolint:errcheck // teardown; unblocks any parked server write
		select {
		case <-serveErr:
		case <-time.After(10 * time.Second):
			t.Log("startPipeServer: Serve goroutine did not exit in cleanup")
		}
	})
	return cliConn
}

// singleConnListener is a net.Listener that yields one pre-created connection
// from its first Accept and blocks every subsequent Accept until Close. It is
// safe for the concurrent Accept/Close that [server.Server.Serve] performs.
type singleConnListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newSingleConnListener(c net.Conn) *singleConnListener {
	l := &singleConnListener{ch: make(chan net.Conn, 1), done: make(chan struct{})}
	l.ch <- c
	return l
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *singleConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return pipeAddr{} }

// pipeAddr is the static address reported by [singleConnListener].
type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// liveHeapBytes returns the live heap (reachable objects only) after a forced
// collection, so transient per-row garbage from streaming does not pollute
// the measurement.
func liveHeapBytes() int64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc) //nolint:gosec // G115: HeapAlloc is far below MaxInt64 in any realistic test process
}

// waitStableHeapDelta polls the live heap until it has been stable (four
// consecutive readings within 256 KiB of each other) for at least one second,
// then returns the movement relative to base. The minimum elapsed time
// guarantees a buffered handlePull has finished materialising its response
// slice before the measurement is accepted; while it is still appending,
// consecutive readings differ by megabytes and reset the stability counter.
func waitStableHeapDelta(t *testing.T, base int64) int64 {
	t.Helper()
	const (
		pollEvery   = 150 * time.Millisecond
		tolerance   = 256 << 10 // 256 KiB
		needStable  = 4
		minElapsed  = time.Second
		maxWaitTime = 30 * time.Second
	)
	start := time.Now()
	prev := liveHeapBytes()
	stable := 0
	for {
		if time.Since(start) > maxWaitTime {
			t.Fatalf("live heap did not stabilise within %s", maxWaitTime)
		}
		time.Sleep(pollEvery)
		cur := liveHeapBytes()
		diff := cur - prev
		if diff < 0 {
			diff = -diff
		}
		if diff < tolerance {
			stable++
		} else {
			stable = 0
		}
		prev = cur
		if stable >= needStable && time.Since(start) >= minElapsed {
			return cur - base
		}
	}
}
