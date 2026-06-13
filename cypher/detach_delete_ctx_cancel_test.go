package cypher_test

// detach_delete_ctx_cancel_test.go — sprint #157, task #1308 (integration).
//
// End-to-end check that a DETACH DELETE of a high-degree node, run through
// Engine.RunInTx, honours context cancellation that arrives while the operator
// is sweeping the node's incident edges. The operator-level isolation of the
// in-sweep ctx check (that the sweep aborts having removed zero edges) is in
// cypher/exec/detach_delete_ctx_test.go; this test verifies the ctx is threaded
// from RunInTx to the operator and that a mid-execution cancellation surfaces as
// a context error from the write entrypoint, leaving the graph unchanged
// (atomic rollback under the visibility barrier).
//
// Layer: the CancelDuringSweep stress test is soak-gated (#1460); the
// LiveContext test stays short. Race-clean.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// cancelAfterNErr reports cancelled once Err() has been called more than n
// times. RunInTx polls ctx.Err() a small, bounded number of times during
// entry/parse/plan before execution; n is set comfortably past that prefix and
// below the full-run poll count so cancellation lands during the edge sweep
// rather than at the pre-parse entry guard (which the dedicated
// already-cancelled tests in api_ctx_cancel_test.go already cover).
type cancelAfterNErr struct {
	context.Context
	n     int64
	calls atomic.Int64
}

func (c *cancelAfterNErr) Err() error {
	if c.calls.Add(1) <= c.n {
		return nil
	}
	return context.Canceled
}

// seedHub creates a Hub node with `leaves` outgoing edges to fresh Leaf nodes
// and returns the engine. The graph is store-less and in-memory.
func seedHub(t *testing.T, leaves int) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	q := fmt.Sprintf(
		`CREATE (h:Hub {id: 0}) WITH h UNWIND range(1, %d) AS i CREATE (h)-[:R]->(:Leaf {id: i})`,
		leaves,
	)
	if _, err := eng.RunInTx(context.Background(), q, nil); err != nil {
		t.Fatalf("seed hub: %v", err)
	}
	return eng
}

// countHubEdges returns the number of outgoing edges from the Hub node by
// counting result rows (one row per edge), avoiding any dependence on the
// concrete scalar type of a count(...) projection.
func countHubEdges(t *testing.T, eng *cypher.Engine) int {
	t.Helper()
	res, err := eng.Run(context.Background(), `MATCH (:Hub)-[r]->() RETURN r`, nil)
	if err != nil {
		t.Fatalf("count edges: %v", err)
	}
	c := 0
	for res.Next() {
		c++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("count edges result: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("count edges close: %v", err)
	}
	return c
}

// TestRunInTx_DetachDelete_CancelDuringSweep_ReturnsCtxError verifies that a
// DETACH DELETE of a supernode whose context is cancelled during execution
// returns a context error from RunInTx and leaves every edge intact (the write
// is rolled back atomically under the barrier).
func TestRunInTx_DetachDelete_CancelDuringSweep_ReturnsCtxError(t *testing.T) {
	testlayers.RequireSoak(t) // 20k-leaf cancellation stress → soak layer (short-layer per-package budget, #1460)
	t.Parallel()
	const leaves = 20_000
	eng := seedHub(t, leaves)
	if got := countHubEdges(t, eng); got != leaves {
		t.Fatalf("setup: hub has %d edges; want %d", got, leaves)
	}

	// n == 10 is past RunInTx's entry/parse/plan poll prefix and below the
	// full-run poll count for a 20k-edge sweep, so cancellation lands during
	// execution (the edge sweep), not at the pre-parse entry guard.
	ctx := &cancelAfterNErr{Context: context.Background(), n: 10}
	res, err := eng.RunInTx(ctx, `MATCH (h:Hub {id: 0}) DETACH DELETE h`, nil)
	// The cancellation may surface either as the error from RunInTx directly or
	// as the Result's iteration error, depending on where the poll trips; accept
	// both and require a context.Canceled.
	gotErr := err
	if gotErr == nil && res != nil {
		for res.Next() {
		}
		gotErr = res.Err()
		_ = res.Close()
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("DETACH DELETE under cancelled ctx: err = %v; want errors.Is(err, context.Canceled)", gotErr)
	}

	// Atomicity: the cancelled write must have rolled back; all edges intact.
	if got := countHubEdges(t, eng); got != leaves {
		t.Fatalf("after cancelled DETACH DELETE: hub has %d edges; want %d (rollback)", got, leaves)
	}
}

// TestRunInTx_DetachDelete_LiveContext_DeletesAll is the non-cancelled control:
// a live context deletes the hub and all its incident edges.
func TestRunInTx_DetachDelete_LiveContext_DeletesAll(t *testing.T) {
	t.Parallel()
	const leaves = 2_000
	eng := seedHub(t, leaves)

	res, err := eng.RunInTx(context.Background(), `MATCH (h:Hub {id: 0}) DETACH DELETE h`, nil)
	if err != nil {
		t.Fatalf("DETACH DELETE: %v", err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("DETACH DELETE result: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("DETACH DELETE close: %v", err)
	}
	if got := countHubEdges(t, eng); got != 0 {
		t.Fatalf("after DETACH DELETE: hub has %d edges; want 0", got)
	}
}
