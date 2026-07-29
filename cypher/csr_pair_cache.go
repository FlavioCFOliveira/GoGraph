package cypher

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
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
// # Memory
//
// The cache holds at most ONE pair per Engine — replaced wholesale when the epoch
// advances, so the previous pair becomes garbage immediately. That is a fixed
// bound of two CSR snapshots, which is the same peak an uncached build already
// reached transiently while the old pair was still referenced by an in-flight
// query. No new unbounded resource is introduced.
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
		return nil, nil, false
	}
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
		return
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
