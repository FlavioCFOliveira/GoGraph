package server_test

// explicit_tx_regression_test.go — regression tests for true explicit Bolt
// transactions (#1280), connection-rooted cancellation + bounded tx timeout
// (#1302), and the handleConn teardown rollback (#1309).
//
// These drive the RAW Bolt wire (bolt test client) against a WAL-backed server
// so the open transaction holds the store's single-writer mutex — the resource
// whose prompt release (#1309) and cross-statement isolation (#1280) these tests
// assert. A store-less server would hold the engine writer mutex instead; the
// engine-level analogues live in cypher/exectx_test.go.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// init registers boltboom() on the default function registry (the registry the
// test engines use) so a Bolt RUN can drive a panic during statement execution.
// It always panics, exercising the engine-layer panic boundary on the explicit
// transaction path.
func init() {
	funcs.DefaultRegistry.Register("boltboom", func([]expr.Value) (expr.Value, error) {
		panic("boltboom: injected test panic during statement execution")
	})
}

// newWALEngine builds a WAL-backed Cypher engine over a fresh directed graph in
// a temp dir. The WAL writer is closed on test cleanup.
func newWALEngine(t *testing.T) *cypher.Engine {
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
	return cypher.NewEngineWithStore(store)
}

// startTestServerWithEngine starts a Server backed by eng on a random port,
// applying the same test defaults as startTestServer (no-auth, finite
// ConnTimeout) and registering a t.Cleanup that drains the Serve goroutine.
//
//nolint:gocritic // hugeParam: test helper takes Options by value to mirror the public NewServer signature; not a hot path.
func startTestServerWithEngine(t *testing.T, eng *cypher.Engine, opts server.Options) string {
	t.Helper()
	if opts.ConnTimeout == 0 {
		opts.ConnTimeout = 5 * time.Second
	}
	if opts.Auth == nil {
		opts.Auth = server.NoAuthHandler{}
	}
	srv, err := server.NewServer(eng, opts)
	if err != nil {
		t.Fatalf("startTestServerWithEngine NewServer: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startTestServerWithEngine listen: %v", err)
	}
	addr := ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveErr:
		case <-time.After(5 * time.Second):
			t.Log("startTestServerWithEngine: Serve goroutine did not exit in cleanup")
		}
	})
	time.Sleep(10 * time.Millisecond)
	return addr
}

// leakCountingBackend counts cypher.result.leaked increments and ignores every
// other metric, so a test can assert the finalizer leak path did NOT fire.
type leakCountingBackend struct {
	leaked atomic.Uint64
}

func (b *leakCountingBackend) IncCounter(name string, delta uint64) {
	if name == "cypher.result.leaked" {
		b.leaked.Add(delta)
	}
}
func (b *leakCountingBackend) ObserveLatency(string, time.Duration) {}

// quietLogger returns a slog.Logger that discards output, used to silence the
// panic stack-trace logs the engine/connection boundaries emit during the
// deliberate-panic tests.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestExplicitTx_Disconnect_ReleasesWriterMutexPromptly is the #1309 AC: a client
// completes the handshake, sends BEGIN + RUN CREATE, then drops the socket
// WITHOUT PULL. handleConn's deferred teardown must roll back the open
// transaction, releasing the store single-writer mutex IMMEDIATELY (not on GC),
// so a second connection's write completes promptly. cypher.result.leaked must
// NOT increment (the cursor was closed deterministically, not finalised).
//
// Pre-#1309, handleConn returned on disconnect without rolling back sess.tx: the
// store mutex stayed held until the GC finalised the leaked transaction, so the
// second connection's write would block for the GC interval (or until the
// process exited), and the finalizer leak counter would eventually fire.
func TestExplicitTx_Disconnect_ReleasesWriterMutexPromptly(t *testing.T) {
	probe := &leakCountingBackend{}
	cmetrics.SetBackend(probe)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })
	before := probe.leaked.Load()

	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{})

	// Connection A: handshake, HELLO, BEGIN, RUN CREATE (no PULL), then drop.
	cA := newBoltTestClient(t, addr)
	cA.negotiate(t)
	cA.hello(t)
	cA.begin(t)
	cA.run(t, "CREATE (:Doomed {v:1})", nil)
	// Drop the socket abruptly without PULL/COMMIT/ROLLBACK.
	cA.close(t)

	// Connection B: its write must complete promptly. We run it on a goroutine
	// with a tight deadline; the store mutex must already be free.
	done := make(chan struct{})
	go func() {
		defer close(done)
		cB := newBoltTestClient(t, addr)
		defer cB.close(t)
		cB.negotiate(t)
		cB.hello(t)
		cB.run(t, "CREATE (:B {v:1})", nil)
		cB.pullAll(t)
	}()
	select {
	case <-done:
		// Expected: the writer mutex was released on A's disconnect teardown.
	case <-time.After(5 * time.Second):
		t.Fatal("second connection's write did not complete promptly — writer mutex leaked on disconnect (#1309)")
	}

	// The cursor must have been closed deterministically on teardown, not leaked
	// to the finalizer. Give any (incorrect) finalizer a brief window.
	time.Sleep(50 * time.Millisecond)
	if delta := probe.leaked.Load() - before; delta != 0 {
		t.Errorf("cypher.result.leaked delta = %d after disconnect; want 0 (cursor closed on teardown, not GC)", delta)
	}
}

// TestExplicitTx_RollbackIsolation_SecondTxSeesNothing is the #1280 cross-session
// rollback AC over the raw Bolt wire on a WAL-backed server: session A opens a
// transaction, CREATEs a node, ROLLBACKs; a second session must then observe the
// node absent. Pre-#1280, ROLLBACK closed cursors but undid nothing, so the node
// stayed in the graph.
func TestExplicitTx_RollbackIsolation_SecondTxSeesNothing(t *testing.T) {
	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{})

	cA := newBoltTestClient(t, addr)
	defer cA.close(t)
	cA.negotiate(t)
	cA.hello(t)
	cA.begin(t)
	cA.run(t, "CREATE (:RB {v:1})", nil)
	cA.pullAll(t)
	cA.rollback(t)

	// Second session counts :RB nodes — must be zero after the rollback.
	cB := newBoltTestClient(t, addr)
	defer cB.close(t)
	cB.negotiate(t)
	cB.hello(t)
	cB.run(t, "MATCH (n:RB) RETURN count(n) AS c", nil)
	records, _ := cB.pullAll(t)
	if len(records) != 1 {
		t.Fatalf("count query returned %d records, want 1", len(records))
	}
	got, ok := records[0][0].(int64)
	if !ok {
		t.Fatalf("count value type = %T, want int64", records[0][0])
	}
	if got != 0 {
		t.Errorf("post-rollback :RB count = %d, want 0 (ROLLBACK must undo the CREATE)", got)
	}
}

// TestExplicitTx_TxTimeoutDefaultBoundsHold is the #1302 default-timeout AC: with
// no client tx_timeout, the server applies a bounded DefaultTxTimeout. We set a
// very short default, BEGIN, then issue a RUN after the bound elapses; the
// in-flight statement must observe the deadline and return a typed FAILURE rather
// than holding the writer lock forever.
func TestExplicitTx_TxTimeoutDefaultBoundsHold(t *testing.T) {
	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{
		DefaultTxTimeout: 150 * time.Millisecond,
	})

	cA := newBoltTestClient(t, addr)
	defer cA.close(t)
	cA.negotiate(t)
	cA.hello(t)
	cA.begin(t) // no tx_timeout supplied → server default (150ms) applies

	// Let the transaction deadline elapse, then RUN: the statement context
	// (derived from the tx context with the default timeout) is already past its
	// deadline, so the engine must reject it promptly.
	time.Sleep(300 * time.Millisecond)
	cA.sendRequest(t, &proto.Run{Query: "CREATE (:Late {v:1})", Parameters: map[string]any{}, Extra: map[string]any{}})
	fail := cA.recvFailure(t)
	if fail.Code == "" {
		t.Fatalf("expected a typed FAILURE code on a timed-out tx RUN, got empty")
	}

	// The writer mutex must be free afterwards: a fresh session writes promptly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		cB := newBoltTestClient(t, addr)
		defer cB.close(t)
		cB.negotiate(t)
		cB.hello(t)
		cB.run(t, "CREATE (:After {v:1})", nil)
		cB.pullAll(t)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fresh write blocked after a timed-out transaction — writer mutex held past tx_timeout (#1302)")
	}
}

// TestExplicitTx_PanicDuringRunReleasesWriterMutex is the #1309 panic variant: a
// statement that panics during execution inside an explicit transaction must be
// converted to a typed FAILURE (the connection survives), and the writer mutex
// the transaction holds must be released so a second connection writes promptly.
// The panic is contained at the engine layer (ExplicitTx.Exec's recover), which
// rolls back the transaction and releases the store single-writer mutex; the
// connection is NOT torn down, so the client may RESET and continue.
func TestExplicitTx_PanicDuringRunReleasesWriterMutex(t *testing.T) {
	// Quieten the panic stack-trace logs the engine boundary emits.
	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{
		Logger: quietLogger(),
	})

	cA := newBoltTestClient(t, addr)
	defer cA.close(t)
	cA.negotiate(t)
	cA.hello(t)
	cA.begin(t)
	// RUN a statement that panics during execution; the server must reply FAILURE
	// rather than crash or hang.
	cA.sendRequest(t, &proto.Run{Query: "RETURN boltboom()", Parameters: map[string]any{}, Extra: map[string]any{}})
	fail := cA.recvFailure(t)
	if fail.Code == "" {
		t.Fatalf("expected a typed FAILURE after a panicking RUN, got empty code")
	}

	// The writer mutex must have been released by the engine's panic rollback: a
	// fresh connection's write completes promptly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		cB := newBoltTestClient(t, addr)
		defer cB.close(t)
		cB.negotiate(t)
		cB.hello(t)
		cB.run(t, "CREATE (:AfterPanic {v:1})", nil)
		cB.pullAll(t)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("write after a panicking tx RUN blocked — writer mutex leaked on panic path (#1309)")
	}
}

// TestGoleak_BeginDisconnect verifies that opening explicit transactions and then
// dropping the connection WITHOUT COMMIT/ROLLBACK leaks no goroutines and no
// engine writer mutex: after driving many BEGIN→RUN→drop cycles concurrently, a
// final write must still complete and goleak must be clean at teardown (#1309).
func TestGoleak_BeginDisconnect(t *testing.T) {
	addr := startTestServerWithEngine(t, newWALEngine(t), server.Options{})

	const cycles = 32
	var wg sync.WaitGroup
	wg.Add(cycles)
	for i := 0; i < cycles; i++ {
		go func() {
			defer wg.Done()
			c := newBoltTestClient(t, addr)
			c.negotiate(t)
			c.hello(t)
			c.begin(t)
			c.run(t, "CREATE (:Drop {v:1})", nil)
			// Drop without PULL/COMMIT/ROLLBACK; teardown must roll back the tx.
			c.close(t)
		}()
	}
	wg.Wait()

	// If any of the dropped transactions leaked the writer mutex, this final
	// write would block; a prompt completion proves every teardown released it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		c := newBoltTestClient(t, addr)
		defer c.close(t)
		c.negotiate(t)
		c.hello(t)
		c.run(t, "CREATE (:Final {v:1})", nil)
		c.pullAll(t)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("final write blocked after BEGIN→disconnect cycles — writer mutex leaked (#1309)")
	}
}
