package cypher

import (
	"testing"
)

// TestPlanCache_LRUEviction_AccessOrderMatters verifies that the
// eviction victim is always the least-recently-used entry, where
// "recently used" is updated by every successful get call.
//
// Scenario (capacity = 3):
//
//  1. Insert a, b, c → order LRU→MRU: [a, b, c].
//  2. get(b)         → order: [a, c, b]; a is now LRU.
//  3. Insert d       → a evicted; order: [c, b, d].
//  4. get(b)         → order: [c, d, b]; c is now LRU.
//  5. Insert e       → c evicted; b and d survive.
func TestPlanCache_LRUEviction_AccessOrderMatters(t *testing.T) {
	t.Parallel()
	c := newPlanCache(3)

	// Step 1: fill to capacity.
	for _, k := range []string{"a", "b", "c"} {
		c.loadOrStore(k, newTestEntry(k))
	}
	if got := c.Len(); got != 3 {
		t.Fatalf("initial Len = %d; want 3", got)
	}

	// Step 2: promote b → a remains LRU.
	if _, ok := c.get("b"); !ok {
		t.Fatalf("get(b) miss before any eviction")
	}

	// Step 3: insert d → a (LRU) must be evicted.
	c.loadOrStore("d", newTestEntry("d"))
	if got := c.Len(); got != 3 {
		t.Fatalf("Len after first eviction = %d; want 3", got)
	}
	if _, ok := c.get("a"); ok {
		t.Error("a survived first eviction; expected a to be the LRU victim")
	}
	// Do NOT get b/c/d here — those calls would change the LRU order.

	// Step 4: promote b again → d and b are MRU; c is LRU.
	// Cache order after step 3: [c, b, d] (c=LRU because it was never
	// accessed after insertion, b was promoted in step 2 and d was
	// just inserted as MRU). We promote b once more so order is [c, d, b].
	if _, ok := c.get("b"); !ok {
		t.Fatal("get(b) miss after first eviction; b should still be cached")
	}

	// Step 5: insert e → c (LRU) must be evicted; b and d survive.
	c.loadOrStore("e", newTestEntry("e"))
	if got := c.Len(); got != 3 {
		t.Fatalf("Len after second eviction = %d; want 3", got)
	}
	if _, ok := c.get("c"); ok {
		t.Error("c survived second eviction; expected c to be the LRU victim")
	}
	for _, k := range []string{"b", "d", "e"} {
		if _, ok := c.get(k); !ok {
			t.Errorf("expected key %q to remain after second eviction", k)
		}
	}
}

// TestPlanCache_LRUEviction_FreshInsert_IsNotEvicted verifies that a
// freshly inserted entry is never immediately evicted when the cache
// is at capacity — only a pre-existing entry is evicted.
func TestPlanCache_LRUEviction_FreshInsert_IsNotEvicted(t *testing.T) {
	t.Parallel()
	c := newPlanCache(2)

	c.loadOrStore("x", newTestEntry("x"))
	c.loadOrStore("y", newTestEntry("y"))

	// z is the fresh insert; x (LRU) must be evicted, not z.
	c.loadOrStore("z", newTestEntry("z"))

	if _, ok := c.get("z"); !ok {
		t.Error("freshly inserted entry z was evicted; expected it to survive")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d; want 2", c.Len())
	}
}
