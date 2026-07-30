package cypher

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// csr_pair_cache.go — cross-query reuse of the forward/reverse CSR pair
// (rmp #2143).
//
// [csrPairFromGraph] builds a forward CSR and its reverse transpose, an O(V+E)
// pass, and its doc used to note that this "happens at most once per query" — that
// is, once per query, EVERY query. On a read-mostly workload repeating a query
// against an unchanged graph that rebuild is pure waste, and it is measurable: on
// the 960k-edge cypher_scale fixture the pair build is roughly a third of a warm,
// highly selective one-hop query's wall time.
//
// It became worth fixing rather than tolerating when #2141 made the build order
// each source's neighbour run. Ordering costs about 4.2 ms on that fixture, which
// a forward-only one-hop query pays and uses none of — it needs no O(log d) probe
// at all. Caching removes the whole per-query build for every query shape rather
// than special-casing which ones benefit from the ordering.
//
// # Why TopoGeneration is the right key, and what had to change for it to be
//
// A CSR is a pure function of the graph's LIVE edge topology, so any change to
// that topology must invalidate the entry, and nothing else needs to.
// [lpg.Graph.TopoGeneration] is documented as exactly that epoch — the counter
// "any CSR-position-keyed cache" must watch — and the write path already bumps it
// on every edge add and remove.
//
// It was NOT sufficient on its own: [lpg.Graph.RemoveNode] tombstoned a node
// without bumping it, and csrPairFromGraph builds LIVE-FILTERED (it omits arcs
// incident to a tombstoned node, #1790). A tombstone between two queries would
// therefore have left a cached pair describing arcs that are no longer live —
// ghost edges, the precise defect the live filter exists to prevent. RemoveNode
// and revive now both bump the generation, which also makes the key sound against
// an ABA hazard the tombstone COUNT would have had: removing one node and
// reviving another leaves the count unchanged while the live set differs.
//
// Node properties and labels do not affect topology and correctly do not
// invalidate.
//
// # A cached pair can be NARROWER than the live node space
//
// Interning a fresh node does not change edge topology, so a bare `CREATE (z:Z)`
// correctly does not invalidate. The consequence is new and load-bearing: a cache
// HIT can therefore return a pair whose `vertices` array is shorter than the
// current node space — measured, 600 node-only creates left a cached pair with 257
// offsets against a live space of 1793. Before this cache existed the pair was
// always rebuilt, so it could not lag.
//
// This is SAFE only because every consumer bounds-checks the vertex index before
// indexing the offsets array — Expand's probes and range load, ShortestPath and its
// bidirectional variant, buildRevToFwd, buildEdgeTypeFilter (which additionally
// bounds its loop on fwdCSR.MaxNodeID()), and ExpandIntersect via its own
// outRange/inRange guards. A node with no edges has no arcs to traverse, so
// treating it as absent from the pair is the correct answer, not an
// approximation. Any NEW consumer that indexes `vertices` unguarded would be a
// latent out-of-range panic; TestCSRPairCache_NodeOnlyCreateStaysSafe pins it.
//
// Keep this list current when a consumer is added — ExpandIntersect was missing
// from it for a whole sprint (rmp #2261). It guards correctly, so nothing was
// broken, but the list is what the next reader will trust when deciding whether a
// new consumer needs a guard.
//
// # Memory
//
// The cache holds at most ONE pair per Engine — replaced wholesale when the epoch
// advances, so the previous pair becomes garbage immediately. The bound is fixed,
// but be precise about what it costs, because two easy claims are both wrong:
//
//   - it is a steady-state FLOOR, not a transient. An uncached build's snapshots
//     became garbage when the query ended; a cached pair stays reachable for the
//     Engine's lifetime. Measured at 20k edges, a warm Engine retains about
//     2.4 MB more than a cold one (~121 B/edge, including the pre-existing filter
//     cache);
//   - the PEAK is higher than uncached, not equal: on an alternating write/read
//     workload the retained pair is still reachable while the next pair is being
//     built, so four snapshots can be live at once against an uncached two.
//
// Roughly (V+1)·8 + E·24 bytes per direction. No EngineOptions result-memory
// ceiling covers it (those bound result and aggregation memory), so
// [EngineOptions.DisableCSRPairCache] exists for an Engine that is long-lived over
// a large graph and would rather pay the rebuild than the retention.
//
// csrPairCache is safe for concurrent use by any number of goroutines.
type csrPairCache struct {
	mu    sync.Mutex
	fwd   *csr.CSR[float64]
	rev   *csr.CSR[float64]
	epoch uint64
	valid bool
}

// newCSRPairCache returns an empty cache.
func newCSRPairCache() *csrPairCache { return &csrPairCache{} }

// get returns the cached pair when it was built at the graph's current
// topology epoch, or (nil, nil, false) when the caller must build one.
func (c *csrPairCache) get(epoch uint64) (fwd, rev *csr.CSR[float64], ok bool) {
	if c == nil {
		return nil, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || c.epoch != epoch {
		metrics.IncCounter("cypher.csr_pair_cache.misses", 1)
		return nil, nil, false
	}
	metrics.IncCounter("cypher.csr_pair_cache.hits", 1)
	return c.fwd, c.rev, true
}

// put records a pair built at epoch. A concurrent builder may have already
// stored a NEWER epoch, in which case this call is dropped rather than
// regressing the entry — both pairs are individually valid, so keeping the
// newer one is always at least as correct.
func (c *csrPairCache) put(epoch uint64, fwd, rev *csr.CSR[float64]) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.epoch > epoch {
		metrics.IncCounter("cypher.csr_pair_cache.stale_puts_dropped", 1)
		return
	}
	if c.valid {
		// Replacing an entry at a superseded epoch: the equivalent of an eviction,
		// and the signal that tells an operator the graph is being written often
		// enough that the cache is not paying for itself.
		metrics.IncCounter("cypher.csr_pair_cache.replacements", 1)
	}
	c.fwd, c.rev, c.epoch, c.valid = fwd, rev, epoch, true
}

// csrPairCached returns the forward/reverse CSR pair for g, reusing the cached
// pair when the graph's topology has not changed since it was built.
//
// cache may be nil (the public BuildPlanWithMutator path has no Engine behind the
// build), in which case it falls back to an uncached [csrPairFromGraph] — correct,
// just unamortised, exactly as [edgeTypeFilterFor] does.
//
// The epoch is read BEFORE the build so a topology change racing the build cannot
// be recorded under the newer epoch: the resulting entry is then stamped with the
// older epoch and the next caller rebuilds. Reading it after would risk caching a
// pair that predates the epoch it claims.
func csrPairCached(cache *csrPairCache, g *lpg.Graph[string, float64]) (fwd, rev *csr.CSR[float64]) {
	if cache == nil || g == nil {
		return csrPairFromGraph(g)
	}
	epoch := g.TopoGeneration()
	if f, r, ok := cache.get(epoch); ok {
		return f, r
	}
	f, r := csrPairFromGraph(g)
	cache.put(epoch, f, r)
	return f, r
}

// csrPairCachedFor is [csrPairCached] taking the build options, so call sites need
// no nil dance of their own. A nil bopts (or a bopts with no Engine behind it)
// builds uncached.
func csrPairCachedFor(bopts *buildOpts, g *lpg.Graph[string, float64]) (fwd, rev *csr.CSR[float64]) {
	if bopts == nil {
		return csrPairFromGraph(g)
	}
	return csrPairCached(bopts.csrPairCache, g)
}

// newCSRPairCacheIfEnabled returns a cache unless the Engine opted out via
// [EngineOptions.DisableCSRPairCache], in which case it returns nil and every
// lookup falls through to an uncached build — the same behaviour the public
// BuildPlanWithMutator path already has.
func newCSRPairCacheIfEnabled(disabled bool) *csrPairCache {
	if disabled {
		return nil
	}
	return newCSRPairCache()
}
