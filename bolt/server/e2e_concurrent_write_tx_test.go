package server_test

// e2e_concurrent_write_tx_test.go — rmp #2305 AC2 and AC4.
//
// # What this gates
//
// An explicit write transaction must not hold a module-wide lock across client
// think-time. Over Bolt that is not an abstract property: a client that opens a
// write transaction and then pauses — a network round-trip, a slow application,
// a user staring at a form — used to block EVERY other writer in the process for
// as long as the transaction stayed open, because BEGIN acquired the graph's
// visibility barrier EXCLUSIVELY and held it to COMMIT.
//
// The audit called this the most consequential single fact in it, and the reason
// is that no MVCC engine behaves this way: an open transaction holds VERSIONS,
// not the engine.
//
// These tests drive the OFFICIAL neo4j-go-driver against a real server over a
// real socket, so they exercise the whole path a client does — BEGIN, RUN, PULL,
// COMMIT across separate messages — rather than an in-process shortcut that
// cannot express think-time.
//
// # Why they are written as rendezvous, not as timing
//
// Both tests establish overlap by making each transaction WAIT for the other to
// be inside its own transaction before either commits. Under a
// transaction-lifetime exclusive lock the second BEGIN cannot complete, so the
// rendezvous cannot be satisfied and the test fails on its own deadline. A
// reintroduced serialiser therefore makes these RED, never merely slow — which is
// what makes them gates rather than benchmarks.
//
// Layer: short.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
)

// newConcurrentTxDriver starts a server and connects the official driver with a
// pool large enough for the concurrent sessions these tests open. The default
// harness caps the pool at 5, which would itself serialise the clients and mask
// what is being measured.
//
// MaxTxIdleTime is set far above these tests' budgets DELIBERATELY. Its 5 s
// default exists precisely because an open transaction used to hold the global
// visibility barrier, so the reaper's job was to cut short the outage that caused
// (see [server.DefaultMaxTxIdleTime] and rmp #2175). Left at the default it also
// RESCUES these tests: the idle transaction is killed, the barrier is released, and
// the blocked writer proceeds — so the tests would pass against the very build
// they exist to fail. Raising it removes the rescue and leaves only the property
// under test.
// # One driver per client, and why it is not cosmetic
//
// neo4j-go-driver v5.28.4 has a DATA RACE of its own in Connector.Connect: it lazily
// assigns `c.SupplyConnection = c.createConnection` on a shared Connector with no
// synchronisation (neo4j/internal/connector/connector.go:55). Two goroutines opening
// their FIRST connection concurrently from one driver both write it, and `-race`
// reports it. These are the first tests in this suite to do that on a cold pool,
// which is why it surfaces here.
//
// It is third-party and not GoGraph's to fix. Giving each client its own driver
// avoids the shared Connector entirely and models two INDEPENDENT clients, which is
// what the tests are about, so the workaround costs nothing in fidelity. Do not
// "fix" this by serialising the clients — that would defeat the tests.
func newConcurrentTxServer(t *testing.T, sessions int) string {
	t.Helper()
	return startTestServer(t, server.Options{
		ConnTimeout:      30 * time.Second,
		MaxConnections:   (sessions + 4) * 2,
		MaxTxIdleTime:    10 * time.Minute,
		DefaultTxTimeout: 10 * time.Minute,
	})
}

// newConcurrentTxDriver connects one driver to addr.
func newConcurrentTxDriver(t *testing.T, addr string, sessions int) neo4j.DriverWithContext {
	t.Helper()
	driver, err := neo4j.NewDriverWithContext(
		"bolt://"+addr,
		neo4j.NoAuth(),
		func(c *config.Config) {
			c.MaxConnectionPoolSize = sessions + 4
			c.ConnectionAcquisitionTimeout = 10 * time.Second
			c.SocketConnectTimeout = 10 * time.Second
		},
	)
	if err != nil {
		t.Fatalf("neo4j.NewDriverWithContext: %v", err)
	}
	t.Cleanup(func() {
		if cerr := driver.Close(context.Background()); cerr != nil {
			t.Logf("driver.Close: %v", cerr)
		}
	})
	return driver
}

// TestE2E_TwoExplicitWriteTransactionsOverlap is AC2: two clients hold open
// explicit write transactions SIMULTANEOUSLY and both make progress on disjoint
// data.
//
// Each client begins a transaction, writes its own node, announces that it is
// inside, and only then waits for its peer. Both must reach the announcement
// before either commits — which is impossible if BEGIN serialises.
func TestE2E_TwoExplicitWriteTransactionsOverlap(t *testing.T) {
	t.Parallel()

	const clients = 2
	const settle = 20 * time.Second
	addr := newConcurrentTxServer(t, clients)
	ctx := context.Background()

	var (
		inside   = make(chan struct{}, clients)
		bothIn   = make(chan struct{})
		closeAll sync.Once
		wg       sync.WaitGroup
		errs     [clients]error
	)

	wg.Add(clients)
	for c := range clients {
		go func(c int) {
			defer wg.Done()
			// Its own driver: see [newConcurrentTxServer] for the driver-side race
			// that sharing one would trip.
			drv := newConcurrentTxDriver(t, addr, 1)
			sess := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
			defer func() { _ = sess.Close(ctx) }()

			tx, err := sess.BeginTransaction(ctx)
			if err != nil {
				errs[c] = fmt.Errorf("BeginTransaction: %w", err)
				return
			}
			// Write inside the transaction, on this client's OWN key, so nothing
			// the two do can legitimately conflict.
			if _, err = tx.Run(ctx, "CREATE (:Acct {owner:$o})", map[string]any{"o": c}); err != nil {
				errs[c] = fmt.Errorf("Run: %w", err)
				_ = tx.Rollback(ctx)
				return
			}

			// THE RENDEZVOUS. Both transactions are open and both have written.
			inside <- struct{}{}
			if len(inside) == clients {
				closeAll.Do(func() { close(bothIn) })
			}
			select {
			case <-bothIn:
			case <-time.After(settle):
				errs[c] = fmt.Errorf("timed out waiting for the peer transaction to open")
				_ = tx.Rollback(ctx)
				return
			}

			errs[c] = tx.Commit(ctx)
		}(c)
	}

	select {
	case <-bothIn:
	case <-time.After(settle):
		// Do not Wait here: the goroutines are parked on their own timeout and
		// reporting the serialisation is the whole result.
		t.Fatalf("two Bolt clients were never inside an explicit write transaction at "+
			"the same time within %s.\nBEGIN is serialising them: rmp #2305 requires that "+
			"an explicit write transaction acquire no module-wide exclusive lock, so a "+
			"second client's BEGIN must complete while the first transaction is still "+
			"open. This is the failure mode the task exists to remove — a paused client "+
			"blocking every other writer in the process.", settle)
	}
	wg.Wait()
	for c, err := range errs {
		if err != nil {
			t.Errorf("client %d: %v", c, err)
		}
	}

	// Both transactions committed, so both nodes must be present: overlapping did
	// not cost either write.
	verifier := newConcurrentTxDriver(t, addr, 1)
	sess := verifier.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer func() { _ = sess.Close(ctx) }()
	rec, err := sess.Run(ctx, "MATCH (a:Acct) RETURN count(a) AS n", nil)
	if err != nil {
		t.Fatalf("verification Run: %v", err)
	}
	single, err := rec.Single(ctx)
	if err != nil {
		t.Fatalf("verification Single: %v", err)
	}
	got, _ := single.Get("n")
	if n, ok := got.(int64); !ok || n != clients {
		t.Fatalf("after %d overlapping committed transactions the graph holds %v :Acct "+
			"nodes, want %d", clients, got, clients)
	}
}

// TestE2E_AnIdleExplicitTransactionDoesNotStallAnotherWriter is AC4, and it is the
// one that names the user-visible symptom: a transaction left OPEN AND IDLE — the
// client did its write and then went away — must not stop an unrelated writer from
// committing.
//
// The idle transaction is deliberately never committed until the other writer has
// finished, so under a transaction-lifetime lock the second writer waits forever
// and the test fails on its deadline rather than passing slowly.
func TestE2E_AnIdleExplicitTransactionDoesNotStallAnotherWriter(t *testing.T) {
	t.Parallel()

	const budget = 15 * time.Second
	addr := newConcurrentTxServer(t, 2)
	ctx := context.Background()

	// Client A opens a write transaction, writes, and then sits idle. Its own driver:
	// see [newConcurrentTxServer].
	idleDrv := newConcurrentTxDriver(t, addr, 1)
	idleSess := idleDrv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer func() { _ = idleSess.Close(ctx) }()
	idleTx, err := idleSess.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("idle BeginTransaction: %v", err)
	}
	if _, err = idleTx.Run(ctx, "CREATE (:Acct {owner:'idle'})", nil); err != nil {
		t.Fatalf("idle Run: %v", err)
	}
	// Rolled back at the end: the point is that it stays OPEN throughout.
	defer func() { _ = idleTx.Rollback(ctx) }()

	// Client B must commit an autocommit write while A sits open.
	done := make(chan error, 1)
	go func() {
		drv := newConcurrentTxDriver(t, addr, 1)
		sess := drv.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		defer func() { _ = sess.Close(ctx) }()
		_, rerr := sess.Run(ctx, "CREATE (:Acct {owner:'other'})", nil)
		if rerr != nil {
			done <- rerr
			return
		}
		// A managed write so the commit is genuinely acknowledged by the server.
		_, rerr = sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx, "CREATE (:Acct {owner:'other-managed'})", nil)
			return nil, err
		})
		done <- rerr
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the unrelated writer failed while a transaction sat idle: %v", err)
		}
	case <-time.After(budget):
		t.Fatalf("an unrelated writer did not commit within %s while an explicit write "+
			"transaction sat OPEN AND IDLE.\nThe idle transaction is holding a "+
			"module-wide lock across client think-time, which over Bolt means one paused "+
			"client blocks every other writer in the process (rmp #2305). An open "+
			"transaction must hold VERSIONS, not the engine.", budget)
	}
}
