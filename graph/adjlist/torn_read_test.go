package adjlist_test

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestAdjList_ConcurrentReads_ConsistentPrefix verifies the copy-on-write
// guarantee: a reader observing node 0's neighbours during a concurrent
// sequential write of edges 0→1, 0→2, …, 0→N must always see a prefix of
// the final sequence. A torn read (skipped element, out-of-order element, or
// stale interior entry) would violate the atomic-pointer swap contract.
func TestAdjList_ConcurrentReads_ConsistentPrefix(t *testing.T) {
	t.Parallel()

	const N = 10_000

	// Leave at least one core for the writer. Each reader performs O(N)
	// snapshot work per iteration, so a fixed high reader count starves the
	// lone writer under the race detector on small/loaded CI runners — the
	// observed "timeout: writer did not finish" flake on a 2-core runner.
	// Scaling to GOMAXPROCS-1 keeps genuine read/write concurrency while
	// guaranteeing the writer always has a core to run on.
	numReaders := max(2, runtime.GOMAXPROCS(0)-1)

	a := adjlist.New[int, int64](adjlist.Config{Directed: true})

	// startCh: fires both the writer and the readers simultaneously.
	// writerDone: closed by the writer goroutine when all N edges are published.
	// done: closed to stop reader goroutines; closed after writerDone.
	startCh := make(chan struct{})
	writerDone := make(chan struct{})
	done := make(chan struct{})

	var violations atomic.Int64
	var wg sync.WaitGroup

	// Writer: appends edges 0→1, 0→2, …, 0→N in order after startCh is closed.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(writerDone)
		<-startCh
		for k := 1; k <= N; k++ {
			if err := a.AddEdge(0, k, int64(k)); err != nil {
				// ErrShardFull is unexpected at this scale; count as violation
				// so the test fails with a diagnostic count.
				violations.Add(1)
				return
			}
		}
	}()

	// Readers: collect Neighbours(0) snapshots and verify the prefix invariant
	// until done is closed.
	wg.Add(numReaders)
	for range numReaders {
		go func() {
			defer wg.Done()
			<-startCh
			for {
				select {
				case <-done:
					return
				default:
				}

				var seen []int
				for v := range a.Neighbours(0) {
					seen = append(seen, v)
				}
				if !isConsistentPrefix(seen, N) {
					violations.Add(1)
				}
				// Release slice reference immediately; do not retain across
				// iterations to avoid pinning stale snapshot memory.
				seen = seen[:0:0]

				// Yield to the scheduler so the writer goroutine is not
				// starved under race-detector instrumentation.
				runtime.Gosched()
			}
		}()
	}

	// Hang guard: fires only if the writer never completes (a genuine stall),
	// not as a throughput assertion — hence generous. Normal completion is a
	// few seconds even under -race on a constrained runner.
	timer := time.AfterFunc(120*time.Second, func() {
		select {
		case <-done:
		default:
			close(done)
		}
	})
	defer timer.Stop()

	// Fire all goroutines.
	close(startCh)

	// Wait for the writer to finish, then signal readers to stop.
	select {
	case <-writerDone:
		// Writer finished normally; close done to stop readers.
		select {
		case <-done:
			// AfterFunc already closed it; nothing to do.
		default:
			close(done)
		}
	case <-done:
		// AfterFunc fired before the writer finished.
		t.Error("timeout: writer did not finish within deadline")
	}

	wg.Wait()

	if v := violations.Load(); v != 0 {
		t.Errorf("consistent-prefix violations detected: %d", v)
	}
}

// isConsistentPrefix reports whether observed is a valid prefix of the
// sequence [1, 2, 3, …] and does not exceed maxN in length.
// An empty slice is a valid prefix: the writer may not have published any
// edges yet.
func isConsistentPrefix(observed []int, maxN int) bool {
	if len(observed) > maxN {
		return false
	}
	for i, v := range observed {
		if v != i+1 {
			return false
		}
	}
	return true
}
