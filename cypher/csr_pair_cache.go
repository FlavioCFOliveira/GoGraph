package cypher

import (
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
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
// csrPairKey identifies the graph state a cached pair describes.
//
// # Why the epoch alone is not a key under MVCC
//
// It was one before MVCC, when the pair was a pure function of the live
// adjacency and the epoch moved on every change to it. It stopped being one when
// the pair became a function of the reader's INSTANT as well (rmp #2293): two
// readers holding different snapshots need different pairs, and an epoch-only
// key hands each of them the other's — which made a committed edge invisible to
// a reader whose snapshot postdated the commit, and let an older reader traverse
// an arc to a node that did not exist at its instant.
//
// Adding the epoch to startTS would not be enough on its own either, and adding
// startTS to the epoch is what actually closes it. Writes apply EAGERLY and
// publish their commit timestamp afterwards, so the epoch can move before the
// commit is visible: a reader that starts in that window builds a pair without
// the arc and files it under the already-moved epoch, and the next reader — for
// whom the commit has since published — is served it. Two reads with the same
// startTS, in contrast, see the same set of published commits by construction,
// because startTS is drawn from the published instant and only moves when a
// commit publishes.
//
// # Both components, and what each one is for
//
//   - startTS answers WHICH COMMITS the pair includes. It is the load-bearing
//     component for a snapshot reader.
//   - epoch answers which topology the PRESENT-reading build saw, and is the
//     only component a nil-snapshot reader has. It is also kept for snapshot
//     readers, where it is redundant but free, so that a pair can never outlive
//     a topology change that startTS somehow failed to distinguish.
//
// versioned keeps a present read from ever colliding with a snapshot read that
// happens to carry startTS 0 — a legitimate value for a reader that started
// before any commit.
type csrPairKey struct {
	epoch     uint64
	startTS   uint64
	versioned bool
}

// csrPairCache is safe for concurrent use by any number of goroutines.
type csrPairCache struct {
	mu    sync.Mutex
	fwd   *csr.CSR[float64]
	rev   *csr.CSR[float64]
	key   csrPairKey
	valid bool
}

// newCSRPairCache returns an empty cache.
func newCSRPairCache() *csrPairCache { return &csrPairCache{} }

// get returns the cached pair when it describes exactly the state key names, or
// (nil, nil, false) when the caller must build one.
func (c *csrPairCache) get(key csrPairKey) (fwd, rev *csr.CSR[float64], ok bool) {
	if c == nil {
		return nil, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || c.key != key {
		metrics.IncCounter("cypher.csr_pair_cache.misses", 1)
		return nil, nil, false
	}
	metrics.IncCounter("cypher.csr_pair_cache.hits", 1)
	return c.fwd, c.rev, true
}

// put records a pair describing the state key names.
//
// A concurrent builder may have already stored a pair at a LATER instant, in
// which case this call is dropped rather than regressing the entry — both pairs
// are individually valid, so keeping the one more readers will ask for is always
// at least as useful. "Later" compares startTS first and the epoch second,
// because startTS is what a snapshot reader's key turns on; a present read
// (versioned false) carries startTS 0 and so is ordered by its epoch alone,
// which is the only thing that distinguishes two of them.
func (c *csrPairCache) put(key csrPairKey, fwd, rev *csr.CSR[float64]) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.key.newerThan(key) {
		metrics.IncCounter("cypher.csr_pair_cache.stale_puts_dropped", 1)
		return
	}
	if c.valid {
		// Replacing an entry at a superseded state: the equivalent of an eviction,
		// and the signal that tells an operator the graph is being written often
		// enough that the cache is not paying for itself.
		metrics.IncCounter("cypher.csr_pair_cache.replacements", 1)
	}
	c.fwd, c.rev, c.key, c.valid = fwd, rev, key, true
}

// newerThan reports whether k describes a strictly later state than other.
func (k csrPairKey) newerThan(other csrPairKey) bool {
	if k.startTS != other.startTS {
		return k.startTS > other.startTS
	}
	return k.epoch > other.epoch
}

// csrPairCached returns the forward/reverse CSR pair for g, reusing the cached
// pair when the graph's topology has not changed since it was built.
//
// cache may be nil (the public BuildPlanWithMutator path has no Engine behind the
// build), in which case it falls back to an uncached [csrPairFromGraph] — correct,
// just unamortised, exactly as [edgeTypeFilterFor] does.
//
// The key the entry is filed under comes BACK from the build, sampled with it,
// rather than being read here beforehand: a topology change racing the build
// would otherwise have the pair recorded under a state it does not describe.
func csrPairCached(cache *csrPairCache, g *lpg.ReadView[string, float64]) (fwd, rev *csr.CSR[float64]) {
	f, r, _ := csrPairCachedAt(cache, g)
	return f, r
}

// csrPairCachedAt is [csrPairCached] also returning the key the pair it hands
// back describes, for a caller that derives a second structure from the pair and
// must file it under the same state — see [edgeTypeFilterFor].
func csrPairCachedAt(
	cache *csrPairCache, g *lpg.ReadView[string, float64],
) (fwd, rev *csr.CSR[float64], at csrPairKey) {
	if cache == nil || g == nil {
		f, r, built := csrPairFromGraphAt(g)
		return f, r, built
	}
	key := csrPairKeyFor(g)
	if f, r, ok := cache.get(key); ok {
		return f, r, key
	}
	f, r, built := csrPairFromGraphAt(g)
	cache.put(built, f, r)
	return f, r, built
}

// csrPairKeyFor is the key a lookup for g asks about. It must name exactly what
// [csrPairFromGraphAt] stamps a pair with, so the two are kept adjacent.
func csrPairKeyFor(g *lpg.ReadView[string, float64]) csrPairKey {
	key := csrPairKey{epoch: g.TopoGeneration()}
	if snap := g.Snapshot(); snap != nil {
		key.startTS, key.versioned = snap.StartTS(), true
	}
	return key
}

// csrPairCachedFor is [csrPairCached] taking the build options, so call sites need
// no nil dance of their own. A nil bopts (or a bopts with no Engine behind it)
// builds uncached.
func csrPairCachedFor(bopts *buildOpts, g *lpg.ReadView[string, float64]) (fwd, rev *csr.CSR[float64]) {
	f, r, _ := csrPairCachedForAt(bopts, g)
	return f, r
}

// csrPairCachedForAt is [csrPairCachedFor] also returning the key the pair
// describes, for a caller that must file a pair-derived structure under the same
// state.
func csrPairCachedForAt(
	bopts *buildOpts, g *lpg.ReadView[string, float64],
) (fwd, rev *csr.CSR[float64], at csrPairKey) {
	if bopts == nil {
		return csrPairFromGraphAt(g)
	}
	return csrPairCachedAt(bopts.csrPairCache, g)
}

// expandAdjacencySource returns an [exec.AdjacencySource] that resolves the CSR
// pair AND the relationship-type filter keyed to it at EXECUTION time rather than
// when the plan is built (rmp #2317).
//
// # Why the filter travels with the pair
//
// edgeTypeFilter maps ABSOLUTE EDGE POSITIONS in the forward CSR's edges array to
// type names. A filter built against one CSR is meaningless against another: the
// positions name different edges. Resolving the two separately — the pair at
// execution time and the filter at plan-build time — would apply a filter built
// for the pre-write topology to the post-write one, which silently MISTYPES
// relationships rather than merely missing them.
//
// Both come from caches keyed on the same [csrPairKey], so when the topology has
// not moved this is two mutex-guarded hits and no rebuild; when it has moved, both
// rebuild together and stay consistent by construction.
func expandAdjacencySource(
	bopts *buildOpts, g *lpg.ReadView[string, float64], relTypes []string,
) exec.AdjacencySource {
	return func() (exec.CSRAdjacency, exec.CSRAdjacency, map[uint64]string) {
		fwd, rev, at := csrPairCachedForAt(bopts, g)
		if len(relTypes) == 0 {
			return fwd, rev, nil
		}
		return fwd, rev, edgeTypeFilterFor(g, fwd, relTypes, bopts, at)
	}
}

// intersectAdjacencySource is [expandAdjacencySource] for the fused cyclic expand,
// which filters two legs and so needs both filters keyed to the one adjacency.
func intersectAdjacencySource(
	bopts *buildOpts, g *lpg.ReadView[string, float64], midRelTypes, endRelTypes []string,
) exec.IntersectAdjacencySource {
	return func() (exec.CSRAdjacency, exec.CSRAdjacency, map[uint64]string, map[uint64]string) {
		fwd, rev, at := csrPairCachedForAt(bopts, g)
		var mid, end map[uint64]string
		if len(midRelTypes) > 0 {
			mid = edgeTypeFilterFor(g, fwd, midRelTypes, bopts, at)
		}
		if len(endRelTypes) > 0 {
			end = edgeTypeFilterFor(g, fwd, endRelTypes, bopts, at)
		}
		return fwd, rev, mid, end
	}
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
