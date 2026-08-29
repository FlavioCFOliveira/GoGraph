package cypher

import (
	"sync"
	"sync/atomic"

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
	mu  sync.Mutex
	fwd *csr.CSR[float64]
	rev *csr.CSR[float64]
	// col is the slot-aligned relationship-type column for THIS pair (rmp #2251),
	// filled lazily on the first typed query against the pair and dropped with it.
	//
	// It lives here, beside the pair, rather than in a cache of its own because it
	// is a function of exactly the same state: the arc positions it indexes are the
	// pair's, so the pair's key is its key and the pair's invalidation is its
	// invalidation. The per-type-set LRU it replaced had to carry the pair key as a
	// second component to say the same thing (rmp #2293), and had to be consulted
	// through a second mutex on every outer row. Both are gone.
	col   *exec.RelTypeColumn
	key   csrPairKey
	valid bool
}

// newCSRPairCache returns an empty cache.
func newCSRPairCache() *csrPairCache { return &csrPairCache{} }

// get returns the cached pair when it describes exactly the state key names, or
// (nil, nil, false) when the caller must build one.
func (c *csrPairCache) get(key csrPairKey) (fwd, rev *csr.CSR[float64], ok bool) {
	f, r, _, ok := c.getWithColumn(key)
	return f, r, ok
}

// getWithColumn is [csrPairCache.get] also returning the pair's relationship-type
// column, which may be nil because no typed query has needed one yet. Fetching
// both in ONE lookup is the point of storing them together: a typed expand asks
// for its adjacency and its type information in a single mutex acquisition, where
// the retired per-type-set LRU cost it a second one on every outer row.
func (c *csrPairCache) getWithColumn(
	key csrPairKey,
) (fwd, rev *csr.CSR[float64], col *exec.RelTypeColumn, ok bool) {
	if c == nil {
		return nil, nil, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || c.key != key {
		metrics.IncCounter("cypher.csr_pair_cache.misses", 1)
		return nil, nil, nil, false
	}
	metrics.IncCounter("cypher.csr_pair_cache.hits", 1)
	return c.fwd, c.rev, c.col, true
}

// putColumn records col as the relationship-type column of the pair currently
// cached under key, and is a no-op when the entry has since been replaced by a
// different state. Dropping the column in that case is correct and not merely
// convenient: it indexes the arc positions of the pair it was built from, so
// filing it against another pair would mistype relationships rather than merely
// miss them (the rmp #2293 failure mode).
func (c *csrPairCache) putColumn(key csrPairKey, col *exec.RelTypeColumn) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || c.key != key {
		return
	}
	c.col = col
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
	// The column describes the OUTGOING pair's arc positions, so it must not
	// survive the pair it was built for.
	c.fwd, c.rev, c.col, c.key, c.valid = fwd, rev, nil, key, true
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
	if cache == nil || g == nil || viewCarriesOwnWrites(g) {
		if cache == nil {
			csrPairAbsentCacheBuildCount.Add(1)
		}
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

// viewCarriesOwnWrites reports whether g resolves through a WRITE
// transaction's view — one whose snapshot carries the transaction's own id and
// therefore sees its own uncommitted writes ([mvcc.Visible]'s own-id rule).
//
// Such a view must NEITHER be served from NOR stored into any cache shared
// across transactions (rmp #2446, found by the DST multi-session mode): the
// pair it builds embeds its own pending arcs, and a pure reader at the same
// (epoch, startTS) served that pair sees uncommitted topology — CSR positions
// shift and the position-keyed edge-type filter mislands, so committed edges
// lose their types. The converse serve hides the writer's own writes from
// itself. Pure readers carry TxID zero ([lpg.Graph.BeginRead]) and identical
// visibility at identical (epoch, startTS), so their sharing stays sound.
func viewCarriesOwnWrites(g *lpg.ReadView[string, float64]) bool {
	snap := g.Snapshot()
	return snap != nil && snap.TxID() != 0
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
		csrPairAbsentCacheBuildCount.Add(1)
		return csrPairFromGraphAt(g)
	}
	return csrPairCachedAt(bopts.csrPairCache, g)
}

// csrPairAbsentCacheBuildCount counts the subset of [csrPairUncachedBuildCount]
// whose cause is that THERE WAS NO CACHE TO CONSULT — a nil bopts, or a bopts
// whose csrPairCache field was never threaded — as distinct from a cache that was
// consulted and missed, or from the deliberate [viewCarriesOwnWrites] bypass.
//
// The distinction is the whole of the root cause when a rebuild turns out to be
// unamortised, and no timing and no profile can supply it: a per-row rebuild
// caused by an absent cache is a WIRING defect, one caused by misses is an
// INVALIDATION defect, and the two have nothing in common but their symptom.
// Process-global and monotonic, like [csrPairUncachedBuildCount]; bracket a drive
// to read a delta.
var csrPairAbsentCacheBuildCount atomic.Uint64

// csrPairAndColumnCachedFor returns the CSR pair AND the slot-aligned
// relationship-type column describing it, in ONE cache lookup (rmp #2251).
//
// # Why they are fetched together
//
// The column indexes the pair's ABSOLUTE ARC POSITIONS, so it is meaningful
// against that exact pair and no other. Fetching them separately is what made the
// retired per-type-set filter cache need the pair's key as a second key component
// (rmp #2293) and cost a second mutex acquisition on every outer row. Stored
// beside the pair, the column is invalidated by the pair's own replacement and
// costs nothing extra to reach.
//
// The column is built LAZILY: an untyped workload never pays for one. It is built
// OUTSIDE the cache mutex, deliberately — it is an O(V+E) sweep, and serialising
// concurrent queries behind it is exactly the hot-path contention this project's
// concurrency mandates forbid. Two racers may therefore both build one; both
// results are equally valid for the state they were built against, and whichever
// lands last is kept.
func csrPairAndColumnCachedFor(
	bopts *buildOpts, g *lpg.ReadView[string, float64],
) (fwd, rev *csr.CSR[float64], col *exec.RelTypeColumn) {
	var cache *csrPairCache
	if bopts != nil {
		cache = bopts.csrPairCache
	}
	// A write transaction's view sees its own uncommitted writes, so neither the
	// pair NOR the column it describes may be shared across transactions — the same
	// rmp #2446 rule the pair alone already obeyed, and for the same reason: the
	// column is position-keyed, so a reader served a private pair's column would
	// have committed edges mistyped rather than merely missing.
	if cache == nil || g == nil || viewCarriesOwnWrites(g) {
		if cache == nil {
			csrPairAbsentCacheBuildCount.Add(1)
		}
		f, r, _ := csrPairFromGraphAt(g)
		return f, r, buildRelTypeColumn(g, f, r)
	}
	key := csrPairKeyFor(g)
	f, r, c, ok := cache.getWithColumn(key)
	if !ok {
		var built csrPairKey
		f, r, built = csrPairFromGraphAt(g)
		cache.put(built, f, r)
		key, c = built, nil
	}
	if c == nil {
		c = buildRelTypeColumn(g, f, r)
		cache.putColumn(key, c)
	} else {
		metrics.IncCounter("cypher.reltype_column.reuses", 1)
	}
	return f, r, c
}

// expandAdjacencySource returns an [exec.AdjacencySource] that resolves the CSR
// pair AND the relationship-type admission view keyed to it at EXECUTION time
// rather than when the plan is built (rmp #2317).
//
// # Why the type information travels with the pair
//
// The type column is indexed by ABSOLUTE ARC POSITION in the pair it was built
// from. A column built against one CSR is meaningless against another: the
// positions name different edges. Resolving the two separately — the pair at
// execution time and the types at plan-build time — would apply type information
// built for the pre-write topology to the post-write one, which silently MISTYPES
// relationships rather than merely missing them.
//
// # Why the accepted-type set is resolved per Init, not once
//
// The column says what each arc IS; the MASK says what this pattern accepts, and
// it is derived from the graph's label registry, which a preceding clause of the
// same statement may have extended. Resolving it here, beside the adjacency,
// keeps the two describing the same instant. It costs one registry lookup per
// named type and — because [exec.RelTypeAdmit] is a value — no allocation at all
// for any schema whose LabelIDs stay below 64.
func expandAdjacencySource(
	bopts *buildOpts, g *lpg.ReadView[string, float64], relTypes []string,
) exec.AdjacencySource {
	if len(relTypes) == 0 {
		return func() (exec.CSRAdjacency, exec.CSRAdjacency, exec.RelTypeAdmit) {
			fwd, rev := csrPairCachedFor(bopts, g)
			return fwd, rev, exec.RelTypeAdmit{}
		}
	}
	return func() (exec.CSRAdjacency, exec.CSRAdjacency, exec.RelTypeAdmit) {
		fwd, rev, col := csrPairAndColumnCachedFor(bopts, g)
		return fwd, rev, col.Admit(relTypeCodesFor(g, relTypes))
	}
}

// intersectAdjacencySource is [expandAdjacencySource] for the fused cyclic expand,
// which filters two legs and so needs both admission views keyed to the one
// adjacency. Both legs share ONE column — it is type-set independent — so the
// second leg costs only its own mask.
func intersectAdjacencySource(
	bopts *buildOpts, g *lpg.ReadView[string, float64], midRelTypes, endRelTypes []string,
) exec.IntersectAdjacencySource {
	if len(midRelTypes) == 0 && len(endRelTypes) == 0 {
		return func() (exec.CSRAdjacency, exec.CSRAdjacency, exec.RelTypeAdmit, exec.RelTypeAdmit) {
			fwd, rev := csrPairCachedFor(bopts, g)
			return fwd, rev, exec.RelTypeAdmit{}, exec.RelTypeAdmit{}
		}
	}
	return func() (exec.CSRAdjacency, exec.CSRAdjacency, exec.RelTypeAdmit, exec.RelTypeAdmit) {
		fwd, rev, col := csrPairAndColumnCachedFor(bopts, g)
		var mid, end exec.RelTypeAdmit
		if len(midRelTypes) > 0 {
			mid = col.Admit(relTypeCodesFor(g, midRelTypes))
		}
		if len(endRelTypes) > 0 {
			end = col.Admit(relTypeCodesFor(g, endRelTypes))
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
