package server_test

// e2e_readtx_snapshot_test.go — an explicit Bolt transaction observes ONE
// instant for all of its statements, over the official neo4j-go-driver
// (rmp #2307, sprint 334, acceptance criterion 2).
//
// The engine-level gate is cypher/readtx_snapshot_test.go. This one exists
// because the property has to survive the transport: a Bolt BEGIN with
// mode="r" routes to [cypher.Engine.BeginReadTx] (bolt/server/tx.go:86) and a
// default BEGIN routes to [cypher.Engine.BeginTx], and the two reach snapshot
// isolation by DIFFERENT mechanisms — the read handle by pinning one snapshot
// for its lifetime, the write handle by holding the visibility barrier
// exclusively so nothing else can publish a commit while it runs. Both are
// asserted here, because a client cannot tell which mechanism it is relying on
// and the contract it is promised is the same.

import (
	"context"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// countSnapNodes runs the count inside tx and returns the scalar.
func countSnapNodes(ctx context.Context, t *testing.T, tx neo4j.ExplicitTransaction, what string) int64 {
	t.Helper()
	res, err := tx.Run(ctx, "MATCH (p:Snap) RETURN count(p) AS n", nil)
	if err != nil {
		t.Fatalf("%s: Run: %v", what, err)
	}
	rec, err := res.Single(ctx)
	if err != nil {
		t.Fatalf("%s: Single: %v", what, err)
	}
	n, ok := rec.Values[0].(int64)
	if !ok {
		t.Fatalf("%s: count is %T, want int64", what, rec.Values[0])
	}
	return n
}

// TestE2E_ReadTxSnapshotIsolationOverBolt drives the read-mode contract end to
// end: BEGIN with mode="r", read, let a SEPARATE session commit, read again.
// The second read must not see the commit.
func TestE2E_ReadTxSnapshotIsolationOverBolt(t *testing.T) {
	ctx := context.Background()
	addr := startTestServer(t, server.Options{ConnTimeout: 15 * time.Second})
	drv := newSnapDriver(t, addr)

	// Seed one node through an ordinary autocommit session.
	writer := drv.NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = writer.Close(ctx) }()
	if _, err := writer.Run(ctx, "CREATE (:Snap {n: 1})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := writer.Run(ctx, "MATCH (p:Snap) RETURN count(p)", nil); err != nil {
		t.Fatalf("seed barrier: %v", err) // forces the seed to have been applied
	}

	reader := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer func() { _ = reader.Close(ctx) }()
	rtx, err := reader.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction(read): %v", err)
	}
	defer func() { _ = rtx.Close(ctx) }()

	if got := countSnapNodes(ctx, t, rtx, "first read"); got != 1 {
		t.Fatalf("first read saw %d, want 1", got)
	}

	// A different session commits while the read transaction is open. It is not
	// blocked: a read handle takes no writer serialisation and no barrier.
	if _, err := writer.Run(ctx, "CREATE (:Snap {n: 2})", nil); err != nil {
		t.Fatalf("interleaved commit: %v", err)
	}
	if _, err := writer.Run(ctx, "MATCH (p:Snap) RETURN count(p)", nil); err != nil {
		t.Fatalf("interleaved commit barrier: %v", err)
	}

	if got := countSnapNodes(ctx, t, rtx, "second read"); got != 1 {
		t.Fatalf("second read saw %d, want 1: the Bolt read transaction observed a commit that "+
			"landed after it began, which is read-committed, not snapshot isolation", got)
	}
	if err := rtx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// After the transaction closes, a fresh read transaction sees both.
	after, err := reader.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction(after): %v", err)
	}
	defer func() { _ = after.Close(ctx) }()
	if got := countSnapNodes(ctx, t, after, "read after"); got != 2 {
		t.Fatalf("a read transaction opened after the commit saw %d, want 2", got)
	}
	if err := after.Commit(ctx); err != nil {
		t.Fatalf("Commit(after): %v", err)
	}
}

// TestE2E_WriteTxSnapshotIsolationOverBolt drives the DEFAULT (write) mode. A
// write transaction reaches the same guarantee by a different route: it holds
// the visibility barrier exclusively from BEGIN to COMMIT, so a concurrent
// writer cannot publish a commit in between — the interleaved write below
// blocks on the writer serialisation and lands only after this transaction
// commits.
//
// The assertion is therefore not that the barrier exists, but that a client
// gets the same observable contract on both modes: no commit from anyone else
// becomes visible mid-transaction.
func TestE2E_WriteTxSnapshotIsolationOverBolt(t *testing.T) {
	ctx := context.Background()
	addr := startTestServer(t, server.Options{ConnTimeout: 15 * time.Second})
	drv := newSnapDriver(t, addr)

	seed := drv.NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = seed.Close(ctx) }()
	if _, err := seed.Run(ctx, "CREATE (:Snap {n: 1})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := seed.Run(ctx, "MATCH (p:Snap) RETURN count(p)", nil); err != nil {
		t.Fatalf("seed barrier: %v", err)
	}

	main := drv.NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = main.Close(ctx) }()
	wtx, err := main.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction(write): %v", err)
	}

	if got := countSnapNodes(ctx, t, wtx, "first read"); got != 1 {
		t.Fatalf("first read saw %d, want 1", got)
	}

	// A separate session commits while the write transaction is open. Since
	// rmp #2305 it does NOT queue — nothing serialises writers — so it lands
	// immediately; the point of the test is unchanged and is now the only point: it
	// cannot become VISIBLE to the statements below, because wtx reads through the
	// snapshot it took at BEGIN.
	landed := make(chan error, 1)
	go func() {
		other := drv.NewSession(ctx, neo4j.SessionConfig{})
		defer func() { _ = other.Close(ctx) }()
		_, err := other.Run(ctx, "CREATE (:Snap {n: 2})", nil)
		if err == nil {
			_, err = other.Run(ctx, "MATCH (p:Snap) RETURN count(p)", nil)
		}
		landed <- err
	}()

	// Wait for it to actually land. Before rmp #2305 this was a 250 ms window that
	// normally EXPIRED, because the commit was parked behind the writer
	// serialisation, and the error was collected later after Commit released it.
	// Now it completes promptly and must be collected HERE — collecting it twice is
	// what hung this test for the full ten-minute timeout when the barrier hold went,
	// since the second receive had nothing left to read.
	//
	// The assertion this feeds is stronger than the old one: the interleaved commit is
	// known to have COMPLETED before the reads below, so their seeing 1 is genuine
	// snapshot isolation rather than the commit simply not having happened yet.
	select {
	case err := <-landed:
		if err != nil {
			t.Fatalf("interleaved commit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the interleaved commit did not land while a write transaction was open. " +
			"Since rmp #2305 an open transaction blocks no writer, so it must complete " +
			"promptly; if it is parked, a transaction-lifetime serialiser has been " +
			"reintroduced.")
	}

	if got := countSnapNodes(ctx, t, wtx, "second read"); got != 1 {
		t.Fatalf("second read saw %d, want 1: a concurrent commit became visible inside an open "+
			"write transaction", got)
	}

	// This transaction's own write IS visible to its own later statements —
	// snapshot isolation is not a wall against yourself.
	if _, err := wtx.Run(ctx, "CREATE (:Snap {n: 3})", nil); err != nil {
		t.Fatalf("own write: %v", err)
	}
	if got := countSnapNodes(ctx, t, wtx, "read own write"); got != 2 {
		t.Fatalf("after its own CREATE the transaction saw %d, want 2: read-your-own-writes is broken", got)
	}

	if err := wtx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Everything is visible now: the seed, this transaction's write, and the
	// one that was queued behind it.
	verify := drv.NewSession(ctx, neo4j.SessionConfig{})
	defer func() { _ = verify.Close(ctx) }()
	res, err := verify.Run(ctx, "MATCH (p:Snap) RETURN count(p) AS n", nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	rec, err := res.Single(ctx)
	if err != nil {
		t.Fatalf("verify single: %v", err)
	}
	if got := rec.Values[0].(int64); got != 3 {
		t.Fatalf("after everything committed the graph has %d :Snap nodes, want 3", got)
	}
}

// newSnapDriver opens a driver against addr and closes it with the test.
func newSnapDriver(t *testing.T, addr string) neo4j.DriverWithContext {
	t.Helper()
	drv, err := neo4j.NewDriverWithContext("bolt://"+addr, neo4j.NoAuth())
	if err != nil {
		t.Fatalf("NewDriverWithContext: %v", err)
	}
	t.Cleanup(func() { _ = drv.Close(context.Background()) })
	return drv
}
