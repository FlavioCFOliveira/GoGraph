package plan

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"sync"
	"sync/atomic"
)

// ─────────────────────────────────────────────────────────────────────────────
// CacheStats
// ─────────────────────────────────────────────────────────────────────────────

// CacheStats holds a point-in-time snapshot of [Cache] metrics.
type CacheStats struct {
	// Hits is the total number of successful Get lookups since the cache was
	// created (or last [Cache.Clear]ed).
	Hits uint64
	// Misses is the total number of unsuccessful Get lookups.
	Misses uint64
	// Evictions is the total number of entries expelled by the LRU policy.
	Evictions uint64
	// Size is the current number of entries held in the cache.
	Size int
	// Cap is the maximum number of entries the cache can hold.
	Cap int
}

// ─────────────────────────────────────────────────────────────────────────────
// internal entry
// ─────────────────────────────────────────────────────────────────────────────

type cacheEntry struct {
	key   [32]byte
	value any
	prev  *cacheEntry
	next  *cacheEntry
}

// ─────────────────────────────────────────────────────────────────────────────
// Cache
// ─────────────────────────────────────────────────────────────────────────────

// Cache is a thread-safe LRU plan cache.
//
// Key: a [32]byte SHA-256 digest (see [CacheKey]).
// Value: the logical plan stored as any; the cache does not inspect values.
//
// Metrics (hits, misses, evictions) are exposed as atomic counters readable
// without acquiring the main lock via [Cache.Stats].
//
// Cache is safe for concurrent use.
type Cache struct {
	mu   sync.Mutex
	cap  int
	m    map[[32]byte]*cacheEntry
	head *cacheEntry // MRU sentinel (dummy)
	tail *cacheEntry // LRU sentinel (dummy)

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// NewCache creates a Cache with the given capacity.
// If capacity < 1 it defaults to 1024.
func NewCache(capacity int) *Cache {
	if capacity < 1 {
		capacity = 1024
	}
	head := &cacheEntry{}
	tail := &cacheEntry{}
	head.next = tail
	tail.prev = head
	return &Cache{
		cap:  capacity,
		m:    make(map[[32]byte]*cacheEntry, capacity),
		head: head,
		tail: tail,
	}
}

// Put inserts or updates the entry for key with the given value.
// If inserting causes the cache to exceed its capacity, the least-recently-used
// entry is evicted.
func (c *Cache) Put(key [32]byte, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.m[key]; ok {
		// Update existing entry and move it to the front.
		e.value = value
		c.detach(e)
		c.insertFront(e)
		return
	}

	// Evict LRU when at capacity.
	if len(c.m) >= c.cap {
		lru := c.tail.prev
		if lru != c.head { // should always be true when cap >= 1
			c.detach(lru)
			delete(c.m, lru.key)
			c.evictions.Add(1)
		}
	}

	e := &cacheEntry{key: key, value: value}
	c.m[key] = e
	c.insertFront(e)
}

// Get looks up key in the cache. If found it moves the entry to the
// most-recently-used position and returns (value, true). Otherwise it
// returns (nil, false).
func (c *Cache) Get(key [32]byte) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.m[key]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.detach(e)
	c.insertFront(e)
	c.hits.Add(1)
	return e.value, true
}

// Stats returns a point-in-time snapshot of cache metrics.
func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	size := len(c.m)
	c.mu.Unlock()
	return CacheStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		Size:      size,
		Cap:       c.cap,
	}
}

// Clear removes all entries and resets the hit/miss/eviction counters.
func (c *Cache) Clear() {
	c.mu.Lock()
	c.m = make(map[[32]byte]*cacheEntry, c.cap)
	c.head.next = c.tail
	c.tail.prev = c.head
	c.mu.Unlock()

	c.hits.Store(0)
	c.misses.Store(0)
	c.evictions.Store(0)
}

// ─────────────────────────────────────────────────────────────────────────────
// doubly-linked list helpers (called with c.mu held)
// ─────────────────────────────────────────────────────────────────────────────

// detach removes e from its current position in the list.
func (c *Cache) detach(e *cacheEntry) {
	e.prev.next = e.next
	e.next.prev = e.prev
}

// insertFront places e immediately after the head sentinel (MRU position).
func (c *Cache) insertFront(e *cacheEntry) {
	e.next = c.head.next
	e.prev = c.head
	c.head.next.prev = e
	c.head.next = e
}

// ─────────────────────────────────────────────────────────────────────────────
// CacheKey
// ─────────────────────────────────────────────────────────────────────────────

// CacheKey computes the SHA-256 cache key for a query string and a slice of
// parameter type names. The parameter names are sorted before hashing so the
// key is independent of the slice order.
//
// The key layout is:
//
//	uint32 big-endian query length | query bytes | sorted param types (each
//	prefixed with uint32 big-endian length)
//
// This makes collisions between a query with zero params and a query whose
// string happens to equal another's query+param encoding impossible.
func CacheKey(query string, paramTypes []string) [32]byte {
	sorted := make([]string, len(paramTypes))
	copy(sorted, paramTypes)
	sort.Strings(sorted)

	h := sha256.New()
	var lenBuf [4]byte

	// Write query length then query bytes.
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(query)))
	_, _ = h.Write(lenBuf[:])
	_, _ = h.Write([]byte(query))

	// Write each param type prefixed by its length.
	for _, pt := range sorted {
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(pt)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(pt))
	}

	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
