package cypher

import (
	"container/list"
	"sync"

	"gograph/internal/metrics"
)

// DefaultPlanCacheCapacity is the default upper bound on the number
// of entries held by an [Engine]'s plan cache. Chosen so that a
// typical OLTP workload — the same hundreds of queries reissued by
// connection pools and ORMs — stays entirely in-cache without
// unbounded growth under high query-text churn (parameter-baked
// queries, ad-hoc analytics, fuzzed input).
//
// Configure a different capacity via [EngineOptions.PlanCacheCapacity];
// pass 0 to use the default, or a positive integer to override. A
// negative value is rejected at constructor time as a configuration
// error.
const DefaultPlanCacheCapacity = 1024

// planCacheNode pairs a cache key with its entry so the doubly-
// linked list maintained by [planCache] can walk back from a list
// element to the map key for eviction.
type planCacheNode struct {
	key   string
	value *planCacheEntry
}

// planCache is a bounded LRU keyed by query text. The implementation
// is the classic map + doubly-linked-list pair: O(1) Get/Put, single
// sync.Mutex serialising the structural updates. The mutex is held
// only across the map/list manipulation; the cached *planCacheEntry
// itself is immutable once published, so callers operate on the
// returned pointer without any further synchronisation.
//
// Hit, miss and eviction events are reported via the global
// [metrics] surface under the names:
//
//   - cypher.plan_cache.hits
//   - cypher.plan_cache.misses
//   - cypher.plan_cache.evictions
//
// On the default no-op metrics backend the cost is two atomic loads
// per event.
//
// planCache is safe for concurrent use by any number of goroutines.
// A single mutex is acceptable here because plan-cache lookups are
// not on the row-level hot path: they happen once per query
// invocation, gating the per-query work that dominates the total
// runtime by orders of magnitude.
type planCache struct {
	mu  sync.Mutex
	cap int
	ll  *list.List // *planCacheNode, front = most recently used
	by  map[string]*list.Element
}

// newPlanCache constructs a planCache with the given capacity. A
// non-positive capacity falls back to [DefaultPlanCacheCapacity] so
// misconfiguration cannot silently disable the bound.
func newPlanCache(capacity int) *planCache {
	if capacity <= 0 {
		capacity = DefaultPlanCacheCapacity
	}
	return &planCache{
		cap: capacity,
		ll:  list.New(),
		by:  make(map[string]*list.Element, capacity),
	}
}

// get returns the cached entry for key and promotes it to the front
// of the LRU list. It returns (nil, false) on a miss.
func (c *planCache) get(key string) (*planCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.by[key]; ok {
		c.ll.MoveToFront(e)
		metrics.IncCounter("cypher.plan_cache.hits", 1)
		//nolint:forcetypeassert // cache invariant: list.Element.Value is always *planCacheNode
		return e.Value.(*planCacheNode).value, true
	}
	metrics.IncCounter("cypher.plan_cache.misses", 1)
	return nil, false
}

// loadOrStore returns the cached entry for key when present (without
// promoting in a single locked section) or installs entry as the new
// most-recently-used value. The bool result mirrors sync.Map's
// LoadOrStore semantics: true when a previously-installed entry was
// returned, false when the supplied entry was installed.
//
// When installing forces eviction, the least-recently-used entry is
// dropped and metrics.cypher.plan_cache.evictions is incremented.
func (c *planCache) loadOrStore(key string, entry *planCacheEntry) (*planCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.by[key]; ok {
		c.ll.MoveToFront(e)
		//nolint:forcetypeassert // cache invariant
		return e.Value.(*planCacheNode).value, true
	}
	if c.ll.Len() >= c.cap {
		back := c.ll.Back()
		if back != nil {
			c.ll.Remove(back)
			//nolint:forcetypeassert // cache invariant
			delete(c.by, back.Value.(*planCacheNode).key)
			metrics.IncCounter("cypher.plan_cache.evictions", 1)
		}
	}
	n := &planCacheNode{key: key, value: entry}
	c.by[key] = c.ll.PushFront(n)
	return entry, false
}

// Len returns the current number of cached entries. Intended for
// tests and operational introspection.
func (c *planCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Capacity returns the configured maximum. Intended for tests and
// operational introspection.
func (c *planCache) Capacity() int { return c.cap }
