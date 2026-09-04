package server_test

// e2e_autocommit_read_no_block_test.go — regression gate for task #1432.
//
// Before the fix, bolt autocommit handler called RunInTxAny for ALL queries,
// which routes through Engine.RunInTx → lockWriter() → writeMu.Lock(). This
// means concurrent autocommit READ queries all serialized on writeMu, even
// though reads need no write lock.
//
// After the fix, autocommit queries go through RunAny, which routes reads to
// Engine.Run took the visibility barrier in read mode. Read locks were shared, so N
// concurrent autocommit read sessions can now execute in parallel.
//
// Note: since rmp #2290 an autocommit read takes NO barrier at all — it pins an
// MVCC snapshot and resolves every store as of that instant, so it neither
// blocks behind, nor is blocked by, an open explicit write transaction, and it
// still never sees uncommitted writes.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// TestE2E_ConcurrentAutocommitReadsRunInParallel verifies that N concurrent
// autocommit read sessions all complete in parallel rather than serialising on
// a write lock (task #1432 regression gate).
//
// Before the fix, each autocommit read acquired writeMu exclusively, so
// N reads completed in Θ(N × T). After the fix they all acquire visMu.RLock
// concurrently and complete in Θ(T). We assert that 8 concurrent reads finish
// within 4 × the measured single-read latency, providing ≈2× margin without
// depending on a precise wall-clock constant.
func TestE2E_ConcurrentAutocommitReadsRunInParallel(t *testing.T) {
	const (
		concurrency = 8
		// A serialised handler (the #1432 regression) takes ~concurrency* the
		// single-read baseline; a parallel one is bounded by the host CPU count.
		// 0.75*concurrency still flags serialisation (~8x) while tolerating
		// CPU-bound execution on small CI runners (8 reads on 2 cores ~4x).
		maxFactor = 0.75 * concurrency
	)

	ctx := context.Background()
	addr := startTestServer(t, server.Options{ConnTimeout: 15 * time.Second})

	newDriver := func() neo4j.DriverWithContext {
		drv, err := neo4j.NewDriverWithContext(
			"bolt://"+addr,
			neo4j.NoAuth(),
			func(c *config.Config) {
				c.MaxConnectionPoolSize = concurrency + 2
				c.ConnectionAcquisitionTimeout = 5 * time.Second
				c.SocketConnectTimeout = 3 * time.Second
			},
		)
		if err != nil {
			t.Fatalf("NewDriverWithContext: %v", err)
		}
		t.Cleanup(func() { _ = drv.Close(context.Background()) })
		return drv
	}

	drv := newDriver()

	runRead := func() (time.Duration, error) {
		sess := drv.NewSession(ctx, neo4j.SessionConfig{})
		defer func() { _ = sess.Close(ctx) }()
		start := time.Now()
		// A read heavy enough (a few ms) that the serialisation signal dominates
		// fixed per-request overhead (goroutine scheduling, driver pool mutex,
		// session Run/Consume). A sub-millisecond "RETURN 1" makes the
		// concurrent/baseline ratio noise-dominated rather than a parallelism
		// measurement. This is a pure read (no graph mutation): it routes
		// through the shared read lock, exactly the #1432 path under test.
		result, err := sess.Run(ctx, "UNWIND range(1, 200000) AS x RETURN count(x) AS n", nil)
		if err != nil {
			return 0, err
		}
		if _, err = result.Consume(ctx); err != nil {
			return 0, err
		}
		return time.Since(start), nil
	}

	// Prime the driver single-threaded first. One read initialises the neo4j
	// driver's shared connector state (it lazily assigns Connector.SupplyConnection
	// on the first Connect, unsynchronised in v5.28.4) and opens one pooled
	// connection. Without this prime, the concurrent warm-up below would have
	// many goroutines hit that cold-start lazy-init simultaneously, and the race
	// detector would (correctly) flag the driver's own unsynchronised write.
	if _, err := runRead(); err != nil {
		t.Fatalf("priming read: %v", err)
	}

	// Warm the ENTIRE connection pool next. The baseline below warms only one
	// pooled connection, but the concurrent phase needs `concurrency` of them;
	// without this warm-up the concurrent phase pays to establish concurrency-1
	// fresh Bolt connections (TCP + handshake + HELLO), which dominates the
	// measurement and is unrelated to read parallelism.
	var warm sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		warm.Add(1)
		go func() { defer warm.Done(); _, _ = runRead() }()
	}
	warm.Wait()

	// Warm up: measure a single-read baseline (evicts cold-start latency).
	baseline, err := runRead()
	if err != nil {
		t.Fatalf("baseline read: %v", err)
	}
	t.Logf("baseline single-read latency: %v", baseline)

	// Concurrent phase: fire concurrency reads simultaneously.
	type res struct {
		dur time.Duration
		err error
	}
	results := make([]res, concurrency)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			d, e := runRead()
			results[idx] = res{dur: d, err: e}
		}(i)
	}
	wg.Wait()
	total := time.Since(start)

	for i, r := range results {
		if r.err != nil {
			t.Errorf("session %d error: %v", i, r.err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	limit := time.Duration(float64(baseline) * maxFactor)
	t.Logf("%d concurrent reads finished in %v (limit %v, baseline %v)", concurrency, total, limit, baseline)

	// The strict assertion below is a ratio of two wall-clock windows measured
	// SECONDS APART, so a load change between them reads as subject behaviour, and
	// the property under test — that the read did not serialise behind the writer
	// — depends on the parallelism actually available at that instant, which under
	// `make ci` is not the core count (rmp #2573).
	//
	// It is guarded rather than replaced because the three instruments rmp #2573
	// proposed are all unavailable, which was established by measurement rather
	// than assumed:
	//
	//   - counting writeMu acquisitions is impossible: Engine.writeMu was retired
	//     outright by rmp #2306 and no longer exists.
	//   - no counter the module exposes concerns read/write overlap or lock
	//     acquisition; the full set is MetricDeltaApplied, MetricExpandIntersect-
	//     Engaged, MetricLookup*, MetricRefresh*, MetricRecompute, MetricRelabel-
	//     Dirtied and the two MetricsSkippedEmptyRegistry pair.
	//   - observed overlap cannot be measured CLIENT-SIDE, where this test sits: a
	//     read BLOCKED on a lock is in flight exactly as much as one executing, so
	//     if the server serialised them all N would still be simultaneously in
	//     flight. The two regimes are indistinguishable from here.
	//
	// The soak-layer fallback the task offers would be pure coverage loss: soak is
	// explicitly not a release gate, and this gate measured 0 failures in 8 runs
	// under 300 CPU-bound processes on a 10-core host, ratios 1.05x-1.45x against
	// its 6.0x limit. RequireQuietMachine keeps it gating every push, in the serial
	// `make test-timing` phase where the two windows see the same machine.
	//
	// This REPLACES the testing.CoverMode() hatch that used to sit here. That hatch
	// existed for the same distortion by a different cause, and `make cover-gate`
	// now sets GOGRAPH_PARALLEL_SUITE too, so the guard subsumes it. Keeping both
	// would be two mechanisms for one precondition.
	testlayers.RequireQuietMachine(t, fmt.Sprintf(
		"the ratio of %d concurrent reads (%v) against a serial baseline (%v) measured seconds earlier, "+
			"limit %.1fx", concurrency, total, baseline, maxFactor))
	if total > limit {
		t.Errorf("concurrent reads took %v > %v (%.1f× baseline %v): reads appear to be serialised",
			total, limit, float64(total)/float64(baseline), baseline)
	}
}

// TestE2E_AutocommitReadDoesNotAcquireWriterLock verifies that a read-only
// autocommit query can proceed concurrently with autocommit WRITE queries: the
// read uses visMu.RLock (shared) while each write holds writeMu exclusively
// only for its own duration. After the write's brief visMu hold, the read can
// proceed.
//
// This is the core of the task #1432 fix: reads no longer go through
// RunInTxAny (which took writeMu), so they do not serialise behind writes that
// happen to hold writeMu.
func TestE2E_AutocommitReadDoesNotAcquireWriterLock(t *testing.T) {
	const readTimeout = 5 * time.Second

	ctx := context.Background()
	addr := startTestServer(t, server.Options{ConnTimeout: 30 * time.Second})

	newDriver := func() neo4j.DriverWithContext {
		drv, err := neo4j.NewDriverWithContext(
			"bolt://"+addr,
			neo4j.NoAuth(),
			func(c *config.Config) {
				c.MaxConnectionPoolSize = 5
				c.ConnectionAcquisitionTimeout = 5 * time.Second
				c.SocketConnectTimeout = 3 * time.Second
			},
		)
		if err != nil {
			t.Fatalf("NewDriverWithContext: %v", err)
		}
		t.Cleanup(func() { _ = drv.Close(context.Background()) })
		return drv
	}

	drvW := newDriver()
	drvR := newDriver()

	// Run a sequence of autocommit writes on the write driver (each acquires
	// writeMu briefly and releases it, then releases visMu).
	var writesDone int32
	const writeCount = 20
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writeCount; i++ {
			sessW := drvW.NewSession(ctx, neo4j.SessionConfig{})
			res, runErr := sessW.Run(ctx, "CREATE (n:RaceTest {i: $i}) RETURN n", map[string]any{"i": int64(i)})
			if runErr == nil {
				_, _ = res.Consume(ctx)
			}
			_ = sessW.Close(ctx)
		}
		writesDone = 1
	}()

	// Immediately run a read-only autocommit query on the read driver. It must
	// complete within readTimeout regardless of write activity.
	type readResult struct {
		elapsed time.Duration
		err     error
	}
	ch := make(chan readResult, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sessR := drvR.NewSession(ctx, neo4j.SessionConfig{})
		defer func() { _ = sessR.Close(ctx) }()

		rCtx, cancel := context.WithTimeout(ctx, readTimeout)
		defer cancel()

		start := time.Now()
		res, runErr := sessR.Run(rCtx, "RETURN 42 AS n", nil)
		if runErr != nil {
			ch <- readResult{err: runErr}
			return
		}
		if _, runErr = res.Consume(rCtx); runErr != nil {
			ch <- readResult{err: runErr}
			return
		}
		ch <- readResult{elapsed: time.Since(start)}
	}()

	wg.Wait()
	r := <-ch
	if r.err != nil {
		t.Fatalf("read-only autocommit failed: %v", r.err)
	}
	t.Logf("read-only autocommit completed in %v (writes done=%d)", r.elapsed, writesDone)
	if r.elapsed >= readTimeout {
		t.Fatalf("read-only autocommit took %v ≥ %v (appears blocked by concurrent writes)", r.elapsed, readTimeout)
	}
}
