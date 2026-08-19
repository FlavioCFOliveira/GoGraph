package server_test

// abandoned_tx_test.go — rmp #2175 (round-3 comparative audit, finding A1).
//
// THE REPRODUCTION, AS IT WAS IN 2026-07. One authenticated Bolt client sent
// BEGIN and then stopped talking. Because an open explicit write transaction then
// held the engine's global visibility barrier, every reader on every other
// connection stalled behind it: the audit measured a 4.7 ms read becoming
// 30.001 s — the full DefaultTxTimeout — followed by a hard TransactionTimedOut,
// repeatable indefinitely, since bolt/ had no per-principal or per-IP limit of
// any kind.
//
// The tense is load-bearing. That hold is GONE: rmp #2305/#2306 retired both the
// writer serialisation and the visibility barriers a transaction held across
// client think-time, so an abandoned transaction blocks neither readers nor
// writers today (cypher/exectx.go, "NO writer serialisation is acquired"). What
// it still costs is resources — a pinned reclamation horizon, a registry entry
// and a quota slot — which is what the bounds below now defend. The tests here
// still pin those bounds; they no longer reproduce an outage, and the fixture
// that once did is documented as having stopped reproducing one.
//
// Lowering the total transaction timeout cannot fix this: it converts the outage
// into a shorter one, and it kills legitimate long transactions along the way.
// The bound that distinguishes the two cases is an IDLE bound, because a working
// client sends messages and an abandoned one does not.
//
// The tests below are the PoC and its fix, in the same file so the claim and its
// evidence cannot drift apart.
//
// Layer: short. The timings are deliberately coarse — they assert an order of
// magnitude, not scheduler behaviour.

import (
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
)

// TestAbandonedTx_IdleReaperBoundsTheReaderStall is the acceptance test for the
// idle bound: a client that sends BEGIN and goes silent must not stall readers
// for the full transaction timeout.
//
// MaxTxIdleTime is set well below DefaultTxTimeout so the two bounds are
// distinguishable: if the reader unblocks near the idle bound the idle reaper did
// it, and if it unblocks near the total bound the old behaviour is still in
// place.
func TestAbandonedTx_IdleReaperBoundsTheReaderStall(t *testing.T) {
	t.Parallel()
	const (
		idleBound  = 300 * time.Millisecond
		totalBound = 20 * time.Second
	)
	addr := startTestServer(t, server.Options{
		ConnTimeout:      30 * time.Second,
		DefaultTxTimeout: totalBound,
		MaxTxIdleTime:    idleBound,
	})

	// The abandoning client: BEGIN, then silence. It never sends COMMIT,
	// ROLLBACK or RESET, and it deliberately keeps the socket open so the
	// connection teardown cannot be what releases the barrier.
	abandoner := newBoltTestClient(t, addr)
	defer abandoner.close(t)
	abandoner.negotiate(t)
	abandoner.hello(t)
	abandoner.begin(t)
	abandoner.run(t, "CREATE (:Abandoned {v: 1})", nil)
	abandoner.pullAll(t)
	// From here on the abandoner sends nothing at all.

	// The victim: a plain read on another connection. Before the fix this
	// completed only once the transaction hit its TOTAL timeout.
	victim := newBoltTestClient(t, addr)
	defer victim.close(t)
	victim.negotiate(t)
	victim.hello(t)

	start := time.Now()
	victim.run(t, "MATCH (n) RETURN count(n) AS c", nil)
	victim.pullAll(t)
	stalled := time.Since(start)

	if stalled >= totalBound {
		t.Fatalf("the reader stalled for %v — the full transaction timeout. The idle bound "+
			"did not fire, which is the defect A1 describes", stalled)
	}
	// Generous ceiling: the reader must unblock on the IDLE bound's timescale, an
	// order of magnitude below the total bound, not on the total bound's.
	if stalled > idleBound+5*time.Second {
		t.Fatalf("the reader stalled for %v, want the idle bound's timescale (%v)", stalled, idleBound)
	}
	t.Logf("reader unblocked after %v (idle bound %v, total bound %v)", stalled, idleBound, totalBound)
}

// TestAbandonedTx_BusyTransactionIsNotReaped is the other half, and the reason an
// idle bound is the right instrument: a transaction that keeps talking must NOT
// be killed by the idle bound, however long it lives. Without this the fix would
// simply be a shorter total timeout wearing a different name.
func TestAbandonedTx_BusyTransactionIsNotReaped(t *testing.T) {
	t.Parallel()
	const idleBound = 300 * time.Millisecond
	addr := startTestServer(t, server.Options{
		ConnTimeout:      30 * time.Second,
		DefaultTxTimeout: 20 * time.Second,
		MaxTxIdleTime:    idleBound,
	})

	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)
	c.begin(t)

	// Live for several multiples of the idle bound, sending a message in each
	// interval. Every one must succeed.
	const rounds = 6
	for i := 0; i < rounds; i++ {
		time.Sleep(idleBound / 2)
		c.run(t, "CREATE (:Busy {v: 1})", nil)
		c.pullAll(t)
	}
	total := time.Duration(rounds) * (idleBound / 2)
	if total <= idleBound {
		t.Fatalf("test misconfigured: the transaction lived %v, not longer than the %v idle bound",
			total, idleBound)
	}
	// The transaction must still be alive and committable.
	c.commit(t)
}

// TestAbandonedTx_PerPrincipalCapRejectsWithTypedError pins acceptance criterion
// (2). The cap counts OPEN transactions of BOTH kinds: rmp #2306 retired the
// writer serialisation BEGIN used to take (Engine.beginTxSession acquires none;
// concurrency control is MVCC with per-object conflict detection), so write
// transactions are no longer capped at one server-wide and are as concurrent as
// read ones. Read transactions are used here only because they are the simplest
// thing to leave open; internal/sim's bolt-tx-quota arm fills the cap with one of
// each and asserts that both count against it (rmp #2482).
func TestAbandonedTx_PerPrincipalCapRejectsWithTypedError(t *testing.T) {
	t.Parallel()
	const cap = 2
	addr := startTestServer(t, server.Options{
		ConnTimeout:           30 * time.Second,
		MaxTxIdleTime:         30 * time.Second, // long: the cap is what must fire
		MaxOpenTxPerPrincipal: cap,
	})

	// Open `cap` READ transactions, each on its own connection. Read transactions
	// acquire no serialisation, so all of them are genuinely open at once.
	clients := make([]*boltTestClient, 0, cap+1)
	for i := 0; i < cap; i++ {
		c := newBoltTestClient(t, addr)
		t.Cleanup(func() { c.close(t) })
		c.negotiate(t)
		c.hello(t)
		c.beginRead(t)
		clients = append(clients, c)
	}

	// The next one must be refused, with a typed error rather than a hang.
	over := newBoltTestClient(t, addr)
	t.Cleanup(func() { over.close(t) })
	over.negotiate(t)
	over.hello(t)
	failure := over.beginReadExpectFailure(t)
	if failure.Code != "Neo.ClientError.General.LimitExceeded" {
		t.Fatalf("BEGIN over the cap returned code %q, want Neo.ClientError.General.LimitExceeded",
			failure.Code)
	}
	if failure.Message == "" {
		t.Fatal("the refusal carried no message")
	}
	t.Logf("refused with: %s", failure.Message)

	// Releasing one slot must let a new transaction in, so the cap bounds
	// concurrency rather than acting as a lifetime quota.
	clients[0].rollback(t)
	after := newBoltTestClient(t, addr)
	t.Cleanup(func() { after.close(t) })
	after.negotiate(t)
	after.hello(t)
	after.beginRead(t) // must succeed now
}

// TestAbandonedTx_CapIsPerPrincipalNotPerServer proves the cap does not
// accidentally become a global limit: a second principal is unaffected by the
// first one's slots.
func TestAbandonedTx_CapIsPerPrincipalNotPerServer(t *testing.T) {
	t.Parallel()
	const cap = 1
	addr := startTestServer(t, server.Options{
		ConnTimeout:           30 * time.Second,
		MaxTxIdleTime:         30 * time.Second,
		MaxOpenTxPerPrincipal: cap,
		// NoAuthHandler yields Identity{Principal: whatever the client sent}, so
		// the two clients below are genuinely different principals.
		Auth: server.NoAuthHandler{},
	})

	alice := newBoltTestClient(t, addr)
	t.Cleanup(func() { alice.close(t) })
	alice.negotiate(t)
	alice.helloAs(t, "alice")
	alice.beginRead(t)

	bob := newBoltTestClient(t, addr)
	t.Cleanup(func() { bob.close(t) })
	bob.negotiate(t)
	bob.helloAs(t, "bob")
	bob.beginRead(t) // must succeed: bob holds none of his own

	// Alice, however, is at her cap.
	alice2 := newBoltTestClient(t, addr)
	t.Cleanup(func() { alice2.close(t) })
	alice2.negotiate(t)
	alice2.helloAs(t, "alice")
	f := alice2.beginReadExpectFailure(t)
	if f.Code != "Neo.ClientError.General.LimitExceeded" {
		t.Fatalf("alice's second BEGIN returned %q, want Neo.ClientError.General.LimitExceeded", f.Code)
	}
}
