//go:build stress

// Package stress holds CI-only stress tests for write-path concurrency.
package stress

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestMain runs the full test suite and then checks for goroutine leaks at
// package scope via goleak.VerifyTestMain. This is the recommended goleak
// integration for packages that use t.Parallel (VerifyNone is not compatible
// with t.Parallel).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestCypherWriteConflict_MERGE verifies that serialised MERGE write calls
// interleaved with concurrent read queries leave the graph in a consistent
// state and produce no data races or goroutine leaks.
//
// MERGE semantics note: the current exec/merge.go implementation uses a stub
// searchFn that always returns no matches, so every serialised MERGE takes the
// ON CREATE path and adds a new node. With N writer goroutines (each holding
// the mutex in turn) the post-state invariant is exactly N nodes in the graph.
// The test validates race-cleanliness and goroutine hygiene rather than
// idempotent-MERGE semantics (tracked separately).
func TestCypherWriteConflict_MERGE(t *testing.T) {
	t.Parallel()

	for _, n := range []int{16, 64, 256} {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			for iter := range 10 {
				iter := iter
				t.Run(fmt.Sprintf("iter=%d", iter), func(t *testing.T) {
					runMergeConflictTest(t, n)
				})
			}
		})
	}
}

// runMergeConflictTest runs one iteration of the write-conflict stress
// scenario with n goroutines.
func runMergeConflictTest(t *testing.T, n int) {
	t.Helper()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	engine := cypher.NewEngine(g)

	// mu serialises RunInTx calls to honour the single-writer contract on
	// write queries documented in cypher/api.go (Engine.RunInTx godoc).
	var mu sync.Mutex
	ctx := context.Background()

	// ── Concurrent readers ────────────────────────────────────────────────
	// Run is safe for concurrent use (read-only, no mutation).
	// runtime.Gosched() after each scan yields the scheduler so that writer
	// goroutines waiting on a write lock can make progress. Without it, with
	// N=256 readers in a tight loop, writers starve on the mapper's RWMutex.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(n)
	for range n {
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					res, err := engine.Run(ctx, "MATCH (n) RETURN n", nil)
					if err != nil {
						runtime.Gosched()
						continue
					}
					for res.Next() {
					}
					_ = res.Err()
					res.Close() //nolint:errcheck // read-only; no index commit path
					runtime.Gosched()
				}
			}
		}()
	}

	// ── Serialised writers ────────────────────────────────────────────────
	// Each goroutine acquires mu before calling RunInTx so that at most one
	// write pipeline is active at any given moment. The node creation happens
	// during exec.Run's plan.Init call (inside RunInTx), which is under the
	// lock. Close is called outside the lock; it only commits index changes
	// (label/property indices) which are per-Result and safe to apply
	// concurrently across distinct Result instances.
	var writers sync.WaitGroup
	writers.Add(n)
	for range n {
		go func() {
			defer writers.Done()

			mu.Lock()
			res, err := engine.RunInTx(ctx, `MERGE (n:Person {name: "Alice"})`, nil)
			mu.Unlock()

			if err != nil {
				return
			}
			// Drain the result (no rows for a write-only query) and close to
			// flush the IndexBuffer.
			for res.Next() {
			}
			_ = res.Err()
			res.Close() //nolint:errcheck // index commit errors are non-fatal for stress invariant
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()

	// ── Post-state invariant ──────────────────────────────────────────────
	// Each serialised MERGE took the ON CREATE path (searchFn stub returns
	// no matches), so exactly n nodes must exist in the graph.
	res, err := engine.Run(ctx, "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("post-check Run: %v", err)
	}
	var count int
	for res.Next() {
		count++
	}
	if cerr := res.Err(); cerr != nil {
		t.Fatalf("post-check iteration error: %v", cerr)
	}
	res.Close() //nolint:errcheck // read-only result; no commit path

	if count != n {
		t.Errorf("post-state: expected %d nodes (one per serialised MERGE ON CREATE), got %d", n, count)
	}
}
