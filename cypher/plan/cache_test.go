package plan

import (
	"fmt"
	"sync"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func makeKey(s string) [32]byte { return CacheKey(s, nil) }

// ─────────────────────────────────────────────────────────────────────────────
// TestCache_GetPut — basic round-trip
// ─────────────────────────────────────────────────────────────────────────────

func TestCache_GetPut(t *testing.T) {
	c := NewCache(8)
	k := makeKey("MATCH (n) RETURN n")

	// Miss before Put.
	if _, ok := c.Get(k); ok {
		t.Fatal("expected miss before Put")
	}

	c.Put(k, "plan-value")

	v, ok := c.Get(k)
	if !ok {
		t.Fatal("expected hit after Put")
	}
	if v.(string) != "plan-value" {
		t.Fatalf("got %v, want plan-value", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCache_LRU_Eviction
// ─────────────────────────────────────────────────────────────────────────────

func TestCache_LRU_Eviction(t *testing.T) {
	const cacheSize = 4
	c := NewCache(cacheSize)

	keys := make([][32]byte, cacheSize+1)
	for i := range keys {
		keys[i] = makeKey(fmt.Sprintf("query-%d", i))
	}

	// Fill to capacity; keys[0] is LRU.
	for i := range cacheSize {
		c.Put(keys[i], i)
	}

	// Insert one more entry — keys[cacheSize] should evict keys[0] (the LRU).
	c.Put(keys[cacheSize], cacheSize)

	if _, ok := c.Get(keys[0]); ok {
		t.Fatal("LRU entry keys[0] should have been evicted")
	}
	for i := 1; i <= cacheSize; i++ {
		if _, ok := c.Get(keys[i]); !ok {
			t.Fatalf("entry keys[%d] should still be present", i)
		}
	}

	stats := c.Stats()
	if stats.Evictions != 1 {
		t.Fatalf("eviction count: got %d, want 1", stats.Evictions)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCache_HitRate
// ─────────────────────────────────────────────────────────────────────────────

func TestCache_HitRate(t *testing.T) {
	const warmEntries = 100
	const queries = 1000

	c := NewCache(warmEntries)

	keys := make([][32]byte, warmEntries)
	for i := range keys {
		keys[i] = makeKey(fmt.Sprintf("query-%d", i))
		c.Put(keys[i], i)
	}

	for i := range queries {
		c.Get(keys[i%warmEntries])
	}

	stats := c.Stats()
	total := stats.Hits + stats.Misses
	if total == 0 {
		t.Fatal("no queries recorded")
	}
	hitRate := float64(stats.Hits) / float64(total)
	if hitRate < 0.9 {
		t.Fatalf("hit rate too low: %.2f (hits=%d misses=%d)", hitRate, stats.Hits, stats.Misses)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCache_Concurrent
// ─────────────────────────────────────────────────────────────────────────────

func TestCache_Concurrent(t *testing.T) {
	const goroutines = 50
	const opsPerGoroutine = 200
	const keysPerGoroutine = 20

	c := NewCache(64)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(g int) {
			defer wg.Done()
			for i := range opsPerGoroutine {
				k := makeKey(fmt.Sprintf("g%d-q%d", g, i%keysPerGoroutine))
				c.Put(k, i)
				c.Get(k)
			}
		}(g)
	}
	wg.Wait()
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCacheKey_Deterministic
// ─────────────────────────────────────────────────────────────────────────────

func TestCacheKey_Deterministic(t *testing.T) {
	query := "MATCH (n:Person) RETURN n"
	params := []string{"string", "int64"}

	k1 := CacheKey(query, params)
	k2 := CacheKey(query, params)
	if k1 != k2 {
		t.Fatal("same inputs produced different keys")
	}

	// Order-independent.
	k3 := CacheKey(query, []string{"int64", "string"})
	if k1 != k3 {
		t.Fatal("param order should not affect key")
	}

	// Different param types → different key.
	k4 := CacheKey(query, []string{"float64"})
	if k1 == k4 {
		t.Fatal("different param types should produce different keys")
	}

	// No params vs one param → different key.
	k5 := CacheKey(query, nil)
	if k1 == k5 {
		t.Fatal("nil params should differ from non-empty params")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCache_Stats
// ─────────────────────────────────────────────────────────────────────────────

func TestCache_Stats(t *testing.T) {
	c := NewCache(4)
	k1 := makeKey("q1")
	k2 := makeKey("q2")
	k3 := makeKey("q3")

	// 1 miss
	c.Get(k1)

	// 2 puts
	c.Put(k1, "v1")
	c.Put(k2, "v2")

	// 2 hits
	c.Get(k1)
	c.Get(k2)

	// 1 miss
	c.Get(k3)

	st := c.Stats()
	if st.Hits != 2 {
		t.Fatalf("hits: got %d, want 2", st.Hits)
	}
	if st.Misses != 2 {
		t.Fatalf("misses: got %d, want 2", st.Misses)
	}
	if st.Evictions != 0 {
		t.Fatalf("evictions: got %d, want 0", st.Evictions)
	}
	if st.Size != 2 {
		t.Fatalf("size: got %d, want 2", st.Size)
	}
	if st.Cap != 4 {
		t.Fatalf("cap: got %d, want 4", st.Cap)
	}

	// Clear resets everything.
	c.Clear()
	st2 := c.Stats()
	if st2.Hits != 0 || st2.Misses != 0 || st2.Evictions != 0 || st2.Size != 0 {
		t.Fatalf("after Clear: unexpected stats %+v", st2)
	}
}
