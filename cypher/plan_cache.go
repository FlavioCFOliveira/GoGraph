package cypher

import (
	"container/list"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
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
	value *planCacheEntry
	key   string
}

// planCache is a bounded LRU keyed by query text. The implementation
// is the classic map + doubly-linked-list pair: O(1) Get/Put, single
// sync.Mutex serialising the structural updates. The mutex is held
// only across the map/list manipulation; the cached *planCacheEntry
// itself is immutable once published, so callers operate on the
// returned pointer without any further synchronisation.
//
// Hit, miss, eviction and invalidation events are reported via the
// global [metrics] surface under the names:
//
//   - cypher.plan_cache.hits
//   - cypher.plan_cache.misses
//   - cypher.plan_cache.evictions
//   - cypher.plan_cache.invalidations
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
	ll  *list.List // *planCacheNode, front = most recently used
	by  map[string]*list.Element
	cap int
	mu  sync.Mutex
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

// clear removes every cached entry and resets the LRU list. It is
// invoked by the DDL operators (CREATE/DROP INDEX, CREATE/DROP
// CONSTRAINT) via [Engine.ClearPlanCache] whenever the schema mutates
// so that subsequent queries re-plan against the new schema.
//
// clear emits cypher.plan_cache.invalidations exactly once per call —
// even when the cache is already empty — so callers can drive the
// metric unconditionally without first inspecting [planCache.Len].
//
// The mutex is held across the structural reset; concurrent get /
// loadOrStore observers will either complete fully before the clear or
// see a fully reset cache afterwards, consistent with the rest of the
// LRU contract.
func (c *planCache) clear() {
	c.mu.Lock()
	c.ll.Init()
	for k := range c.by {
		delete(c.by, k)
	}
	c.mu.Unlock()
	metrics.IncCounter("cypher.plan_cache.invalidations", 1)
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

// mergeAutoParams returns params extended with the auto-parameters
// [parser.StripLiterals] hoisted out of the query text.
//
// The caller's map is never mutated: it belongs to the caller, may be reused
// across executions, and may be read concurrently. When there is nothing to
// merge the original map is returned unchanged, so the common path allocates
// nothing.
//
// Auto-parameter names contain spaces and are backtick-quoted in the rewritten
// text, so they cannot collide with a user parameter.
func mergeAutoParams(params map[string]expr.Value, auto map[string]string) map[string]expr.Value {
	if len(auto) == 0 {
		return params
	}
	out := make(map[string]expr.Value, len(params)+len(auto))
	for k, v := range params {
		out[k] = v
	}
	for k, v := range auto {
		out[k] = expr.StringValue(v)
	}
	return out
}

// planBuild is one in-flight compilation of a single cache key. done is
// closed exactly once, by the goroutine that performed the build; entry and
// err are written before the close and read only after it, so the close is the
// happens-before edge that publishes them.
type planBuild struct {
	done  chan struct{}
	entry *planCacheEntry
	err   error
}

// planBuildGroup collapses the concurrent compilations of one cache key into a
// single compilation whose result every caller shares.
//
// # Why it exists
//
// The plan cache absorbs a repeated query completely — but only from the
// SECOND execution onwards, and only after the first has published its entry.
// Until then every concurrent caller misses, and every one of them compiles.
// Measured on bench/contention at rmp #2740, the miss count was exactly the
// concurrency level at every rung of the ladder (8, 64, 128, 192, 256, 384,
// 512, 1024): each goroutine misses once, on its first call, and hits forever
// after. So N goroutines released together perform N redundant compilations of
// the same text.
//
// That is expensive twice over. The obvious cost is N-1 wasted parses. The
// large one is that they are not independent: the ANTLR parser and lexer share
// ONE ATN per grammar for the whole process (gen.CypherParserParserStaticData
// and gen.CypherLexerLexerStaticData are package-level), and the ATN's
// stateMu, edgeMu and mu are taken — the first two in WRITE mode — while the
// DFA for a not-yet-seen token path is filled in. N concurrent first parses
// therefore serialise on a process-global mutex. On cypher-read-scan-large at
// level 1024, over five interleaved replicas of bench/contention, the median
// was 762.68 s of mutex delay inside parser.ParseStatement out of 762.90 s in
// the whole process. Publishing the entry before the same workload ran — the
// single-variable control — took the parse-path figure to exactly zero.
//
// # Prior art
//
// Neo4j reaches the same conclusion for the same topology — one shared plan
// cache behind many concurrent clients. Its query and execution-plan caches go
// through LFUCache.computeIfAbsent (neo4j/neo4j, branch dev,
// community/cypher/cypher-cache/src/main/scala/org/neo4j/cypher/internal/cache/LFUCache.scala),
// which is a single call to Caffeine's Cache.get(key, mappingFunction) rather
// than a get / compute / put sequence. That method's contract (ben-manes/caffeine,
// tag v3.1.8, caffeine/src/main/java/com/github/benmanes/caffeine/cache/Cache.java)
// is "The entire method invocation is performed atomically, so the function is
// applied at most once per key", with other callers blocked while it runs.
//
// PostgreSQL takes the other route and is worth recording because it does NOT
// transfer: its plan cache is per-backend — "the backend's list of 'saved'
// CachedPlanSources" (postgres/postgres, REL_16_STABLE,
// src/backend/utils/cache/plancache.c) — so a process-wide stampede cannot
// arise in the first place. GoGraph is embedded and shares one Engine across
// every goroutine, which is Neo4j's shape, not PostgreSQL's.
//
// # What it does and does not change
//
// It changes WHO compiles, never WHAT is compiled. The build is a pure
// function of the query text and the index schema, and the entry it produces
// is immutable once published, which is already why the LRU may hand the same
// pointer to every later caller. Sharing it with the callers that raced the
// first one is the same sharing, one execution earlier. No openCypher-visible
// behaviour depends on which goroutine ran the parser.
//
// The group is NOT a cache: an entry lives in the map only while its build is
// running, and is removed as soon as it finishes, whether it succeeded or
// failed. Failures are therefore not cached, exactly as before. The map's size
// is bounded by the number of DISTINCT query texts being compiled at one
// instant, which is bounded in turn by the number of goroutines executing a
// query — the same bound the caller already imposes, and far smaller than the
// N simultaneous parse trees the unshared path allocated.
//
// planBuildGroup is safe for concurrent use by any number of goroutines.
type planBuildGroup struct {
	inflight map[string]*planBuild
	mu       sync.Mutex
}

// newPlanBuildGroup constructs an empty group.
func newPlanBuildGroup() *planBuildGroup {
	return &planBuildGroup{inflight: make(map[string]*planBuild)}
}

// do returns the result of build(key), performing at most one build per key
// per instant: the first caller runs build while every caller that arrives
// before it finishes waits and receives that same result.
//
// g.mu guards only the in-flight map and is never held across build, so a
// compilation of one query never delays a lookup — or a build — of another.
//
// If build panics, the deferred close still releases the waiters, which then
// see the zero (nil, nil) result and build for themselves rather than
// deadlocking. build never returns (nil, nil) on any non-panicking path, so
// that pair is unambiguous as a signal. The panic itself is not recovered:
// panics indicate programmer error and must surface.
func (g *planBuildGroup) do(key string, build func(string) (*planCacheEntry, error)) (*planCacheEntry, error) {
	g.mu.Lock()
	if b, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		<-b.done
		if b.entry == nil && b.err == nil {
			return build(key) // leader panicked; do not inherit its silence
		}
		return b.entry, b.err
	}
	b := &planBuild{done: make(chan struct{})}
	g.inflight[key] = b
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.inflight, key)
		g.mu.Unlock()
		close(b.done)
	}()

	b.entry, b.err = build(key)
	return b.entry, b.err
}
