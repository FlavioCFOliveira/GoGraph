package server_test

// tx_introspection_test.go — rmp #2176 (round-3 comparative audit, finding A1).
//
// During the abandoned-transaction stall the audit found that an operator gets
// one counter and one log line: no session, no principal, no query text, no
// elapsed time, and no way to end the offender. Neo4j ships SHOW TRANSACTIONS
// and TERMINATE TRANSACTIONS in COMMUNITY and Memgraph has both, so the gap
// could not be dismissed as an Enterprise-only feature.
//
// Server.Transactions and Server.TerminateTransaction are the primitives. The
// decisive test here is the last one: it reproduces the stall and RESOLVES it by
// terminating the offender, which is what makes these diagnostics rather than
// decoration.
//
// Layer: short.

import (
	"errors"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// waitForTransactions polls until the server reports want open transactions, or
// fails. Polling rather than sleeping: registration happens on the session's
// goroutine, so its timing is not something the test can assume.
func waitForTransactions(t *testing.T, srv *server.Server, want int) []server.TransactionInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []server.TransactionInfo
	for time.Now().Before(deadline) {
		last = srv.Transactions()
		if len(last) == want {
			return last
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("server reports %d open transactions, want %d: %+v", len(last), want, last)
	return nil
}

// TestTxIntrospection_ListsTheOpenTransaction pins acceptance criterion (1): the
// listing carries the fields an operator needs to identify the offender.
func TestTxIntrospection_ListsTheOpenTransaction(t *testing.T) {
	t.Parallel()
	srv, addr := startTestServerHandle(t, server.Options{
		ConnTimeout:      30 * time.Second,
		DefaultTxTimeout: 20 * time.Second,
		MaxTxIdleTime:    20 * time.Second, // long: the test drives the lifecycle
	})

	if got := srv.Transactions(); len(got) != 0 {
		t.Fatalf("a fresh server reports %d open transactions, want 0", len(got))
	}

	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.helloAs(t, "alice")
	c.begin(t)
	const query = "CREATE (:Introspected {v: 1})"
	c.run(t, query, nil)
	c.pullAll(t)

	infos := waitForTransactions(t, srv, 1)
	got := infos[0]
	if got.ID == "" {
		t.Fatal("the transaction has no id, so it cannot be terminated")
	}
	if got.Principal != "alice" {
		t.Errorf("Principal = %q, want alice", got.Principal)
	}
	if got.Mode != "w" {
		t.Errorf("Mode = %q, want w", got.Mode)
	}
	if got.Remote == "" {
		t.Error("Remote is empty; an operator cannot locate the client")
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	if got.Elapsed <= 0 {
		t.Errorf("Elapsed = %v, want a positive duration", got.Elapsed)
	}
	if got.Query != query {
		t.Errorf("Query = %q, want %q — the listing must show what the transaction is doing", got.Query, query)
	}
	if got.State == "" {
		t.Error("State is empty")
	}
	t.Logf("listed: id=%s principal=%s mode=%s state=%s elapsed=%v query=%q",
		got.ID, got.Principal, got.Mode, got.State, got.Elapsed, got.Query)

	// Ending it must remove it from the listing.
	c.commit(t)
	waitForTransactions(t, srv, 0)
}

// TestTxIntrospection_TerminateRollsBackAtomically pins acceptance criterion (2):
// a terminated transaction leaves NO partial state, exactly as a client ROLLBACK
// would.
func TestTxIntrospection_TerminateRollsBackAtomically(t *testing.T) {
	t.Parallel()
	srv, addr := startTestServerHandle(t, server.Options{
		ConnTimeout:      30 * time.Second,
		DefaultTxTimeout: 20 * time.Second,
		MaxTxIdleTime:    20 * time.Second,
	})

	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)
	c.begin(t)
	// Several writes, so a partial rollback would be observable as a subset.
	for i := 0; i < 3; i++ {
		c.run(t, "CREATE (:Terminated {v: 1})", nil)
		c.pullAll(t)
	}

	infos := waitForTransactions(t, srv, 1)
	if err := srv.TerminateTransaction(infos[0].ID); err != nil {
		t.Fatalf("TerminateTransaction: %v", err)
	}
	waitForTransactions(t, srv, 0)

	// A fresh connection must see none of the three nodes.
	v := newBoltTestClient(t, addr)
	defer v.close(t)
	v.negotiate(t)
	v.hello(t)
	v.run(t, "MATCH (n:Terminated) RETURN count(n) AS c", nil)
	records, _ := v.pullAll(t)
	if len(records) != 1 || len(records[0]) != 1 {
		t.Fatalf("unexpected count shape: %+v", records)
	}
	if n, ok := records[0][0].(int64); !ok || n != 0 {
		t.Fatalf("count(:Terminated) = %v, want 0 — the termination left partial state", records[0][0])
	}
}

// TestTxIntrospection_TerminateUnknownID pins the error contract: a stale id from
// an earlier listing must be reported, not silently ignored and not applied to
// whatever transaction came next.
func TestTxIntrospection_TerminateUnknownID(t *testing.T) {
	t.Parallel()
	srv, _ := startTestServerHandle(t, server.Options{ConnTimeout: 30 * time.Second})
	err := srv.TerminateTransaction("no-such-transaction-1")
	if !errors.Is(err, server.ErrNoSuchTransaction) {
		t.Fatalf("TerminateTransaction = %v, want it to wrap ErrNoSuchTransaction", err)
	}
}

// TestTxIntrospection_TerminationEndsAnAbandonedTransaction is what this test became
// once there was no stall left to resolve.
//
// # Its fixture has now stopped reproducing an outage TWICE, and both times that was
// the point
//
// It began as a READER stall: an idle open transaction held the visibility barrier and
// every reader waited behind it. MVCC P4c (rmp #2274, #2290) retired the read barrier,
// so the fixture stopped reproducing anything and the test was re-aimed at the stall
// that remained — writes took the barrier exclusively, so a second WRITER waited.
//
// rmp #2305 retired that hold too. An abandoned transaction now blocks neither readers
// nor writers, so there is no stall for termination to resolve, and asserting one would
// assert the defect.
//
// # What termination is FOR now, and what this asserts
//
// An abandoned transaction still costs something: it pins the reclamation horizon, so
// no version it could still read is freed while it lives, and it occupies one of the
// per-principal transaction slots. Termination is the operator's remedy for THAT, and
// it must still work on demand rather than only via a timeout — which is why both
// automatic bounds are set to five minutes, far beyond the test's own patience.
//
// So the assertions are: an idle offender blocks NEITHER a reader NOR a writer (the
// rmp #2274 and rmp #2305 properties, asserted where a regression in either would be
// caught), and terminating it removes it from the registry promptly by operator action
// alone.
func TestTxIntrospection_TerminationEndsAnAbandonedTransaction(t *testing.T) {
	t.Parallel()
	srv, addr := startTestServerHandle(t, server.Options{
		ConnTimeout:      60 * time.Second,
		DefaultTxTimeout: 5 * time.Minute,
		MaxTxIdleTime:    5 * time.Minute,
	})

	// The offender: BEGIN, one write, then silence.
	ab := newBoltTestClient(t, addr)
	defer ab.close(t)
	ab.negotiate(t)
	ab.helloAs(t, "offender")
	ab.begin(t)
	ab.run(t, "CREATE (:Stall {v: 1})", nil)
	ab.pullAll(t)

	infos := waitForTransactions(t, srv, 1)
	if infos[0].Principal != "offender" {
		t.Fatalf("the listing names %q, want offender", infos[0].Principal)
	}

	// A READER must be served straight away. The rmp #2274 property, asserted where it
	// would be missed if it regressed.
	freeReader := newBoltTestClient(t, addr)
	defer freeReader.close(t)
	freeReader.negotiate(t)
	freeReader.hello(t)
	readerDone := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		freeReader.run(t, "MATCH (n) RETURN count(n) AS c", nil)
		freeReader.pullAll(t)
		readerDone <- time.Since(start)
	}()
	select {
	case d := <-readerDone:
		t.Logf("a reader concurrent with the offender was served in %v", d)
	case <-time.After(10 * time.Second):
		t.Fatal("a reader was NOT served while an idle write transaction was open: " +
			"the read barrier is back and rmp #2274 has regressed")
	}

	// A WRITER must be served straight away too. The rmp #2305 property, and the one
	// this test used to assert the opposite of.
	writer := newBoltTestClient(t, addr)
	defer writer.close(t)
	writer.negotiate(t)
	writer.hello(t)
	writerDone := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		writer.begin(t)
		writer.run(t, "CREATE (:Victim {v: 2})", nil)
		writer.pullAll(t)
		writer.commit(t)
		writerDone <- time.Since(start)
	}()
	select {
	case d := <-writerDone:
		t.Logf("a writer concurrent with the offender committed in %v", d)
	case <-time.After(10 * time.Second):
		t.Fatal("a writer was NOT served while an idle write transaction was open. The " +
			"offender is holding a transaction-lifetime lock across client think-time, " +
			"which rmp #2305 retired — over Bolt that is one paused client blocking every " +
			"other writer in the process.")
	}

	// Termination remains the operator's remedy for the resource the offender DOES
	// still hold: its reclamation-horizon slot and its per-principal transaction slot.
	// Both automatic bounds are 5m, so if it leaves the registry it is because the
	// termination did it.
	if err := srv.TerminateTransaction(infos[0].ID); err != nil {
		t.Fatalf("TerminateTransaction: %v", err)
	}
	waitForTransactions(t, srv, 0)
}

// TestTxIntrospection_ReadAndWriteTransactionsBothListed guards against the
// registry only seeing writers: a read transaction blocks nobody, but an operator
// still needs to see it, and the per-principal cap counts it.
func TestTxIntrospection_ReadAndWriteTransactionsBothListed(t *testing.T) {
	t.Parallel()
	srv, addr := startTestServerHandle(t, server.Options{
		ConnTimeout:      30 * time.Second,
		DefaultTxTimeout: 20 * time.Second,
		MaxTxIdleTime:    20 * time.Second,
	})

	w := newBoltTestClient(t, addr)
	defer w.close(t)
	w.negotiate(t)
	w.helloAs(t, "writer")
	w.begin(t)

	r := newBoltTestClient(t, addr)
	defer r.close(t)
	r.negotiate(t)
	r.helloAs(t, "reader")
	r.beginRead(t)

	infos := waitForTransactions(t, srv, 2)
	modes := map[string]string{}
	for _, in := range infos {
		modes[in.Principal] = in.Mode
	}
	if modes["writer"] != "w" {
		t.Errorf("writer's mode = %q, want w", modes["writer"])
	}
	if modes["reader"] != "r" {
		t.Errorf("reader's mode = %q, want r", modes["reader"])
	}
	// Oldest first, so the writer (opened first) leads.
	if infos[0].Principal != "writer" {
		t.Errorf("listing is not oldest-first: leader is %q, want writer", infos[0].Principal)
	}
}
