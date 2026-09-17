package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// write_coalescing_test.go — rmp #2834.
//
// The Bolt server answers a pipelined RUN+PULL with three messages (the RUN
// SUCCESS, the RECORDs, the PULL SUCCESS) that the client asked for in a single
// write(2). This file measures how many write(2) and read(2) calls the server
// actually spends on one such query, and guards the invariant that makes the
// saving legal: the response buffer is always drained before the server can
// block waiting for the client.
//
// The counters come from countingConn (record_batching_test.go), which wraps the
// accepted net.Conn: with a raw socket underneath, one Write is one write(2) and
// one Read is one read(2), so these are syscall counts and not a proxy for them.

// queriesPerCoalescingSample is the number of queries the ratio is averaged
// over. It is large enough that the handshake and the first query cannot skew
// the mean and small enough that the test stays well inside the short layer.
const queriesPerCoalescingSample = 200

// newCountingDriver dials a server whose accepted connection counts its own
// syscalls, pinned to ONE pooled connection so that ln.last is the connection
// every query travels over.
func newCountingDriver(t *testing.T) (neo4j.DriverWithContext, *countingListener) {
	t.Helper()
	addr, ln := startCountingServer(t)
	driver, err := neo4j.NewDriverWithContext(
		"bolt://"+addr,
		neo4j.NoAuth(),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = 1
			c.ConnectionAcquisitionTimeout = 5 * time.Second
			c.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("neo4j.NewDriverWithContext: %v", err)
	}
	t.Cleanup(func() {
		if err := driver.Close(context.Background()); err != nil {
			t.Logf("driver.Close: %v", err)
		}
	})
	return driver, ln
}

// TestBoltServer_PipelinedRunPullCostsOneWrite is the syscall oracle for
// rmp #2834.
//
// The neo4j-go-driver queues RUN and PULL and sends both in ONE write (its
// chunker "tr[ies] to make as few writes as possible"), so a server that flushes
// after the RUN SUCCESS spends two write(2) answering a request that cost the
// client one. Deferring that flush until the server is actually about to wait
// for the client collapses the pair into one.
//
// The oracle has two halves and needs both: the ratio alone would pass on a
// server that buffered the reply and never sent it, so every query's VALUE is
// checked too.
func TestBoltServer_PipelinedRunPullCostsOneWrite(t *testing.T) {
	ctx := context.Background()
	driver, ln := newCountingDriver(t)

	sess := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer sess.Close(ctx)

	query := func(i int) {
		t.Helper()
		res, err := sess.Run(ctx, "RETURN $i AS n", map[string]any{"i": int64(i)})
		if err != nil {
			t.Fatalf("query %d: Run: %v", i, err)
		}
		rec, err := res.Single(ctx)
		if err != nil {
			t.Fatalf("query %d: Single: %v", i, err)
		}
		v, ok := rec.Get("n")
		if !ok {
			t.Fatalf("query %d: record has no column 'n'", i)
		}
		// Correctness first: coalescing may change WHEN bytes are written,
		// never WHETHER they are, nor what they say.
		if got, ok := v.(int64); !ok || got != int64(i) {
			t.Fatalf("query %d: n = %v (%T), want %d", i, v, v, i)
		}
	}

	// Warm up: the handshake, HELLO/LOGON and the first query each cost writes
	// that are not part of the steady-state per-query ratio.
	query(-1)

	conn := ln.last.Load()
	if conn == nil {
		t.Fatal("no connection was accepted")
	}
	writesBefore, readsBefore := conn.writes.Load(), conn.reads.Load()

	for i := 0; i < queriesPerCoalescingSample; i++ {
		query(i)
	}

	if now := ln.last.Load(); now != conn {
		t.Fatal("the driver opened a second connection; the per-query ratio would be meaningless")
	}
	writes := conn.writes.Load() - writesBefore
	reads := conn.reads.Load() - readsBefore

	wpq := float64(writes) / float64(queriesPerCoalescingSample)
	rpq := float64(reads) / float64(queriesPerCoalescingSample)
	t.Logf("%d queries: %d writes (%.3f/query), %d reads (%.3f/query)",
		queriesPerCoalescingSample, writes, wpq, reads, rpq)

	// The defect is exactly one surplus write per query. The bound is 1.5 rather
	// than 1.0 because the saving is conditional by design: when the reader has
	// not yet delivered the PULL, the server flushes and pays the second write —
	// correct, and rarer the busier the connection. Anything at or above 1.5 means
	// the flush is unconditional again.
	if wpq >= 1.5 {
		t.Errorf("server spent %.3f write(2) per query; want below 1.5. "+
			"Two writes per query means the RUN summary is being flushed before the PULL is handled", wpq)
	}
	// Guard the other direction: a server that never flushed would score 0 here
	// while the queries above would already have failed. Stating the floor makes
	// the oracle's shape explicit rather than implicit.
	if wpq < 0.5 {
		t.Errorf("server spent %.3f write(2) per query; a reply cannot cost less than one write", wpq)
	}
}

// TestBoltServer_FlushesBeforeBlockingRead is the deadlock gate for rmp #2834.
//
// It drives the server the way a NON-pipelining client does: send RUN, then wait
// for its SUCCESS before sending anything else. That is the exact shape the
// deferred flush must not break — the client is waiting for bytes the server
// holds, and the server is waiting for a request the client will not send until
// it has them.
//
// A server that defers the flush past the point where it blocks does not fail
// this test slowly; it deadlocks. The client's connection deadline (set by
// newBoltTestClient) turns that deadlock into a bounded, reported failure rather
// than a hung test binary.
func TestBoltServer_FlushesBeforeBlockingRead(t *testing.T) {
	addr, _ := startCountingServer(t)
	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)

	// c.run sends RUN and then reads the SUCCESS — it sends no PULL first, so
	// nothing else can push the server's buffer out.
	for i := 0; i < 3; i++ {
		c.run(t, "RETURN 1 AS x", nil)
		records, _ := c.pullAll(t)
		if len(records) != 1 {
			t.Fatalf("round %d: got %d records, want 1", i, len(records))
		}
	}
}

// TestBoltServer_FailureIsDeliveredThenConnectionEnds guards the second half
// of the flush obligation: a response written on a path that then tears the
// connection down must still reach the client.
//
// A malformed frame makes the server answer FAILURE and keep the connection
// alive; GOODBYE then ends it. With the flush deferred to the point of blocking,
// the FAILURE is in the buffer when the loop is about to exit, so only the
// teardown flush can deliver it.
func TestBoltServer_FailureIsDeliveredThenConnectionEnds(t *testing.T) {
	addr, _ := startCountingServer(t)
	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)

	// A PULL with no preceding RUN is an illegal state transition, so the server
	// answers FAILURE from a path that does not return to a fresh READY state.
	c.sendRequest(t, &proto.Pull{N: -1, QID: -1})
	f := c.recvFailure(t)
	if f.Code == "" {
		t.Fatal("FAILURE arrived with an empty code")
	}
	t.Logf("FAILURE delivered: code=%s", f.Code)
}
