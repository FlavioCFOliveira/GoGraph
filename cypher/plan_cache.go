package cypher

import (
	"container/list"
	"hash/maphash"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// DefaultPlanCacheCapacity is the default upper bound on the TOTAL number
// of entries held by an [Engine]'s plan cache, across every shard.
// Chosen so that a typical OLTP workload — the same hundreds of queries
// reissued by connection pools and ORMs — stays entirely in-cache without
// unbounded growth under high query-text churn (parameter-baked queries,
// ad-hoc analytics, fuzzed input).
//
// Configure a different capacity via [EngineOptions.PlanCacheCapacity];
// pass 0 to use the default, or a positive integer to override. A
// negative value is clamped to the default by the constructor.
const DefaultPlanCacheCapacity = 1024

// planCacheShards is the number of independently-locked shards a
// [planCache] is divided into, before the clamp in [newPlanCacheWithShards]
// reduces it to fit a small capacity.
//
// It is measured on two OPPOSED axes, and it is the minimum of their sum
// rather than the best point on either. Contention falls monotonically with
// the count and has no knee, so that axis alone would choose the largest
// count measured. Hit rate falls with it too, and costs far more: dividing
// one bound into N fixed per-shard bounds lets an uneven hash overfill a
// shard while its neighbours sit part-empty, and a miss is a full recompile
// (~1.8 us) against a hit of tens of nanoseconds, so one lost point of hit
// rate outweighs a doubling of the count.
//
// At 16, 78% of the whole available contention reduction is bought for at
// most half a point of hit rate at working sets up to 75% of capacity, and
// for nothing at all at or below half capacity. Both ladders, the noise
// floor they were read against, and the price of every rung are in
// docs/benchmarks/cypher-plan-cache-shard-2026-09-16-2851-2852/shard-count.md;
// they are deliberately NOT restated here, because a number copied into two
// places drifts. This constant is the only thing that has to change to move
// the decision.
const planCacheShards = 16

// planCacheShardPadBytes pads [planCacheShard] out to a whole cache line so
// that two shards locked by two cores never write to the same line. The
// fields occupy 32 bytes on a 64-bit build (three words plus an 8-byte
// sync.Mutex), so 96 bytes of padding take the struct to 128 — the LARGER of
// the two line sizes Go assumes for this module's shipped targets
// (internal/cpu.CacheLinePadSize is 128 on arm64 and 64 on amd64), so one
// constant is safe on both. TestPlanCacheShard_IsCacheLineSized pins the
// arithmetic, which is what makes the hard-coded 96 safe: adding a field
// fails that test instead of silently overflowing the struct to 192 bytes.
const planCacheShardPadBytes = 96

// planCacheNode pairs a cache key with its entry so the doubly-
// linked list maintained by a [planCacheShard] can walk back from a list
// element to the map key for eviction.
type planCacheNode struct {
	value *planCacheEntry
	key   string
}

// planCacheShard is one independently-locked slice of a [planCache]: its
// own mutex, its own LRU list, its own map and its own capacity bound.
// Nothing is shared with any other shard, so two lookups whose keys hash
// to different shards never touch the same memory.
type planCacheShard struct {
	ll  *list.List // *planCacheNode, front = most recently used
	by  map[string]*list.Element
	cap int
	mu  sync.Mutex // guards ll and by; cap is written only at construction
	_   [planCacheShardPadBytes]byte
}

// planCache is a bounded LRU keyed by query text, SHARDED by a hash of that
// key. Each shard is the classic map + doubly-linked-list pair — O(1)
// get/loadOrStore — behind its own mutex, so N shards admit N concurrent
// lookups of keys that hash apart. The mutex is held only across the
// map/list manipulation. Every field of the cached *planCacheEntry that the
// cache itself publishes is written before [planCache.loadOrStore] installs it
// and never again, so callers operate on the returned pointer without any
// further synchronisation; the two memos filled lazily on it afterwards
// ([planCacheEntry.scalarUse] and [planCacheEntry.countVarRewrite]) are NOT
// covered by that and carry their own.
//
// # Why it is sharded
//
// A single mutex across the whole cache made the warm lookup SLOWER with
// more cores, not faster: [Engine.parseAndAnalyse] on a warm cache measured
// 15.04 ns/op at -cpu=1 and 92.14 ns/op at -cpu=10 (66.5 M/s down to
// 10.9 M/s — ten cores delivering 16% of one core's throughput), with the
// mutex profile charging 98.46% of 19.75 s of delay to (*planCache).get.
// The path is on EVERY statement the engine executes. Evidence:
// docs/benchmarks/cypher-parse-lab-2026-09-16-spike2846/ and
// docs/benchmarks/cypher-plan-cache-shard-2026-09-16-2851-2852/.
//
// # Eviction is PER SHARD — the accepted consequence
//
// Sharding moves the LRU order from the cache to the shard. There is no
// longer one global recency order and no single least-recently-used entry:
// an insert that overflows a shard evicts THAT SHARD's least-recently-used
// entry, which may be more recently used than an entry sitting in another
// shard. A key that would have survived under one global list can therefore
// be evicted, and vice versa.
//
// This is a deliberate trade, taken with the hit/miss semantics unchanged:
// a key that is present is still found, a key that is absent still misses,
// and the plan built for a key is unchanged. Only WHICH entry is dropped at
// capacity moves. The cache remains a cache — correctness never depends on
// an entry surviving, because a miss simply recompiles.
//
// # The total bound is exact
//
// [newPlanCacheWithShards] divides the requested capacity across the shards
// so the per-shard bounds SUM to exactly the requested total: with capacity
// C over N shards, C%N shards hold ⌈C/N⌉ entries and the rest hold ⌊C/N⌋.
// Each shard enforces its own bound under its own lock, so the total entry
// count can never exceed C, and [planCache.Capacity] reports C.
// The shard count is clamped to the largest power of two not exceeding the
// capacity, so no shard is ever given a bound of zero — a zero-bound shard
// would silently make every key hashing to it permanently uncacheable.
//
// # Metrics
//
// Hit, miss, eviction and invalidation events are reported via the
// global metrics surface under the names:
//
//   - cypher.plan_cache.hits
//   - cypher.plan_cache.misses
//   - cypher.plan_cache.evictions
//   - cypher.plan_cache.invalidations
//
// On the default no-op metrics backend the cost is two atomic loads
// per event. The events are emitted OUTSIDE the shard mutex: the counter
// is still incremented exactly once per lookup, but the work no longer
// lengthens a critical section every caller queues behind.
//
// planCache is safe for concurrent use by any number of goroutines.
type planCache struct {
	shards []planCacheShard
	seed   maphash.Seed
	mask   uint64
	cap    int
}

// newPlanCache constructs a planCache with the given capacity and the
// measured default shard count. A non-positive capacity falls back to
// [DefaultPlanCacheCapacity] so misconfiguration cannot silently disable
// the bound.
func newPlanCache(capacity int) *planCache {
	return newPlanCacheWithShards(capacity, planCacheShards)
}

// newPlanCacheWithShards constructs a planCache with an explicit shard
// count. It exists so the shard-count ladder and the eviction-contract
// tests can pin a count; production goes through [newPlanCache].
//
// The effective shard count is the largest power of two that is at most
// both shards and capacity — a power of two so the shard index is a mask
// rather than a division, and at most capacity so every shard is given a
// bound of at least one entry.
func newPlanCacheWithShards(capacity, shards int) *planCache {
	if capacity <= 0 {
		capacity = DefaultPlanCacheCapacity
	}
	if shards < 1 {
		shards = 1
	}
	n, limit := 1, min(shards, capacity)
	for n*2 <= limit {
		n *= 2
	}
	c := &planCache{
		shards: make([]planCacheShard, n),
		seed:   maphash.MakeSeed(),
		mask:   uint64(n - 1),
		cap:    capacity,
	}
	// Divide the total bound so the per-shard bounds sum to it EXACTLY:
	// the first capacity%n shards carry one extra entry.
	base, rem := capacity/n, capacity%n
	for i := range c.shards {
		s := &c.shards[i]
		s.cap = base
		if i < rem {
			s.cap++
		}
		s.ll = list.New()
		s.by = make(map[string]*list.Element, s.cap)
	}
	return c
}

// shardFor returns the shard that owns key. The seed is per-cache and drawn
// at construction, so the shard a given query text lands in is not
// predictable from outside the process — an attacker cannot craft a set of
// query texts that all collide on one shard and evict a hot plan from it.
func (c *planCache) shardFor(key string) *planCacheShard {
	return &c.shards[maphash.String(c.seed, key)&c.mask]
}

// get returns the cached entry for key and promotes it to the front of its
// SHARD's LRU list. It returns (nil, false) on a miss.
func (c *planCache) get(key string) (*planCacheEntry, bool) {
	s := c.shardFor(key)
	s.mu.Lock()
	e, ok := s.by[key]
	var v *planCacheEntry
	if ok {
		s.ll.MoveToFront(e)
		//nolint:forcetypeassert // cache invariant: list.Element.Value is always *planCacheNode
		v = e.Value.(*planCacheNode).value
	}
	s.mu.Unlock()
	if ok {
		metrics.IncCounter("cypher.plan_cache.hits", 1)
		return v, true
	}
	metrics.IncCounter("cypher.plan_cache.misses", 1)
	return nil, false
}

// loadOrStore returns the cached entry for key when present, promoting it to
// the front of its shard's LRU list, or installs entry as the new
// most-recently-used value of key's shard. The bool result mirrors
// sync.Map's LoadOrStore semantics: true when a previously-installed entry
// was returned, false when the supplied entry was installed.
//
// When installing forces eviction, the least-recently-used entry OF THAT
// SHARD is dropped and metrics.cypher.plan_cache.evictions is incremented.
// See the eviction contract on [planCache].
func (c *planCache) loadOrStore(key string, entry *planCacheEntry) (*planCacheEntry, bool) {
	s := c.shardFor(key)
	s.mu.Lock()
	if e, ok := s.by[key]; ok {
		s.ll.MoveToFront(e)
		//nolint:forcetypeassert // cache invariant
		v := e.Value.(*planCacheNode).value
		s.mu.Unlock()
		return v, true
	}
	evicted := false
	if s.ll.Len() >= s.cap {
		if back := s.ll.Back(); back != nil {
			s.ll.Remove(back)
			//nolint:forcetypeassert // cache invariant
			delete(s.by, back.Value.(*planCacheNode).key)
			evicted = true
		}
	}
	n := &planCacheNode{key: key, value: entry}
	s.by[key] = s.ll.PushFront(n)
	s.mu.Unlock()
	if evicted {
		metrics.IncCounter("cypher.plan_cache.evictions", 1)
	}
	return entry, false
}

// clear removes every cached entry and resets every shard's LRU list. It is
// invoked by the DDL operators (CREATE/DROP INDEX, CREATE/DROP CONSTRAINT)
// via [Engine.ClearPlanCache] whenever the schema mutates so that subsequent
// queries re-plan against the new schema.
//
// clear emits cypher.plan_cache.invalidations exactly once per call —
// even when the cache is already empty — so callers can drive the
// metric unconditionally without first inspecting [planCache.Len].
//
// EVERY shard mutex is held across the whole reset, and the reset is split
// into three loops on purpose: acquire all, then reset all, then release all.
// Acquiring mutates nothing, so a concurrent [planCache.get] or
// [planCache.loadOrStore] sees either its shard before ANY shard was reset or
// its shard after EVERY shard was reset — never a cache half cleared. Fusing
// the reset into the acquisition loop would destroy that property.
//
// [planCache.Len] is not such an observer and makes no such claim: it takes
// one shard lock at a time, so a clear running beside it can leave it
// returning a total no instant ever held. It is introspection, and its own
// contract says so.
//
// The order is ascending, but what actually makes the fixed order safe is that
// a shard mutex is a LEAF: nothing inside any shard critical section acquires
// another lock, blocks, or calls out to caller-supplied code — the metrics
// events are emitted outside. No other path holds two shard mutexes.
//
// The list reset and the map clear must stay inside ONE lock hold per shard.
// [list.List.Init] leaves every old element still pointing at this list, so an
// element that outlived the reset would pass MoveToFront's ownership guard and
// corrupt the length; deleting the map keys under the same hold is what makes
// every such element unreachable.
func (c *planCache) clear() {
	for i := range c.shards {
		c.shards[i].mu.Lock()
	}
	for i := range c.shards {
		s := &c.shards[i]
		s.ll.Init()
		clear(s.by)
	}
	for i := range c.shards {
		c.shards[i].mu.Unlock()
	}
	metrics.IncCounter("cypher.plan_cache.invalidations", 1)
}

// Len returns the current number of cached entries across every shard.
// Intended for tests and operational introspection.
//
// The shards are counted one at a time, each under its own mutex, so the
// result is not an instantaneous snapshot of the whole cache when writers
// are concurrently active. It is nonetheless a sound BOUND: each shard's
// count never exceeds that shard's own capacity, so any sum of per-shard
// reads is at most [planCache.Capacity].
func (c *planCache) Len() int {
	n := 0
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		n += s.ll.Len()
		s.mu.Unlock()
	}
	return n
}

// Capacity returns the configured maximum TOTAL number of entries, summed
// over every shard. Intended for tests and operational introspection.
func (c *planCache) Capacity() int { return c.cap }

// Shards returns the effective number of shards. Intended for tests and
// operational introspection.
func (c *planCache) Shards() int { return len(c.shards) }

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
