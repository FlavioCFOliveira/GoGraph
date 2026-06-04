package server_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
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

// TestServerShutdown_TearsDownOwnedCloser proves the bolt.Server adoption of
// the composed store.DB closer: a Server constructed with Options.Closer tears
// the whole durability stack down — in the crash-safe order — when Shutdown
// drains, leaving NO leaked checkpoint goroutine (goleak) and a closed WAL
// (a subsequent Append is rejected).
//
// Before this change Server.Shutdown drained connections only and never touched
// the store/WAL/checkpointer, so the embedder's checkpoint goroutine outlived
// the server. The assertion is that Shutdown now teardowns the owned closer.
//
// Not parallel: goleak.IgnoreCurrent snapshots the goroutine set at call time,
// so a parallel sibling's in-flight goroutines would pollute the snapshot. The
// snapshot is taken BEFORE the checkpointer is started, so the checkpoint
// goroutine is in-scope for the leak check — if Shutdown failed to stop it, the
// VerifyNone below would fail.
func TestServerShutdown_TearsDownOwnedCloser(t *testing.T) {
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

	addr := startServeBackground(t, srv)

	// Drive one real authenticated write so the engine/store/WAL path is live
	// and the checkpointer has committed frames to fold.
	c := newBoltTestClient(t, addr)
	c.negotiate(t)
	c.hello(t)
	c.run(t, `CREATE (a:Person {name: 'alice'})-[:KNOWS]->(b:Person {name: 'bob'})`, nil)
	c.pullAll(t)
	c.goodbye(t)
	c.close(t)

	// Graceful shutdown: drains the (now-idle) connection set, then closes the
	// owned store.DB — stopping the checkpoint goroutine before closing the WAL.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// The WAL is closed by the owned closer: a subsequent append is rejected.
	if err := wlog.Append([]byte("after-shutdown")); !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("wal.Append after Shutdown = %v; want wal.ErrWriterClosed", err)
	}
	// The checkpoint loop was stopped cleanly, before the WAL close, so it never
	// touched a closed WAL (no swallowed error).
	if le := cp.Stats().LastError; le != "" {
		t.Fatalf("checkpointer LastError = %q after Shutdown; want empty", le)
	}
	// goleak.VerifyNone (deferred) confirms the checkpoint goroutine is gone.
}

// startServeBackground starts srv.Serve on a fresh 127.0.0.1 listener in a
// background goroutine and returns the dial address. A t.Cleanup drains the
// Serve goroutine so the test binary's goleak gate stays clean even if the test
// fails before Shutdown.
func startServeBackground(t *testing.T, srv *server.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	serveCtx, serveCancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx, ln) }()
	t.Cleanup(func() {
		serveCancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
		}
	})
	// Give the accept loop a moment to enter Accept.
	time.Sleep(10 * time.Millisecond)
	return addr
}
