package adjlist_test

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/internal/shapegen"
)

// TestAdjList_ConcurrentReads_NeighboursIterator verifies that
// Neighbours is safe for any number of concurrent readers: 64 goroutines
// iterate the adjacency list of random nodes simultaneously for 2 s.
// The test relies on -race to surface any data race; its own assertions
// only confirm that work was actually done.
func TestAdjList_ConcurrentReads_NeighboursIterator(t *testing.T) {
	t.Parallel()

	shape := shapegen.BarabasiAlbert(10000, 3, 42)
	g, err := shape.Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()

	// Collect all user keys into a flat slice for O(1) random sampling.
	var keys []int
	a.Mapper().Walk(func(_ graph.NodeID, k int) bool {
		keys = append(keys, k)
		return true
	})
	if len(keys) == 0 {
		t.Fatal("graph has no nodes")
	}

	const numGoroutines = 64
	done := make(chan struct{})
	time.AfterFunc(2*time.Second, func() { close(done) })

	var total atomic.Int64
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	for g := range numGoroutines {
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(id), 0)) //nolint:gosec // deterministic per-goroutine RNG
			var checksum int64
			var count int64
			for {
				select {
				case <-done:
					total.Add(count)
					return
				default:
				}
				k := keys[rng.IntN(len(keys))]
				for v, w := range a.Neighbours(k) {
					checksum += int64(v) + w
					count++
				}
			}
		}(g)
	}
	wg.Wait()

	if total.Load() == 0 {
		t.Error("no neighbour iterations performed — graph may be empty")
	}
}

// TestAdjList_ConcurrentReads_ZeroAllocs verifies that iterating
// Neighbours on a warm graph allocates zero bytes on the heap.
// Must not call t.Parallel: testing.AllocsPerRun panics inside a
// parallel sub-test.
func TestAdjList_ConcurrentReads_ZeroAllocs(t *testing.T) {
	shape := shapegen.BarabasiAlbert(1000, 3, 42)
	g, err := shape.Build(adjlist.Config{Directed: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()

	var keys []int
	a.Mapper().Walk(func(_ graph.NodeID, k int) bool {
		keys = append(keys, k)
		return true
	})
	if len(keys) == 0 {
		t.Fatal("graph has no nodes")
	}

	// Node 0 is the earliest-interned hub in BA graphs and typically
	// accumulates the most edges under preferential attachment.
	src := keys[0]

	// Warmup: ensure iterator code paths are compiled and caches are
	// hot before measuring.
	for range 100 {
		for v, w := range a.Neighbours(src) {
			_ = v
			_ = w
		}
	}

	allocs := testing.AllocsPerRun(100, func() {
		for v, w := range a.Neighbours(src) {
			_ = v
			_ = w
		}
	})
	if allocs > 0 {
		t.Errorf("AllocsPerRun = %.1f, want 0", allocs)
	}
}
