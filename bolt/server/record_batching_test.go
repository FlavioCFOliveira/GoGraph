package server_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// countingConn counts the Write calls the server makes on a connection. With a
// net.Conn underneath, one Write is one write(2) syscall, so this counter is a
// direct measurement of the defect recorded in
// docs/cpu-vs-neo4j-memgraph-2026-08-11.md §4: a K-row Bolt result used to cost
// K syscalls because the framing writer flushed on every message.
type countingConn struct {
	net.Conn
	writes atomic.Int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

// countingListener hands out countingConns and keeps the most recent one, so a
// test can read the server's write count for the connection it just used.
type countingListener struct {
	net.Listener
	last atomic.Pointer[countingConn]
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	cc := &countingConn{Conn: c}
	l.last.Store(cc)
	return cc, nil
}

// startCountingServer is startTestServer with a listener that counts the
// server's writes on the accepted connection.
func startCountingServer(t *testing.T) (string, *countingListener) {
	t.Helper()
	srv, err := server.NewServer(newEngine(t), server.Options{
		ConnTimeout: 5 * time.Second,
		Auth:        server.NoAuthHandler{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln := &countingListener{Listener: raw}
	addr := raw.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("startCountingServer: Serve goroutine did not exit in cleanup")
		}
	})
	time.Sleep(10 * time.Millisecond)
	return addr, ln
}

// TestBoltServer_MultiRecordResultDoesNotWritePerRecord is the end-to-end
// regression test for rmp #2410.
//
// It asserts on the SYSCALL COUNT rather than on elapsed time, because the
// syscall count is the defect itself: a timing assertion would be a load
// question before it was a code question, and would be flaky under a busy host.
//
// The oracle has two halves, and both are needed. The write-count half alone
// would pass on a server that batched the records and never delivered them; the
// record-count half alone is what the old, defective build already satisfied.
func TestBoltServer_MultiRecordResultDoesNotWritePerRecord(t *testing.T) {
	const rows = 500

	addr, ln := startCountingServer(t)
	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)

	// Count only the writes the result stream itself costs, so the handshake
	// and HELLO exchange cannot flatter the measurement.
	before := ln.last.Load().writes.Load()

	c.run(t, "UNWIND range(1, 500) AS i RETURN i", nil)
	records, _ := c.pullAll(t)

	after := ln.last.Load().writes.Load()
	writes := after - before

	// Correctness first: every record must arrive, in full. Batching may change
	// when bytes are written, never whether they are.
	if len(records) != rows {
		t.Fatalf("got %d records, want %d — batching must not lose rows", len(records), rows)
	}
	for i, rec := range records {
		if len(rec) != 1 {
			t.Fatalf("record %d has %d fields, want 1", i, len(rec))
		}
		got, ok := rec[0].(int64)
		if !ok || got != int64(i+1) {
			t.Fatalf("record %d = %v (%T), want %d", i, rec[0], rec[0], i+1)
		}
	}

	// The defect: one write per RECORD. rows+2 covers the RUN summary, every
	// record, and the PULL summary, so the old build could not come in under it.
	// The bound below is deliberately loose — the point is the ORDER, not a
	// tuned constant that would break when the buffer size or the row encoding
	// changes.
	const bound = rows / 4
	if writes >= bound {
		t.Errorf("server issued %d writes for a %d-record result; want fewer than %d. "+
			"One write per record means the framing writer is flushing per message again",
			writes, rows, bound)
	}
	t.Logf("%d-record result delivered in %d writes (%.1f records per write)",
		rows, writes, float64(rows)/float64(max(writes, 1)))
}

// TestBoltServer_SingleRecordResultStillArrives guards the other direction: the
// batching must not depend on a later message arriving to push the buffer out.
// A one-row result is the case where a run of RECORDs is shortest, so it is
// where a missing summary flush would surface first.
func TestBoltServer_SingleRecordResultStillArrives(t *testing.T) {
	addr, _ := startCountingServer(t)
	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)

	c.run(t, "RETURN 1 AS x", nil)
	records, _ := c.pullAll(t)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if got, ok := records[0][0].(int64); !ok || got != 1 {
		t.Fatalf("got %v (%T), want 1", records[0][0], records[0][0])
	}
}

// TestBoltServer_EmptyResultStillArrives covers the run of ZERO records: the
// summary is the only message, and it must still be flushed.
func TestBoltServer_EmptyResultStillArrives(t *testing.T) {
	addr, _ := startCountingServer(t)
	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)

	c.run(t, "UNWIND [] AS i RETURN i", nil)
	records, _ := c.pullAll(t)
	if len(records) != 0 {
		t.Fatalf("got %d records, want 0", len(records))
	}
}
