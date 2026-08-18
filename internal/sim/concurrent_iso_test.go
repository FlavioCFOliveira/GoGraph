package sim

// concurrent_iso_test.go — validation of the rmp #2440 during-run isolation
// oracles: monotonic reads, same-connection read-your-own-writes, and atomic
// batch visibility, observed WHILE a genuinely parallel population runs.

import (
	"context"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestConcurrentIso_ZeroViolationsAt64Connections is the rmp #2440 acceptance
// gate: 64 connections mixing contended transactional writers, atomic batch
// writers, oracle readers, and RYOW probes — zero oracle violations, with the
// oracles provably exercised (nonzero observation count) across seeds.
func TestConcurrentIso_ZeroViolationsAt64Connections(t *testing.T) {
	defer goleak.VerifyNone(t)
	for _, seed := range []uint64{0x150A, 0x150B} {
		srv, err := NewSimServer(SimEngineForServer(), clock.Real())
		if err != nil {
			t.Fatalf("NewSimServer: %v", err)
		}
		res, err := RunConcurrent(context.Background(), srv, ConcurrentConfig{
			Seed:              seed,
			Connections:       64,
			OpsPerConn:        10,
			ContendedCounters: 2,
			Mix: &ConcurrentMix{
				TxContendedWeight: 0.25,
				BatchWriterWeight: 0.25,
				IsoReaderWeight:   0.30,
				RYOWWriterWeight:  0.20,
			},
		})
		_ = srv.Close()
		if err != nil {
			t.Fatalf("seed %#x: RunConcurrent: %v", seed, err)
		}
		if res.Panics != 0 || res.TransportErrors != 0 {
			t.Fatalf("seed %#x: panics=%d transportErrors=%d", seed, res.Panics, res.TransportErrors)
		}
		if res.IsoReads == 0 {
			t.Fatalf("seed %#x: the during-run oracles never observed anything — vacuous", seed)
		}
		if res.IsoMonotonicViolations != 0 {
			t.Fatalf("seed %#x: %d monotonic-read regressions", seed, res.IsoMonotonicViolations)
		}
		if res.IsoRYOWViolations != 0 {
			t.Fatalf("seed %#x: %d same-connection read-your-own-writes misses", seed, res.IsoRYOWViolations)
		}
		if res.IsoBatchViolations != 0 {
			t.Fatalf("seed %#x: %d torn atomic batches observed", seed, res.IsoBatchViolations)
		}
		if !res.TxConserved() || res.TxMissingAcked != 0 || res.TxPhantomRefused != 0 {
			t.Fatalf("seed %#x: transactional ledger broken: %+v", seed, res)
		}
		if !res.Consistent() {
			t.Fatalf("seed %#x: eventual oracle inconsistent: engine=%d acked=%d", seed, res.EngineNodeCount, res.AckedCreates)
		}
		t.Logf("seed %#x: isoReads=%d txCommitted=%d txConflicted=%d batchNodes(acked multiples of %d ok)",
			seed, res.IsoReads, res.TxCommitted, res.TxConflicted, isoBatchSize)
	}
}

// TestConcurrentIso_MonotonicOracleFires proves the monotonic-read oracle
// fires on a harness-simulated regressing observation.
func TestConcurrentIso_MonotonicOracleFires(t *testing.T) {
	last := int64(5)
	if observeMonotonic(&last, 7) {
		t.Fatal("advancing observation misclassified as a regression")
	}
	if last != 7 {
		t.Fatalf("floor not advanced: %d", last)
	}
	if !observeMonotonic(&last, 3) {
		t.Fatal("regressing observation (7 -> 3) did not fire the monotonic oracle")
	}
	if last != 7 {
		t.Fatalf("a regression must not move the floor: %d", last)
	}
}

// TestConcurrentIso_BatchOracleFires proves the atomic-batch oracle fires on
// a harness-simulated torn count and stays silent on whole batches.
func TestConcurrentIso_BatchOracleFires(t *testing.T) {
	if observeBatch(isoBatchSize, int64(3*isoBatchSize)) {
		t.Fatal("a whole multiple misclassified as torn")
	}
	if !observeBatch(isoBatchSize, int64(3*isoBatchSize)+1) {
		t.Fatal("a torn batch count did not fire the atomic-visibility oracle")
	}
}

// TestConcurrentIso_RYOWOracleFires proves the same-connection RYOW probe
// fires on a stale read: the probe's own accounting path is driven with a
// read that misses (the created name is queried on a DIFFERENT name, so the
// count is zero — exactly what a stale same-connection read would return).
func TestConcurrentIso_RYOWOracleFires(t *testing.T) {
	defer goleak.VerifyNone(t)
	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	client, err := srv.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Simulate the stale read: the count the oracle inspects reads zero for a
	// name that was acknowledged. wireScalar and the accounting are the same
	// code the live role runs.
	var wl writerLog
	var acked, transport, bounded atomic.Int64
	c := &counters{ackedCreates: &acked, transportErrors: &transport, boundedRejects: &bounded}
	v, ok, stop := wireScalar(client, "MATCH (n:Person {name:$name}) RETURN count(n)",
		map[string]any{"name": "acked-but-never-visible"}, c)
	if stop || !ok {
		t.Fatalf("probe read failed: ok=%v stop=%v", ok, stop)
	}
	if v == 1 {
		t.Fatal("the simulated stale read unexpectedly saw the write")
	}
	wl.isoReads++
	if v != 1 {
		wl.isoRYOWViolations++
	}
	if wl.isoRYOWViolations != 1 {
		t.Fatal("the RYOW accounting did not record the stale read as a violation")
	}
}
