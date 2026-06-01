package cypher_test

// panic_boundary_test.go — tests for the engine's recover boundary (security
// fix H7). A recoverable panic raised while planning or executing a query must
// be converted into an error wrapping [cypher.ErrInternalPanic] rather than
// unwinding past the engine and crashing the embedding process.
//
// The write-path test is also the regression test for the ACID gap: a panic
// inside [Engine.RunInTx] (after the store's single-writer transaction is
// opened) must roll that transaction back so the single-writer mutex is
// released; otherwise every subsequent write would deadlock.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	cmetrics "gograph/internal/metrics"

	"gograph/cypher"
	"gograph/cypher/expr"
	"gograph/cypher/funcs"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/txn"
	"gograph/store/wal"
)

// quietLogs installs a discard slog default for the duration of the test and
// restores the previous default on cleanup. The panic boundary logs the full
// stack trace via slog.Default(); discarding it keeps test output readable
// while still exercising the logging path.
func quietLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// panicProbe is a metrics.Backend that records how many times each panic
// counter fired so the tests can assert the boundary incremented its metric.
//
// The global metrics backend is swapped atomically; tests that install this
// probe must NOT run in parallel (shared global state).
type panicProbe struct {
	runPanics      atomic.Uint64
	runInTxPanics  atomic.Uint64
	boltConnPanics atomic.Uint64
}

func (p *panicProbe) IncCounter(name string, delta uint64) {
	switch name {
	case "cypher.Run.panics":
		p.runPanics.Add(delta)
	case "cypher.RunInTx.panics":
		p.runInTxPanics.Add(delta)
	case "bolt.server.conn.panics":
		p.boltConnPanics.Add(delta)
	}
}

func (p *panicProbe) ObserveLatency(string, time.Duration) {}

// withPanicProbe installs a fresh panicProbe, runs fn, restores the default
// no-op backend, then returns the probe for inspection.
func withPanicProbe(t *testing.T, fn func()) *panicProbe {
	t.Helper()
	p := &panicProbe{}
	cmetrics.SetBackend(p)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })
	fn()
	return p
}

// init registers boom() on funcs.DefaultRegistry so that BOTH the semantic
// analyser (cypher/api.init wires sema.IsKnownFunction to DefaultRegistry) and
// the engine's runtime resolution accept the call. boom() always panics, which
// is the least-invasive seam for driving the engine's execution path into a
// recoverable panic from a query. The name is unique to these tests; the
// registry has no Unregister, but a single global registration is harmless and
// runs once before any test.
func init() {
	funcs.DefaultRegistry.Register("boom", func([]expr.Value) (expr.Value, error) {
		panic("boom: injected test panic")
	})
}

// TestEngineRun_RecoversExecutionPanic verifies that a panic raised while the
// read path executes a query is converted into an error wrapping
// [cypher.ErrInternalPanic] (never propagated as a process crash) and that the
// cypher.Run.panics counter is incremented.
func TestEngineRun_RecoversExecutionPanic(t *testing.T) {
	quietLogs(t)
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	probe := withPanicProbe(t, func() {
		res, err := eng.Run(context.Background(), "RETURN boom()", nil)
		if err == nil {
			// Drain so a leaked Result cannot trip goleak / WAL assertions.
			if res != nil {
				_ = res.Close()
			}
			t.Fatalf("Run: expected error, got nil")
		}
		if !errors.Is(err, cypher.ErrInternalPanic) {
			t.Fatalf("Run: error %v does not wrap ErrInternalPanic", err)
		}
		if res != nil {
			t.Fatalf("Run: expected nil Result on panic, got %v", res)
		}
	})

	if got := probe.runPanics.Load(); got != 1 {
		t.Fatalf("cypher.Run.panics = %d, want 1", got)
	}
}

// newBoomWALEngine builds a WAL-backed engine whose registry resolves boom()
// to a panicking builtin, plus the underlying store. The wal.Writer is closed
// via t.Cleanup.
func newBoomWALEngine(t *testing.T) (*cypher.Engine, *txn.Store[string, float64]) {
	t.Helper()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	// Default registry is used so boom() (registered in init) resolves both at
	// sema time and at exec time.
	return cypher.NewEngineWithStore(store), store
}

// TestEngineRunInTx_RecoversExecutionPanicAndReleasesWriter is the regression
// test for the H7 ACID gap. A write query that panics mid-execution must
//
//	(a) return an error wrapping cypher.ErrInternalPanic, and
//	(b) NOT leave the store's single-writer mutex held — otherwise the partial
//	    WAL transaction dangles and every future write deadlocks.
//
// We prove (b) by performing a SUBSEQUENT normal write transaction on the same
// engine and asserting it completes; if the mutex had leaked, RunInTx's Begin()
// would block forever, so a watchdog timeout fails the test instead of hanging.
func TestEngineRunInTx_RecoversExecutionPanicAndReleasesWriter(t *testing.T) {
	quietLogs(t)
	eng, _ := newBoomWALEngine(t)

	probe := withPanicProbe(t, func() {
		// boom() is evaluated during exec.Run, which RunInTx executes inside the
		// ApplyAtomically barrier AFTER the store's single-writer transaction is
		// opened — exactly the window in which the writer mutex could leak.
		res, err := eng.RunInTx(context.Background(), "CREATE (n:N) SET n.v = boom() RETURN n", nil)
		if err == nil {
			if res != nil {
				_ = res.Close()
			}
			t.Fatalf("RunInTx: expected error, got nil")
		}
		if !errors.Is(err, cypher.ErrInternalPanic) {
			t.Fatalf("RunInTx: error %v does not wrap ErrInternalPanic", err)
		}
		if res != nil {
			t.Fatalf("RunInTx: expected nil Result on panic, got %v", res)
		}
	})

	if got := probe.runInTxPanics.Load(); got != 1 {
		t.Fatalf("cypher.RunInTx.panics = %d, want 1", got)
	}

	// (b) The single-writer mutex must have been released by the panic-path
	// rollback. A subsequent ordinary write must succeed. Run it under a
	// watchdog so a leaked mutex fails the test deterministically instead of
	// deadlocking the whole package.
	done := make(chan error, 1)
	go func() {
		res, err := eng.RunInTx(context.Background(), "CREATE (m:M) RETURN m", nil)
		if err != nil {
			done <- err
			return
		}
		for res.Next() {
			_ = res.Record()
		}
		if err := res.Err(); err != nil {
			done <- err
			return
		}
		done <- res.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("subsequent write failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("subsequent write deadlocked: the single-writer mutex was not released on the panic path (ACID atomicity/liveness regression)")
	}
}
