package server_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestServeCtxCancel_TearsDownOwnedCloser is the regression gate for task
// #1351: stopping the server by cancelling the Serve context — a documented
// stop mechanism, equal in rank to Shutdown — must tear down the owned
// [server.Options.Closer] (the store.DB bundling the WAL writer and the
// background checkpointer) after the connection drain completes.
//
// Before the fix, Serve's exit path drained every connection and returned nil
// but never closed the owned closer (closeOwned ran only on Shutdown's
// drain-success branch), so an embedder using ctx-cancellation leaked the
// checkpoint goroutine and never closed the WAL through its crash-safe
// teardown. This test fails on that code: the post-cancel wal.Append succeeds
// instead of being rejected, and goleak reports the live checkpoint goroutine.
//
// Not parallel: goleak.IgnoreCurrent snapshots the goroutine set at call time,
// so a parallel sibling's in-flight goroutines would pollute the snapshot. The
// snapshot is taken BEFORE the checkpointer is started, so the checkpoint
// goroutine is in-scope for the leak check.
func TestServeCtxCancel_TearsDownOwnedCloser(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	// Full WAL-backed stack: WAL + typed store + engine + a running
	// checkpointer, bundled into a store.DB handed to the server as its Closer.
	dir := t.TempDir()
	wlog, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	st := txn.NewStoreWithOptions[string, float64](g, wlog, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)

	var unusedMu sync.Mutex
	cp := checkpoint.New(checkpoint.Config{
		Dir:      dir,
		MaxAge:   10 * time.Millisecond,
		Interval: 2 * time.Millisecond,
	}, g, wlog, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()))
	cpCtx, cpCancel := context.WithCancel(context.Background())
	defer cpCancel()
	cp.Start(cpCtx)

	db := store.New(wlog, store.WithCheckpointer(cp))

	srv, err := server.NewServer(eng, server.Options{
		ConnTimeout: 5 * time.Second,
		Auth:        server.NoAuthHandler{},
		Closer:      db,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx, ln) }()

	// Drive one real authenticated write so the engine/store/WAL path is live
	// and the checkpointer has committed frames to fold.
	c := newBoltTestClient(t, addr)
	c.negotiate(t)
	c.hello(t)
	c.run(t, `CREATE (a:Person {name: 'alice'})-[:KNOWS]->(b:Person {name: 'bob'})`, nil)
	c.pullAll(t)
	c.goodbye(t)
	c.close(t)

	// Stop the server via the documented ctx-cancellation mechanism. Serve
	// drains the (now-idle) connection set and must then close the owned
	// store.DB — stopping the checkpoint goroutine before closing the WAL.
	serveCancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v after context cancellation; want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return within 10s of context cancellation")
	}

	// The WAL is closed by the owned closer: a subsequent append is rejected.
	if err := wlog.Append([]byte("after-cancel")); !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("wal.Append after Serve ctx-cancel = %v; want wal.ErrWriterClosed", err)
	}
	// The checkpoint loop was stopped cleanly, before the WAL close, so it never
	// touched a closed WAL (no swallowed error).
	if le := cp.Stats().LastError; le != "" {
		t.Fatalf("checkpointer LastError = %q after Serve ctx-cancel; want empty", le)
	}
	// goleak.VerifyNone (deferred) confirms the checkpoint goroutine is gone.
}

// countingCloser records how many times Close is called. It stands in for an
// embedder-supplied io.Closer that is NOT idempotent, so the test below proves
// the server's own once-guard rather than relying on store.DB's.
type countingCloser struct{ calls atomic.Int32 }

func (c *countingCloser) Close() error {
	c.calls.Add(1)
	return nil
}

// TestServeCtxCancelThenShutdown_ClosesOwnedCloserOnce pins the once-guard on
// the owned-closer teardown (#1351): when the embedder both cancels the Serve
// context AND calls Shutdown (in either order, including a double Shutdown),
// the owned closer is closed exactly once, race-free, and both stop paths
// report success.
func TestServeCtxCancelThenShutdown_ClosesOwnedCloserOnce(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	cc := &countingCloser{}
	srv, err := server.NewServer(eng, server.Options{
		ConnTimeout: 5 * time.Second,
		Auth:        server.NoAuthHandler{},
		Closer:      cc,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx, ln) }()

	// Stop via ctx-cancellation first; Serve drains and closes the owned closer.
	serveCancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v after context cancellation; want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return within 10s of context cancellation")
	}

	// A subsequent Shutdown (already-drained server) must not close it again.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after Serve ctx-cancel: %v", err)
	}
	// Nor a double Shutdown.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}

	if got := cc.calls.Load(); got != 1 {
		t.Fatalf("owned closer Close called %d times; want exactly 1", got)
	}
}
