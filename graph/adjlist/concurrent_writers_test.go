package adjlist_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gograph/graph/adjlist"
)

// findShardNKeys returns n distinct int keys whose Mapper-assigned NodeID has
// low 8 bits equal to shard. The probe uses a temporary, throw-away AdjList
// so interning these keys does not affect the AdjList under test.
func findShardNKeys(t *testing.T, shard byte, n int) []int {
	t.Helper()
	const shardMask = 0xFF
	probe := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	keys := make([]int, 0, n)
	for i := 0; len(keys) < n && i < 10_000_000; i++ {
		id := probe.Mapper().Intern(i)
		if byte(uint64(id)&shardMask) == shard {
			keys = append(keys, i)
		}
	}
	if len(keys) < n {
		t.Fatalf("findShardNKeys: needed %d keys for shard %d, found only %d", n, shard, len(keys))
	}
	return keys
}

// TestAdjList_ConcurrentWriters_SizeConsistency verifies that 64 goroutines
// writing 1000 distinct edges each produce a Size() equal to the sum of
// per-goroutine success counts. No race failures must occur.
func TestAdjList_ConcurrentWriters_SizeConsistency(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 64
		edgesEach  = 1000
	)

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})

	var wg sync.WaitGroup
	var perGoroutine [goroutines]atomic.Int64

	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			base := g * edgesEach * 2 // each goroutine owns a disjoint key space
			for i := 0; i < edgesEach; i++ {
				src := base + i*2
				dst := base + i*2 + 1
				if err := a.AddEdge(src, dst, struct{}{}); err == nil {
					perGoroutine[g].Add(1)
				}
			}
		}()
	}
	wg.Wait()

	var total int64
	for i := range perGoroutine {
		total += perGoroutine[i].Load()
	}
	if got := a.Size(); got != uint64(total) {
		t.Errorf("Size() = %d, sum of per-goroutine counters = %d", got, total)
	}
}

// TestAdjList_ConcurrentWriters_CrossShardNoDeadlock verifies that many
// concurrent writers complete without deadlock. Each goroutine writes to a
// distinct key space, ensuring no shard is a bottleneck shared across all
// writers. The test must finish well within its deadline.
func TestAdjList_ConcurrentWriters_CrossShardNoDeadlock(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 64
		edgesEach  = 500
	)

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			base := g * edgesEach * 2
			for i := 0; i < edgesEach; i++ {
				src := base + i*2
				dst := base + i*2 + 1
				_ = a.AddEdge(src, dst, struct{}{})
			}
		}()
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// all writers completed — no deadlock
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent writers did not complete within 30s: possible deadlock")
	}
}

// TestAdjList_ConcurrentWriters_ShardIndependence verifies that contention on
// shard 0 does not block writers targeting shard 255. Two goroutines write to
// their respective shards concurrently; both must finish within the deadline.
func TestAdjList_ConcurrentWriters_ShardIndependence(t *testing.T) {
	t.Parallel()
	const keysPerShard = 500

	shard0 := findShardNKeys(t, 0, keysPerShard)
	shard255 := findShardNKeys(t, 255, keysPerShard)

	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine A: writes exclusively to shard 0.
	go func() {
		defer wg.Done()
		for i := 0; i < keysPerShard-1; i++ {
			_ = a.AddEdge(shard0[i], shard0[i+1], struct{}{})
		}
	}()

	// Goroutine B: writes exclusively to shard 255.
	go func() {
		defer wg.Done()
		for i := 0; i < keysPerShard-1; i++ {
			_ = a.AddEdge(shard255[i], shard255[i+1], struct{}{})
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// both goroutines finished — shards are independent
	case <-time.After(30 * time.Second):
		t.Fatal("shard-independence test did not complete within 30s: possible cross-shard blocking")
	}
}
