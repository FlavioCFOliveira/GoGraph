package cypher

import (
	"fmt"
	"testing"
)

// TestPlanCache_LargeChurn_LRUEviction drives 1 000 000 distinct
// query strings through a small cache and verifies the cache never
// exceeds its configured capacity. This exercises the LRU eviction
// path at scale, confirming the data structure does not leak entries
// or grow unboundedly under continuous churn.
func TestPlanCache_LargeChurn_LRUEviction(t *testing.T) {
	t.Parallel()
	const (
		capacity = 10
		queries  = 1_000_000
	)
	c := newPlanCache(capacity)
	for i := range queries {
		key := fmt.Sprintf("query_%d", i)
		c.loadOrStore(key, newTestEntry(key))
	}
	// queries is five orders of magnitude above capacity, so every shard has
	// been offered far more keys than its own bound: the saturated cache holds
	// exactly the declared total.
	if got := c.Len(); got != capacity {
		t.Errorf("Len after %d inserts into cap-%d cache = %d; want %d",
			queries, capacity, got, capacity)
	}
}

// TestPlanCache_EvictionPreservesCapacity verifies that repeated
// eviction cycles (fill → overfill) always leave the cache at
// exactly its configured capacity, not fewer.
//
// DELIBERATE CHANGE (rmp #2852): the per-cycle key count was raised from
// cacheSize*3 to cacheSize*40. With the cache sharded, "exactly capacity"
// is reached only once EVERY shard has been offered at least its own bound;
// 15 keys over the 4 shards a capacity-5 cache clamps to would have left a
// shard short by chance often enough to make the assertion flaky, which is a
// property of the sample size and not of the cache. The assertion itself is
// unchanged and is now made twice: the bound is checked as an inequality on
// every single insert, and the saturation equality at the end of each cycle.
func TestPlanCache_EvictionPreservesCapacity(t *testing.T) {
	t.Parallel()
	const (
		cacheSize = 5
		perCycle  = cacheSize * 40
		cycles    = 3
	)
	c := newPlanCache(cacheSize)

	for cycle := range cycles {
		base := cycle * perCycle
		for i := range perCycle {
			key := fmt.Sprintf("q-%d", base+i)
			c.loadOrStore(key, newTestEntry(key))
			if got := c.Len(); got > cacheSize {
				t.Fatalf("cycle %d insert %d: Len = %d exceeds the declared total bound %d",
					cycle, i, got, cacheSize)
			}
		}
		if got := c.Len(); got != cacheSize {
			t.Errorf("cycle %d: Len = %d; want %d", cycle, got, cacheSize)
		}
	}
}
