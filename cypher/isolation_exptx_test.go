package cypher_test

// isolation_exptx_test.go — gate test encoding the known read-uncommitted
// isolation gap for explicit transactions (#1376).
//
// # Known gap: read-uncommitted for readers during ExplicitTx
//
// Engine.Run (read) uses Graph.View which acquires the graph visibility barrier
// as a reader. ExplicitTx.Exec applies writes EAGERLY inside
// Graph.ApplyAtomically (write-side of the same barrier) before the transaction
// is committed or rolled back. Because the eager write completes before the
// transaction ends, a concurrent Engine.Run issued between Exec and
// Commit/Rollback can observe the not-yet-committed write (dirty read).
//
// This is the documented isolation scope for explicit transactions: write-write
// Isolation is guaranteed (the writer mutex held from BeginTx until
// Commit/Rollback serialises concurrent writers), but readers are NOT isolated
// from in-flight writes.
//
// The end-state design (per-shard lock-free snapshot / deferred apply) that
// would close this gap is tracked in docs/isolation-design.md.
//
// # What this file asserts
//
//   - After Exec (before Commit/Rollback) a concurrent Engine.Run observes
//     count == 1 (the dirty write IS visible — documenting the read-uncommitted
//     behaviour).
//   - After Rollback the same Engine.Run observes count == 0 (the undo log
//     retracts the dirty write — documenting the rollback-retraction property).
//
// The test is intentionally asserting the CURRENT gap, not the desired isolation.
// If snapshot isolation is ever implemented and this test's "pre-rollback" check
// changes from 1 to 0, the assertion must be updated to reflect the new contract.
//
// Layer: short. Race-clean.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countTxNodes runs MATCH (n:Tx) RETURN count(n) AS c via Engine.Run and returns
// the integer count. It fatals if the query fails or the result is not a single
// row with a numeric "c" column.
func countTxNodes(t *testing.T, eng *cypher.Engine) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), `MATCH (n:Tx) RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("countTxNodes Run: %v", err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Errorf("countTxNodes res.Close: %v", cerr)
		}
	}()
	if !res.Next() {
		t.Fatal("countTxNodes: no row returned")
	}
	rec := res.Record()
	if err := res.Err(); err != nil {
		t.Fatalf("countTxNodes iterate: %v", err)
	}
	raw, ok := rec["c"]
	if !ok {
		t.Fatalf("countTxNodes: column 'c' absent in %v", rec)
	}
	switch v := raw.(type) {
	case int64:
		return v
	default:
		// Coerce via fmt for robustness against engine integer boxing changes.
		var n int64
		if _, err := fmt.Sscan(fmt.Sprintf("%v", raw), &n); err != nil {
			t.Fatalf("countTxNodes: cannot parse count value %T(%v): %v", raw, raw, err)
		}
		return n
	}
}

// TestExplicitTx_Isolation_ReadUncommitted encodes the current read-uncommitted
// contract for explicit transactions.
//
// KNOWN GAP: this test deliberately asserts a dirty read IS visible (count==1
// after Exec but before Commit). When snapshot isolation is implemented
// (docs/isolation-design.md), the pre-rollback count will be 0 and this
// assertion must be updated to reflect the closed gap.
func TestExplicitTx_Isolation_ReadUncommitted(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	t.Run("dirty_read_visible_before_commit", func(t *testing.T) {
		// Verify starting state: no :Tx nodes.
		if n := countTxNodes(t, eng); n != 0 {
			t.Fatalf("pre-test :Tx count = %d, want 0 (stale state)", n)
		}

		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}

		// CREATE inside the explicit transaction — eagerly applied, not yet committed.
		res, err := tx.Exec(`CREATE (:Tx) RETURN count(*) AS c`, nil)
		if err != nil {
			t.Fatalf("Exec CREATE: %v", err)
		}
		for res.Next() { //nolint:revive // drain
		}
		if err := res.Err(); err != nil {
			t.Fatalf("drain Exec result: %v", err)
		}
		_ = res.Close()

		// At this point the explicit transaction holds the writer mutex AND has
		// applied the write eagerly to the live graph. A concurrent Engine.Run
		// (reader, using Graph.View) must observe the dirty write because the
		// write is already visible in the live graph and Engine.Run acquires only
		// a read-lock on the visibility barrier.
		//
		// KNOWN GAP (read-uncommitted): the node created above is NOT yet
		// committed — it is a dirty read. See docs/isolation-design.md for the
		// tracked end-state that will close this gap.

		// readDone carries (count, error) from the concurrent reader goroutine.
		type readResult struct {
			count int64
			err   string
		}
		readDone := make(chan readResult, 1)

		// execDone signals that Exec has returned and the dirty write is live.
		// The goroutine is started AFTER Exec so the sync point is the channel
		// send itself, not a race on goroutine scheduling.
		var launchOnce sync.Once
		startRead := make(chan struct{})

		launchOnce.Do(func() {
			go func() {
				// Wait until the caller signals that Exec has completed and the
				// dirty write is in the live graph.
				<-startRead
				n := int64(-1)
				var errStr string
				func() {
					defer func() {
						if r := recover(); r != nil {
							errStr = fmt.Sprintf("panic: %v", r)
						}
					}()
					innerRes, innerErr := eng.Run(context.Background(), `MATCH (n:Tx) RETURN count(n) AS c`, nil)
					if innerErr != nil {
						errStr = innerErr.Error()
						return
					}
					defer func() { _ = innerRes.Close() }()
					if !innerRes.Next() {
						errStr = "no row from count query"
						return
					}
					rec := innerRes.Record()
					raw, ok := rec["c"]
					if !ok {
						errStr = fmt.Sprintf("column 'c' absent: %v", rec)
						return
					}
					switch v := raw.(type) {
					case int64:
						n = v
					default:
						fmt.Sscan(fmt.Sprintf("%v", raw), &n) //nolint:errcheck // best-effort int parse
					}
				}()
				readDone <- readResult{count: n, err: errStr}
			}()
		})

		// Signal the reader to proceed now that the dirty write is live.
		close(startRead)

		// Collect the concurrent read result BEFORE rolling back.
		rr := <-readDone
		if rr.err != "" {
			t.Fatalf("concurrent reader error: %s", rr.err)
		}

		// KNOWN GAP assertion: the dirty write IS visible before commit.
		// If snapshot isolation is implemented, this will become 0 and the test
		// must be updated to assert 0 here instead.
		if rr.count != 1 {
			t.Errorf("pre-rollback concurrent read count = %d, want 1 (read-uncommitted gap: dirty write must be visible)", rr.count)
		}

		// Roll back the transaction: the undo log retracts the eager write.
		if err := tx.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}

		// Post-rollback: the dirty write is gone. Engine.Run must now see 0.
		if n := countTxNodes(t, eng); n != 0 {
			t.Errorf("post-rollback :Tx count = %d, want 0 (rollback-retraction: undo log must remove the dirty write)", n)
		}
	})
}
