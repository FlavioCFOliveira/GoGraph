package server_test

// conn_cancel_test.go — regression gate for task #1348 (P5/S5):
// the per-connection message loop used the server-wide accept context, so when
// a client dropped mid-RUN nothing cancelled the running query — an autocommit
// statement with no timeout kept consuming CPU after the client vanished.
//
// The fix derives a per-connection cancellable context and runs the connection
// read in a dedicated goroutine that cancels that context on EOF/disconnect, so
// an in-flight statement (which the engine checks for cancellation per result
// row) stops promptly.
//
// GATE: a long-running autocommit RUN is cancelled (stops doing work) shortly
// after the client closes the connection mid-execution. The query is a per-row
// CREATE (whose operator checks ctx per row) over a slow test function counting
// its calls; after the client disconnects the count must stop growing. On the
// unfixed code the count keeps climbing because the shared accept context is
// never cancelled by a single disconnect.
//
// Layer: short. Not parallel: it owns the package-level boltSlowCount counter.

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// boltSlowCount counts calls to the boltslow() test function. The cancellation
// gate watches it stop growing after the client disconnects.
var boltSlowCount atomic.Int64

func init() {
	// boltslow(x) increments a global counter and sleeps briefly, then returns x.
	// Used per row by a CREATE so the engine's per-row ctx check governs how long
	// the statement runs; the sleep throttles the loop so a test can disconnect
	// mid-execution and observe whether the work stops.
	funcs.DefaultRegistry.Register("boltslow", func(args []expr.Value) (expr.Value, error) {
		boltSlowCount.Add(1)
		time.Sleep(time.Millisecond)
		if len(args) > 0 {
			return args[0], nil
		}
		return expr.IntegerValue(1), nil
	})
}

// TestConnDisconnect_CancelsInFlightRun is the regression gate for #1348.
func TestConnDisconnect_CancelsInFlightRun(t *testing.T) {
	boltSlowCount.Store(0)

	// No statement/tx timeout so the query is bounded ONLY by disconnect
	// cancellation; long idle ConnTimeout so the idle reaper never interferes.
	addr := startTestServer(t, server.Options{ConnTimeout: 30 * time.Second})

	c := newBoltTestClient(t, addr)
	// Closed explicitly mid-test to simulate the disconnect; the defer is a
	// best-effort safety net (idempotent raw close, no error reporting) for the
	// early-return paths.
	defer func() { _ = c.conn.Close() }()
	c.negotiate(t)
	c.hello(t)

	// Fire a long autocommit write whose per-row CREATE evaluates boltslow(x).
	// Do NOT read the response: handleRun blocks materialising the write, calling
	// boltslow once per row, so the server is busy executing when we disconnect.
	// range(1, 200000) builds a small list instantly; the per-row 1 ms sleep in
	// boltslow then makes the materialise loop run for ~200 s unless cancelled.
	c.sendRequest(t, &proto.Run{
		Query:      "UNWIND range(1, 200000) AS x CREATE (:BoltSlow {v: boltslow(x)})",
		Parameters: map[string]any{},
		Extra:      map[string]interface{}{},
	})

	// Wait until the query is demonstrably running (boltslow has been called a
	// few times), then close the connection mid-execution.
	deadline := time.Now().Add(3 * time.Second)
	for boltSlowCount.Load() < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("query never started: boltslow called %d times", boltSlowCount.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
	_ = c.conn.Close() // client disconnects mid-RUN

	// After the disconnect, the in-flight statement must stop promptly. Sample
	// the counter, wait, and sample again: the delta must be tiny (at most the
	// in-flight row when cancellation fired). On the unfixed code the counter
	// keeps climbing (~1 per ms over the wait window) because nothing cancels it.
	time.Sleep(50 * time.Millisecond) // let cancellation propagate
	c1 := boltSlowCount.Load()
	time.Sleep(300 * time.Millisecond)
	c2 := boltSlowCount.Load()

	if delta := c2 - c1; delta > 10 {
		t.Fatalf("in-flight RUN was not cancelled on client disconnect: boltslow called %d more times "+
			"in 300ms after disconnect (want ~0); the query kept running after the client vanished", delta)
	}
}
