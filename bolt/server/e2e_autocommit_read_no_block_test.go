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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestE2E_ConcurrentAutocommitReadsRunInParallel verifies that N concurrent
// autocommit read sessions execute inside the engine AT THE SAME TIME rather than
// serialising on a lock (task #1432 regression gate).
//
// # The oracle is structural, not a timing ratio (rmp #2828)
//
// Each read evaluates a function that parks at a rendezvous inside the engine's
// execution of the query, and the rendezvous opens only once all N reads are
// parked there at once. It records the largest number of reads it ever held
// simultaneously, and the test asserts that number is N. A server that serialises
// autocommit reads — the #1432 regression, where every read held one lock for its
// whole execution — can never have more than one read inside execution, so the
// peak is 1 and the test fails once the rendezvous bound expires.
//
// This replaces a ratio of two wall-clock windows measured seconds apart (N
// concurrent reads against a serial baseline, limit 0.75·N). That ratio measured
// the cores available at each instant as much as the server: at HEAD d506d25d, on
// a 10-core host, it failed 0/32 at loadavg 1.86, 1/32 at loadavg 10.22-12.44,
// 1/32 at 23.94-25.39 and 3/32 at 40.07-42.79, the failing runs reporting
// 6.1x-10.6x, while the rendezvous peaked at N on every run under the same load.
// The failures were the host, not the engine. Observing overlap CLIENT-SIDE could
// not have replaced it either — a read blocked on a lock is in flight exactly as
// much as one executing — which is why the rendezvous sits inside execution,
// where a blocked read never arrives.
//
// Host load can only delay arrivals; it cannot stop N concurrently admitted reads
// from all reaching the rendezvous, so the verdict does not depend on it. The
// bound matters only on the failing path.
func TestE2E_ConcurrentAutocommitReadsRunInParallel(t *testing.T) {
	const (
		concurrency = 8
		// rendezvousBound caps how long a parked read waits for the others. A
		// parallel server releases the rendezvous as soon as the last read arrives,
		// so the bound is spent only when reads are serialised.
		rendezvousBound = 20 * time.Second
	)

	ctx := context.Background()
	rv := newReadRendezvous(concurrency)
	reg := rendezvousRegistry{inner: funcs.DefaultRegistry, rv: rv}
	eng := cypher.NewEngineWithRegistry(lpg.New[string, float64](adjlist.Config{}), reg)
	addr := startTestServerWithEngine(t, eng, server.Options{ConnTimeout: 15 * time.Second})

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

	runRead := func(query string) error {
		sess := drv.NewSession(ctx, neo4j.SessionConfig{})
		defer func() { _ = sess.Close(ctx) }()
		// A pure read (no graph mutation): it routes through the autocommit read
		// path, exactly the #1432 path under test.
		result, err := sess.Run(ctx, query, nil)
		if err != nil {
			return err
		}
		_, err = result.Consume(ctx)
		return err
	}

	// Prime the driver single-threaded first. One read initialises the neo4j
	// driver's shared connector state (it lazily assigns Connector.SupplyConnection
	// on the first Connect, unsynchronised in v5.28.4) and opens one pooled
	// connection. Without this prime, the concurrent phase would have many
	// goroutines hit that cold-start lazy-init simultaneously, and the race
	// detector would (correctly) flag the driver's own unsynchronised write. The
	// prime does not touch the rendezvous.
	if err := runRead("RETURN 1 AS n"); err != nil {
		t.Fatalf("priming read: %v", err)
	}

	// Concurrent phase: every read parks at the rendezvous inside execution.
	t.Cleanup(rv.arm(rendezvousBound))
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = runRead("UNWIND ['" + rendezvousArg + "'] AS s RETURN toString(s) AS v")
		}(i)
	}
	wg.Wait()

	arrived, peak := rv.observed()
	t.Logf("%d reads reached the rendezvous; at most %d were inside execution at once (want %d)",
		arrived, peak, concurrency)
	if arrived == 0 {
		t.Fatalf("no read reached the rendezvous, so the oracle observed no execution and proves "+
			"nothing (errors: %v)", errs)
	}
	// A serialised server lets one read into execution at a time, so the others
	// never arrive while it is parked: arrived < N is the same verdict as peak < N.
	if peak != concurrency {
		t.Fatalf("at most %d of %d concurrent autocommit reads were inside execution at once within %v "+
			"(%d arrived): reads are serialised (errors: %v)", peak, concurrency, rendezvousBound, arrived, errs)
	}
	for i, e := range errs {
		if e != nil {
			t.Errorf("session %d error: %v", i, e)
		}
	}
}

// rendezvousArg is the argument that makes [rendezvousRegistry]'s toString park
// at the rendezvous. Any other argument takes the built-in path unchanged.
const rendezvousArg = "gograph-read-rendezvous"

// readRendezvous holds up to want callers inside [readRendezvous.wait] until all
// of them are there at once, and records the most it ever held simultaneously.
//
// Safe for concurrent use.
type readRendezvous struct {
	want    int
	mu      sync.Mutex
	arrived int
	inside  int
	peak    int
	all     chan struct{}
	expired chan struct{}
}

func newReadRendezvous(want int) *readRendezvous {
	return &readRendezvous{want: want, all: make(chan struct{}), expired: make(chan struct{})}
}

// arm starts the bound after which parked callers give up, and returns the
// function that stops it.
func (r *readRendezvous) arm(bound time.Duration) (stop func()) {
	tm := time.AfterFunc(bound, func() { close(r.expired) })
	return func() { tm.Stop() }
}

// wait parks the caller until want callers are parked together, or until the
// bound set by arm expires, in which case it returns an error.
func (r *readRendezvous) wait() error {
	r.mu.Lock()
	r.arrived++
	r.inside++
	r.peak = max(r.peak, r.inside)
	if r.arrived == r.want {
		close(r.all)
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inside--
		r.mu.Unlock()
	}()
	select {
	case <-r.all:
		return nil
	case <-r.expired:
		return errors.New("read rendezvous: bound expired before every read arrived")
	}
}

// observed returns how many callers arrived and the most held at once.
func (r *readRendezvous) observed() (arrived, peak int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.arrived, r.peak
}

// rendezvousRegistry is the built-in function registry with toString wrapped so
// that a call on [rendezvousArg] parks at rv first. It overrides a built-in
// rather than adding a name because the semantic check accepts only built-in
// function names. Safe for concurrent use, as inner and rv are.
type rendezvousRegistry struct {
	inner expr.FunctionRegistry
	rv    *readRendezvous
}

// Resolve implements [expr.FunctionRegistry].
func (r rendezvousRegistry) Resolve(name string) (expr.BuiltinFn, bool) {
	fn, ok := r.inner.Resolve(name)
	if !ok || name != "tostring" {
		return fn, ok
	}
	return func(args []expr.Value) (expr.Value, error) {
		if len(args) == 1 {
			if s, isStr := args[0].(expr.StringValue); isStr && string(s) == rendezvousArg {
				if err := r.rv.wait(); err != nil {
					return nil, err
				}
			}
		}
		return fn(args)
	}, true
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
