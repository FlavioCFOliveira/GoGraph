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
	if got := c.Len(); got != capacity {
		t.Errorf("Len after %d inserts into cap-%d cache = %d; want %d",
			queries, capacity, got, capacity)
	}
}

// TestPlanCache_EvictionPreservesCapacity verifies that repeated
// eviction cycles (fill → overfill) always leave the cache at
// exactly its configured capacity, not fewer.
func TestPlanCache_EvictionPreservesCapacity(t *testing.T) {
	t.Parallel()
	const cacheSize = 5
	c := newPlanCache(cacheSize)

	// Three fill-and-overflow cycles.
	for cycle := range 3 {
		base := cycle * (cacheSize * 3)
		for i := range cacheSize * 3 {
			key := fmt.Sprintf("q-%d", base+i)
			c.loadOrStore(key, newTestEntry(key))
		}
		if got := c.Len(); got != cacheSize {
			t.Errorf("cycle %d: Len = %d; want %d", cycle, got, cacheSize)
		}
	}
}
