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

// TestTxIntrospection_TerminateResolvesTheReaderStall is acceptance criterion (3)
// and the point of the whole task: reproduce the audit's outage and END it by
// operator action, without waiting for any timeout.
//
// Both automatic bounds are set far beyond the test's own patience, so if the
// reader is served it is because the termination did it and nothing else could
// have.
func TestTxIntrospection_TerminateResolvesTheReaderStall(t *testing.T) {
	t.Parallel()
	srv, addr := startTestServerHandle(t, server.Options{
		ConnTimeout:      60 * time.Second,
		DefaultTxTimeout: 5 * time.Minute,
		MaxTxIdleTime:    5 * time.Minute,
	})

	// The offender: BEGIN, one write, then silence. It holds the visibility
	// barrier, so every reader waits behind it.
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

	// The victim reads on another connection, on its own goroutine because it is
	// expected to block until the termination lands.
	victim := newBoltTestClient(t, addr)
	defer victim.close(t)
	victim.negotiate(t)
	victim.hello(t)

	served := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		victim.run(t, "MATCH (n) RETURN count(n) AS c", nil)
		victim.pullAll(t)
		served <- time.Since(start)
	}()

	// Give the reader time to be genuinely blocked, then intervene.
	time.Sleep(200 * time.Millisecond)
	select {
	case d := <-served:
		t.Fatalf("the reader completed in %v without intervention; the fixture is not "+
			"reproducing the stall", d)
	default:
	}

	if err := srv.TerminateTransaction(infos[0].ID); err != nil {
		t.Fatalf("TerminateTransaction: %v", err)
	}

	select {
	case d := <-served:
		t.Logf("reader served %v after the BEGIN, released by operator termination "+
			"(both automatic bounds were 5m)", d)
		if d > 30*time.Second {
			t.Fatalf("the reader took %v; the termination did not release the barrier promptly", d)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the reader was never served after termination; the barrier was not released")
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
