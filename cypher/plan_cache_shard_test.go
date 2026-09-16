package cypher

import (
	"fmt"
	"sync"
	"testing"
	"unsafe"
)

// TestPlanCacheShard_IsCacheLineSized pins the padding planCacheShardPadBytes
// exists for: that two adjacent shards in the shards slice are far enough
// apart that locking one cannot dirty the other's cache line. The Go spec puts
// slice element i at offset i*Sizeof(T), so the size IS the stride. If a field
// is added to planCacheShard, or sync.Mutex changes size, this fails rather
// than letting two shards silently share a line again.
//
// The lower bound is the smaller of the two line sizes Go assumes for this
// module's targets, so the property holds on any build. The exact 128-byte
// stride the constant is tuned for is pinned only on a 64-bit build, where the
// field block is 32 bytes; no shipped target is 32-bit (.goreleaser.yaml ships
// amd64 and arm64 only) and the property assertion still covers one.
func TestPlanCacheShard_IsCacheLineSized(t *testing.T) {
	t.Parallel()
	const (
		minStride  = 64
		wantStride = 128
	)
	stride := unsafe.Sizeof(planCacheShard{})
	if stride < minStride {
		t.Fatalf("stride between adjacent shards = %d bytes; want at least %d — two shards can share a cache line; adjust planCacheShardPadBytes",
			stride, minStride)
	}
	if unsafe.Sizeof(uintptr(0)) == 8 && stride != wantStride {
		t.Fatalf("stride between adjacent shards = %d bytes on a 64-bit build; want exactly %d — planCacheShardPadBytes is no longer tuned to the field block",
			stride, wantStride)
	}
}

// TestPlanCache_ShardCount_ClampedToCapacity asserts the two properties the
// shard split must have for the total bound to be both exact and useful:
//
//   - the effective shard count is a power of two, at most the requested count
//     AND at most the capacity, so the index is a mask and no shard is left
//     with a bound of zero (a zero-bound shard would make every key hashing to
//     it permanently uncacheable);
//   - the per-shard bounds SUM to exactly the requested capacity.
//
// Both are properties of the constructor's arithmetic alone, so neither
// depends on the per-cache hash seed.
func TestPlanCache_ShardCount_ClampedToCapacity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		capacity, shards, wantShards int
	}{
		{1, 64, 1},
		{2, 64, 2},
		{3, 64, 2},
		{5, 64, 4},
		{7, 8, 4},
		{8, 8, 8},
		{100, 64, 64},
		{1024, 64, 64},
		{1024, 1, 1},
		{1024, 0, 1},
		{1024, -3, 1},
		{DefaultPlanCacheCapacity, planCacheShards, planCacheShards},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("cap%d_shards%d", tc.capacity, tc.shards), func(t *testing.T) {
			t.Parallel()
			c := newPlanCacheWithShards(tc.capacity, tc.shards)
			if got := c.Shards(); got != tc.wantShards {
				t.Fatalf("Shards = %d; want %d", got, tc.wantShards)
			}
			if got := c.Capacity(); got != tc.capacity {
				t.Fatalf("Capacity = %d; want %d", got, tc.capacity)
			}
			sum := 0
			for i := range c.shards {
				if c.shards[i].cap < 1 {
					t.Fatalf("shard %d has cap %d; every shard must hold at least one entry", i, c.shards[i].cap)
				}
				sum += c.shards[i].cap
			}
			if sum != tc.capacity {
				t.Fatalf("sum of shard bounds = %d; want exactly %d", sum, tc.capacity)
			}
		})
	}
}

// TestPlanCache_TotalBoundIsExact_UnderChurn fills far past capacity with
// distinct keys and asserts after EVERY insert that the total entry count has
// never exceeded the declared bound. This is the sharded replacement for the
// single-list "Len == cap" invariant: the bound is a property of the sum of the
// per-shard bounds, so it is the sum that must be checked.
//
// The per-insert inequality is deterministic. The closing equality is not
// strictly so — reaching exactly the bound requires every shard to have been
// offered at least its own bound, and the shard a key lands in depends on the
// per-cache hash seed. The key count is 50x the capacity, which puts the
// probability of a short shard below 1e-20 in the tightest case here; it is
// recorded rather than hidden because it is a different class of assertion
// from the rest of this file.
func TestPlanCache_TotalBoundIsExact_UnderChurn(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{1, 3, 8, 64, 1024} {
		t.Run(fmt.Sprintf("cap%d", capacity), func(t *testing.T) {
			t.Parallel()
			c := newPlanCache(capacity)
			for i := range capacity * 50 {
				key := fmt.Sprintf("q-%d", i)
				c.loadOrStore(key, newTestEntry(key))
				if got := c.Len(); got > capacity {
					t.Fatalf("after %d inserts Len = %d; exceeds the declared total bound %d", i+1, got, capacity)
				}
			}
			if got := c.Len(); got != capacity {
				t.Fatalf("after saturation Len = %d; want exactly %d", got, capacity)
			}
		})
	}
}

// TestPlanCache_TotalBoundIsExact_UnderConcurrentChurn is the same bound under
// contention, so the race detector also covers the sharded write paths.
func TestPlanCache_TotalBoundIsExact_UnderConcurrentChurn(t *testing.T) {
	t.Parallel()
	const (
		capacity    = 64
		goroutines  = 16
		insertsEach = 500
	)
	c := newPlanCache(capacity)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func() {
			defer wg.Done()
			for i := range insertsEach {
				key := fmt.Sprintf("g%d-q%d", g, i)
				c.loadOrStore(key, newTestEntry(key))
				c.get(key)
				if got := c.Len(); got > capacity {
					t.Errorf("goroutine %d insert %d: Len = %d exceeds bound %d", g, i, got, capacity)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := c.Len(); got > capacity {
		t.Fatalf("final Len = %d; exceeds bound %d", got, capacity)
	}
}

// sameShardKeys returns n distinct keys that all hash to one shard of c, so a
// test can exercise the per-shard LRU order inside a genuinely multi-shard
// cache. It fails the test rather than looping forever if the keys cannot be
// found, which cannot happen for any sane hash but must not hang if it does.
func sameShardKeys(t *testing.T, c *planCache, n int) []string {
	t.Helper()
	// Without this the helper would still "pass" on a one-shard cache while
	// silently testing nothing about sharding.
	if c.Shards() < 2 {
		t.Fatalf("cache has %d shard(s); this helper is only meaningful on a multi-shard cache", c.Shards())
	}
	buckets := make(map[*planCacheShard][]string)
	for i := range 100_000 {
		k := fmt.Sprintf("same-shard-probe-%d", i)
		s := c.shardFor(k)
		buckets[s] = append(buckets[s], k)
		if len(buckets[s]) == n {
			return buckets[s]
		}
	}
	t.Fatalf("could not find %d keys sharing one shard out of %d", n, c.Shards())
	return nil
}

// TestPlanCache_PerShardLRUOrder asserts the eviction contract the sharded
// cache actually offers: within one shard the victim is still that shard's
// least-recently-used entry, and get still promotes.
//
// It is the multi-shard companion to TestPlanCache_LRUEviction_AccessOrderMatters,
// which now drives a single-shard cache.
func TestPlanCache_PerShardLRUOrder(t *testing.T) {
	t.Parallel()
	// A capacity high enough that the shard count is not clamped, divided so
	// each shard holds exactly 3 entries.
	c := newPlanCacheWithShards(3*planCacheShards, planCacheShards)
	keys := sameShardKeys(t, c, 4)
	a, b, cc, d := keys[0], keys[1], keys[2], keys[3]

	for _, k := range []string{a, b, cc} {
		c.loadOrStore(k, newTestEntry(k))
	}
	// Promote a, so b is that shard's least-recently-used entry.
	if _, ok := c.get(a); !ok {
		t.Fatalf("get(%q) missed immediately after store", a)
	}
	c.loadOrStore(d, newTestEntry(d))

	if _, ok := c.get(b); ok {
		t.Errorf("key %q survived; the shard's least-recently-used entry must be the victim", b)
	}
	for _, k := range []string{a, cc, d} {
		if _, ok := c.get(k); !ok {
			t.Errorf("key %q was evicted; only the shard's LRU entry may be", k)
		}
	}
}

// TestPlanCache_FreshInsertSurvives_AnyShard is the sharded form of
// TestPlanCache_LRUEviction_FreshInsert_IsNotEvicted: whatever shard a key
// lands in, installing it evicts that shard's back entry BEFORE pushing the new
// one, so the newly installed entry is never the victim of its own insert.
func TestPlanCache_FreshInsertSurvives_AnyShard(t *testing.T) {
	t.Parallel()
	const capacity = 16
	c := newPlanCache(capacity)
	for i := range capacity * 20 {
		key := fmt.Sprintf("fresh-%d", i)
		c.loadOrStore(key, newTestEntry(key))
		if _, ok := c.get(key); !ok {
			t.Fatalf("insert %d: the freshly installed key %q was evicted by its own insert", i, key)
		}
	}
}

// TestPlanCache_Clear_EmptiesEveryShard asserts that clear resets every shard,
// not merely the one a probe key happens to hash to.
func TestPlanCache_Clear_EmptiesEveryShard(t *testing.T) {
	t.Parallel()
	c := newPlanCache(DefaultPlanCacheCapacity)
	const n = 500
	for i := range n {
		key := fmt.Sprintf("clear-%d", i)
		c.loadOrStore(key, newTestEntry(key))
	}
	if got := c.Len(); got != n {
		t.Fatalf("before clear Len = %d; want %d", got, n)
	}
	c.clear()
	if got := c.Len(); got != 0 {
		t.Fatalf("after clear Len = %d; want 0", got)
	}
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		ln, mn := s.ll.Len(), len(s.by)
		s.mu.Unlock()
		if ln != 0 || mn != 0 {
			t.Fatalf("shard %d after clear: list=%d map=%d; want 0/0", i, ln, mn)
		}
	}
}

// TestPlanCache_Clear_ConcurrentWithLookups drives clear against concurrent
// get / loadOrStore traffic. Under -race it covers the all-shards lock order in
// clear against the single-shard locks the lookups take; the assertion is that
// the bound still holds and nothing deadlocks.
func TestPlanCache_Clear_ConcurrentWithLookups(t *testing.T) {
	t.Parallel()
	const (
		capacity = 32
		workers  = 8
		rounds   = 2000
	)
	c := newPlanCache(capacity)
	var wg sync.WaitGroup
	wg.Add(workers + 1)
	for g := range workers {
		go func() {
			defer wg.Done()
			for i := range rounds {
				key := fmt.Sprintf("g%d-k%d", g, i%64)
				if _, ok := c.get(key); !ok {
					c.loadOrStore(key, newTestEntry(key))
				}
			}
		}()
	}
	go func() {
		defer wg.Done()
		for range rounds / 20 {
			c.clear()
		}
	}()
	wg.Wait()
	if got := c.Len(); got > capacity {
		t.Fatalf("final Len = %d; exceeds bound %d", got, capacity)
	}
}
