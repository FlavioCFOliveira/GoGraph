package cypher

import (
	"container/list"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// DefaultEdgeTypeFilterCacheCapacity is the default upper bound on the
// number of entries held by an [Engine]'s edge-type-filter cache. Chosen
// smaller than [DefaultPlanCacheCapacity]: the key space here is the set of
// distinct relationship-type combinations a workload's queries actually use
// (typically a handful — a schema has few relationship types), not raw
// query text, which churns far more under parameterisation and ad-hoc
// analytics.
//
// Configure a different capacity via [EngineOptions.EdgeTypeFilterCacheCapacity];
// pass 0 to use the default, or a positive integer to override. A negative
// value is rejected at construction time the same way PlanCacheCapacity is.
const DefaultEdgeTypeFilterCacheCapacity = 256

// edgeTypeFilterEntry pairs a built filter map with the
// [lpg.Graph.TopoGeneration] value observed at the moment it was built. A
// cached entry is valid for reuse exactly when the graph's current
// generation still equals epoch — any edge addition, removal, or undo of
// either bumps the generation, invalidating every entry unconditionally
// (rmp #1871; see [lpg.Graph.TopoGeneration] for why a single global,
// purely-monotonic counter is the correct — not merely convenient —
// invalidation signal for a CSR-position-keyed map like this one).
type edgeTypeFilterEntry struct {
	filter map[uint64]string
	// at is the graph state the filter describes. It is a [csrPairKey] and not
	// a bare epoch because the filter is indexed by CSR ARC POSITION, so it is
	// only meaningful against the exact pair it was built from — and under MVCC
	// that pair depends on the reader's instant as well as the epoch. Keying on
	// the epoch alone served a filter built against a 5-arc pair to a reader
	// holding a 6-arc one: not merely stale, MISALIGNED, since position i is a
	// different arc in the two pairs. It undercounted a relationship-typed
	// expand — TestCSRPairCache_ConcurrentQueriesAgree, 183 runs in 200
	// (rmp #2293).
	at csrPairKey
}

// edgeTypeFilterCacheNode pairs a cache key with its entry, mirroring
// [planCacheNode]'s role for the doubly-linked LRU list.
type edgeTypeFilterCacheNode struct {
	value *edgeTypeFilterEntry
	key   string
}

// edgeTypeFilterCache is a bounded LRU keyed by a canonicalised
// relationship-type set, caching [buildEdgeTypeFilter]'s O(V+E) result
// across queries so a read-mostly workload pays that cost once per
// (type-set, graph generation) pair rather than once per query execution
// (rmp #1871). Structurally it mirrors [planCache] (map + doubly-linked
// list, O(1) get/put, one sync.Mutex over the structural bookkeeping only)
// with one difference: a key's value is REPLACED across generations rather
// than installed once and kept forever, since the same relationship-type
// combination legitimately needs a fresh filter after every graph mutation.
//
// The O(V+E) rebuild itself always runs OUTSIDE the mutex (see getOrBuild):
// this is not a row-level hot path, but a full graph rebuild is far more
// expensive than a plan-cache lookup, so serialising concurrent rebuilds of
// DIFFERENT (or even the same) key behind one lock would create exactly the
// kind of hot-path contention this project's concurrency mandates forbid.
// A cache miss occurring concurrently on multiple goroutines is handled by
// letting every racer rebuild independently and storing whichever result is
// installed last among those at-or-above the current generation — redundant
// work bounded by the number of concurrently-executing queries missing on
// the same key, never a correctness issue, since every racer's independently
// rebuilt filter is equally valid for the generation it was built against.
//
// edgeTypeFilterCache is safe for concurrent use by any number of
// goroutines.
type edgeTypeFilterCache struct {
	ll  *list.List // *edgeTypeFilterCacheNode, front = most recently used
	by  map[string]*list.Element
	cap int
	mu  sync.Mutex
}

// newEdgeTypeFilterCache constructs an edgeTypeFilterCache with the given
// capacity. A non-positive capacity falls back to
// [DefaultEdgeTypeFilterCacheCapacity] so misconfiguration cannot silently
// disable the bound.
func newEdgeTypeFilterCache(capacity int) *edgeTypeFilterCache {
	if capacity <= 0 {
		capacity = DefaultEdgeTypeFilterCacheCapacity
	}
	return &edgeTypeFilterCache{
		cap: capacity,
		ll:  list.New(),
		by:  make(map[string]*list.Element, capacity),
	}
}

// getOrBuild returns the filter cached under key when it describes exactly the
// graph state at names; otherwise it calls build() (with no lock held, so
// concurrent lookups against other keys — or the graph's write path — are
// never blocked on this rebuild), then installs the fresh result and
// returns it, evicting the least-recently-used entry first if the cache is
// at capacity.
//
// at MUST be the same key the CSR pair passed to build() was stamped with; the
// filter indexes that pair's arc positions and means nothing against another.
func (c *edgeTypeFilterCache) getOrBuild(key string, at csrPairKey, build func() map[uint64]string) map[uint64]string {
	c.mu.Lock()
	if e, ok := c.by[key]; ok {
		//nolint:forcetypeassert // cache invariant: list.Element.Value is always *edgeTypeFilterCacheNode
		node := e.Value.(*edgeTypeFilterCacheNode)
		// The entry pointer is copied out UNDER THE LOCK. Reading node.value
		// after the unlock races a concurrent install (node.value = fresh,
		// below) — `-race` reported exactly that once the read barrier was
		// retired (rmp #2290). The barrier had been hiding it: the epoch this
		// compares against only moves when a writer commits, and a writer used
		// to exclude every reader, so a hit and an install could not overlap.
		// An entry is immutable once built, so holding the pointer is enough.
		v := node.value
		if v.at == at {
			c.ll.MoveToFront(e)
			c.mu.Unlock()
			metrics.IncCounter("cypher.edge_type_filter_cache.hits", 1)
			return v.filter
		}
	}
	c.mu.Unlock()
	metrics.IncCounter("cypher.edge_type_filter_cache.misses", 1)

	filter := build()
	fresh := &edgeTypeFilterEntry{at: at, filter: filter}

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.by[key]; ok {
		//nolint:forcetypeassert // cache invariant
		node := e.Value.(*edgeTypeFilterCacheNode)
		if node.value.at == at || node.value.at.newerThan(at) {
			// A concurrent racer already installed a result at least as
			// fresh as ours: keep theirs, discard the one we just built, but
			// still return OUR result to the caller — it is equally correct for
			// at, just not worth storing twice.
			c.ll.MoveToFront(e)
			return filter
		}
		node.value = fresh
		c.ll.MoveToFront(e)
		return filter
	}
	if c.ll.Len() >= c.cap {
		back := c.ll.Back()
		if back != nil {
			c.ll.Remove(back)
			//nolint:forcetypeassert // cache invariant
			delete(c.by, back.Value.(*edgeTypeFilterCacheNode).key)
			metrics.IncCounter("cypher.edge_type_filter_cache.evictions", 1)
		}
	}
	n := &edgeTypeFilterCacheNode{key: key, value: fresh}
	c.by[key] = c.ll.PushFront(n)
	return filter
}

// Len returns the current number of cached entries. Intended for tests and
// operational introspection.
func (c *edgeTypeFilterCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Capacity returns the configured maximum. Intended for tests and
// operational introspection.
func (c *edgeTypeFilterCache) Capacity() int { return c.cap }
