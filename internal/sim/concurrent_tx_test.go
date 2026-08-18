package sim

// concurrent_tx_test.go — validation of the rmp #2439 transactional roles:
// contended and disjoint explicit transactions over the real Bolt wire, typed
// conflict accounting, transaction-granular all-or-nothing at quiescence, and
// injection proofs that the quiescence verifier can fail.

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestConcurrentTx_ContendedConflictsAndNoLostUpdates is the rmp #2439
// acceptance gate: a contended transactional population over the real wire
// produces a NONZERO typed conflict count (something WAS seen), zero lost
// updates (every shared counter equals its acknowledged increments), committed
// effects all visible, refused transactions traceless, totals conserved, and
// the eventual oracle intact.
func TestConcurrentTx_ContendedConflictsAndNoLostUpdates(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
		Seed:              0xC0117E57,
		Connections:       24,
		OpsPerConn:        15,
		ContendedCounters: 2,
		Mix:               &ConcurrentMix{TxContendedWeight: 0.7, TxWriterWeight: 0.2, ReaderWeight: 0.1},
	})
	if err != nil {
		t.Fatalf("RunConcurrent: %v", err)
	}

	if res.Panics != 0 || res.TransportErrors != 0 {
		t.Fatalf("panics=%d transportErrors=%d (want 0,0)", res.Panics, res.TransportErrors)
	}
	if res.TxIssued == 0 || res.TxCommitted == 0 {
		t.Fatalf("vacuous transactional run: %+v", res)
	}
	if res.TxConflicted == 0 {
		t.Fatal("no typed serialization conflict observed under deliberate contention — the accounting was never exercised")
	}
	if !res.TxConserved() {
		t.Fatalf("transaction totals not conserved: issued=%d committed=%d conflicted=%d rolledBack=%d failed=%d ambiguous=%d",
			res.TxIssued, res.TxCommitted, res.TxConflicted, res.TxRolledBack, res.TxFailed, res.TxAmbiguous)
	}
	if res.TxMissingAcked != 0 {
		t.Fatalf("%d acknowledged transaction markers missing at quiescence (lost committed transactions)", res.TxMissingAcked)
	}
	if res.TxPhantomRefused != 0 {
		t.Fatalf("%d refused transaction markers present at quiescence (refused transactions left traces)", res.TxPhantomRefused)
	}
	for k := range res.ContendedAcked {
		if res.ContendedFinal[k] != res.ContendedAcked[k] {
			t.Fatalf("counter %d: final=%d acked=%d — lost update or phantom apply", k, res.ContendedFinal[k], res.ContendedAcked[k])
		}
	}
	if !res.Consistent() {
		t.Fatalf("eventual oracle inconsistent: engine=%d acked=%d", res.EngineNodeCount, res.AckedCreates)
	}
	t.Logf("txIssued=%d committed=%d conflicted=%d rolledBack=%d failed=%d ambiguous=%d counters(final=acked)=%v",
		res.TxIssued, res.TxCommitted, res.TxConflicted, res.TxRolledBack, res.TxFailed, res.TxAmbiguous, res.ContendedFinal)
}

// TestConcurrentTx_DisjointCommitsAndRollbacks exercises the disjoint
// transactional role alongside plain roles: commits and deliberate rollbacks
// both occur, the ledger holds at quiescence, and the conflict count is
// asserted ZERO — disjoint keys share nothing, so any conflict here would be
// spurious contention the engine invented.
func TestConcurrentTx_DisjointCommitsAndRollbacks(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
		Seed:        0xD1570177, // distinct fixed seed
		Connections: 16,
		OpsPerConn:  20,
		Mix:         &ConcurrentMix{TxWriterWeight: 0.8, ReaderWeight: 0.2},
	})
	if err != nil {
		t.Fatalf("RunConcurrent: %v", err)
	}

	if res.Panics != 0 || res.TransportErrors != 0 {
		t.Fatalf("panics=%d transportErrors=%d (want 0,0)", res.Panics, res.TransportErrors)
	}
	if res.TxCommitted == 0 || res.TxRolledBack == 0 {
		t.Fatalf("commit and rollback paths not both exercised: %+v", res)
	}
	if res.TxConflicted != 0 {
		t.Fatalf("disjoint transactional writers conflicted %d times — keys never collide, so every conflict is spurious", res.TxConflicted)
	}
	if !res.TxConserved() || res.TxMissingAcked != 0 || res.TxPhantomRefused != 0 {
		t.Fatalf("ledger broken: %+v", res)
	}
	if !res.Consistent() {
		t.Fatalf("eventual oracle inconsistent: engine=%d acked=%d", res.EngineNodeCount, res.AckedCreates)
	}
}

// TestConcurrentTx_QuiescenceVerifierFires proves the ledger verification can
// fail: a fabricated acknowledged marker that was never created reports as
// missing, and a fabricated refused marker that DOES exist reports as a
// phantom.
func TestConcurrentTx_QuiescenceVerifierFires(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	// A real node that the fabricated "refused" ledger claims was refused.
	if _, err := seedContendedCounter(srv, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	missing, phantom, _, err := verifyTxQuiescence(srv,
		[]string{"never-created-marker"},
		[]string{wireCounterName(0)},
		0)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if missing != 1 {
		t.Fatalf("missing=%d, want 1 — the verifier did not detect a lost acknowledged transaction", missing)
	}
	if phantom != 1 {
		t.Fatalf("phantom=%d, want 1 — the verifier did not detect a refused transaction's trace", phantom)
	}
}
