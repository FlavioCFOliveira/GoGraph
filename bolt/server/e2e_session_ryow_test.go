package server_test

// e2e_session_ryow_test.go — a Bolt client observes its own committed writes on the
// SAME connection, over the official neo4j-go-driver (rmp #2329, acceptance
// criterion 1).
//
// # Why this needs a transport-level gate
//
// The engine-level gate is cypher/session_test.go. This one exists because the
// guarantee has to survive the wire: it is worth nothing unless the SERVER binds a
// session to the connection. A Bolt connection is a session by definition — one
// client, one ordered conversation — and a client that writes and then reads on it
// expects to see its own write, exactly as it would against any database driver.
//
// Before the server bound one, every statement went through the engine's sessionless
// entry points, so a client got snapshot isolation with no cross-statement promise
// and could miss its own write whenever an unrelated in-flight commit was holding
// the contiguous visible frontier back (rmp #2328).
//
// # Each goroutine gets its OWN driver
//
// neo4j-go-driver v5.28.4 races inside its own Connector when two sessions borrowed
// from ONE driver connect concurrently (both sides at connector.go:55-56, seen under
// -race). That is the driver's bug, not the server's, and it is avoided rather than
// worked around: a driver per goroutine is also the more faithful model of separate
// clients, which is what the property is about.
//
// # Why the noise writers are not decoration
//
// That gap is only reachable while OTHER commits are in flight. On a quiet server a
// commit becomes visible immediately and a sessionless read passes too, so a test
// without concurrent unrelated writers cannot fail on the broken build and would
// assert nothing.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// TestE2E_SessionReadsItsOwnWritesOverBolt writes and reads repeatedly on one
// connection while other connections commit continuously.
func TestE2E_SessionReadsItsOwnWritesOverBolt(t *testing.T) {
	ctx := context.Background()
	addr := startTestServer(t, server.Options{ConnTimeout: 30 * time.Second})
	// Unrelated writers on their OWN drivers and connections, holding the frontier
	// busy.
	stop := make(chan struct{})
	var noise sync.WaitGroup
	for w := 0; w < 4; w++ {
		nd := newSnapDriver(t, addr)
		noise.Add(1)
		go func(id int, drv neo4j.DriverWithContext) {
			defer noise.Done()
			s := drv.NewSession(ctx, neo4j.SessionConfig{})
			defer func() { _ = s.Close(ctx) }()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = s.Run(ctx, fmt.Sprintf("CREATE (:Noise {w: %d, n: %d})", id, n), nil)
			}
		}(w, nd)
	}
	defer func() { close(stop); noise.Wait() }()

	// THE CLIENT: its own driver, one connection, write then read, repeatedly.
	mine := newSnapDriver(t, addr).NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = mine.Close(ctx) }()

	const rounds = 40
	for i := 1; i <= rounds; i++ {
		if _, err := mine.Run(ctx, fmt.Sprintf("CREATE (:Mine {i: %d})", i), nil); err != nil {
			t.Fatalf("round %d write: %v", i, err)
		}
		res, err := mine.Run(ctx, "MATCH (n:Mine) RETURN count(n) AS n", nil)
		if err != nil {
			t.Fatalf("round %d read: %v", i, err)
		}
		rec, err := res.Single(ctx)
		if err != nil {
			t.Fatalf("round %d Single: %v", i, err)
		}
		got, ok := rec.Values[0].(int64)
		if !ok {
			t.Fatalf("round %d: count is %T, want int64", i, rec.Values[0])
		}
		if got != int64(i) {
			t.Fatalf("round %d: the connection sees %d of its own nodes, want %d. A Bolt "+
				"connection is a session: a client that writes and then reads on it must "+
				"observe its own committed write", i, got, i)
		}
	}
}

// TestE2E_SessionTransactionObservesItsOwnEarlierWritesOverBolt covers the explicit
// transaction path, which takes its snapshot when the transaction OPENS — so the
// wait must happen at BEGIN or every statement in the transaction reads stale.
func TestE2E_SessionTransactionObservesItsOwnEarlierWritesOverBolt(t *testing.T) {
	ctx := context.Background()
	addr := startTestServer(t, server.Options{ConnTimeout: 30 * time.Second})
	stop := make(chan struct{})
	var noise sync.WaitGroup
	for w := 0; w < 4; w++ {
		nd := newSnapDriver(t, addr)
		noise.Add(1)
		go func(id int, drv neo4j.DriverWithContext) {
			defer noise.Done()
			s := drv.NewSession(ctx, neo4j.SessionConfig{})
			defer func() { _ = s.Close(ctx) }()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = s.Run(ctx, fmt.Sprintf("CREATE (:TxNoise {w: %d, n: %d})", id, n), nil)
			}
		}(w, nd)
	}
	defer func() { close(stop); noise.Wait() }()

	mine := newSnapDriver(t, addr).NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = mine.Close(ctx) }()

	for i := 1; i <= 20; i++ {
		if _, err := mine.Run(ctx, fmt.Sprintf("CREATE (:TxMine {i: %d})", i), nil); err != nil {
			t.Fatalf("round %d autocommit write: %v", i, err)
		}
		tx, err := mine.BeginTransaction(ctx)
		if err != nil {
			t.Fatalf("round %d BeginTransaction: %v", i, err)
		}
		res, err := tx.Run(ctx, "MATCH (n:TxMine) RETURN count(n) AS n", nil)
		if err != nil {
			_ = tx.Close(ctx)
			t.Fatalf("round %d tx read: %v", i, err)
		}
		rec, err := res.Single(ctx)
		if err != nil {
			_ = tx.Close(ctx)
			t.Fatalf("round %d tx Single: %v", i, err)
		}
		got, ok := rec.Values[0].(int64)
		if !ok {
			_ = tx.Close(ctx)
			t.Fatalf("round %d: count is %T, want int64", i, rec.Values[0])
		}
		if got != int64(i) {
			_ = tx.Close(ctx)
			t.Fatalf("round %d: a transaction opened on this connection sees %d of the "+
				"connection's own nodes, want %d. The snapshot is taken at BEGIN, so the "+
				"session's wait has to happen there", i, got, i)
		}
		if err := tx.Close(ctx); err != nil {
			t.Fatalf("round %d tx Close: %v", i, err)
		}
	}
}
