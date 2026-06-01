//go:build soak

package scenarios_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/search"
)

// TestStreamingIngest_BulkLoaderConcurrentReads exercises the following
// pattern under -race:
//
//  1. A single writer goroutine builds an adjacency list incrementally,
//     adding 100 edges at a time (deterministic pairs derived from a
//     counter), then atomically publishes a new CSR snapshot after each
//     batch.
//  2. Eight reader goroutines repeatedly load the latest CSR snapshot via
//     an atomic.Pointer and run BFS from NodeID 0.
//  3. The test runs for 5 seconds, then asserts that readers completed at
//     least 100 iterations in total with no races or panics.
func TestStreamingIngest_BulkLoaderConcurrentReads(t *testing.T) {
	const (
		totalEdges    = 2_000
		batchSize     = 100
		numReaders    = 8
		duration      = 5 * time.Second
		minIterations = 100
	)

	// snap holds the latest immutable CSR snapshot; readers load it atomically.
	var snap atomic.Pointer[csr.CSR[struct{}]]

	// Seed with an empty snapshot so readers never observe a nil pointer.
	initial := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	snap.Store(csr.BuildFromAdjList(initial))

	done := make(chan struct{})
	var totalIter atomic.Int64
	var wg sync.WaitGroup

	// Writer: adds edges in batches, publishes a fresh CSR after each batch.
	wg.Add(1)
	go func() {
		defer wg.Done()
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		for edge := 0; edge < totalEdges; {
			for k := 0; k < batchSize && edge < totalEdges; k++ {
				u := edge % 50
				v := (edge + 7) % 50
				if u != v {
					_ = a.AddEdge(u, v, struct{}{})
				}
				edge++
			}
			snap.Store(csr.BuildFromAdjList(a))
			time.Sleep(time.Millisecond)
		}
	}()

	// Readers: continuously load snapshot and BFS from node 0.
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				c := snap.Load()
				if c.MaxNodeID() > 0 {
					search.BFS(c, graph.NodeID(0), func(_ graph.NodeID, _ int) bool { return true })
				}
				totalIter.Add(1)
			}
		}()
	}

	// Run for the configured duration, then stop readers and wait.
	time.Sleep(duration)
	close(done)
	wg.Wait()

	iter := totalIter.Load()
	t.Logf("reader iterations: %d", iter)
	if iter < minIterations {
		t.Errorf("readers completed %d iterations, want >= %d", iter, minIterations)
	}
}
