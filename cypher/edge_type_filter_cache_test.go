package cypher_test

// edge_type_filter_cache_test.go — regression coverage for rmp #1871
// (2026-07-02 production-readiness audit round 2, finding
// "buildEdgeTypeFilter rebuilds O(V+E) whole-graph map on every query
// execution regardless of selectivity").
//
// Background. Every relationship-type-filtered pattern (`-[:TYPE]->`)
// rebuilt its edge-type filter map from a full O(V+E) graph scan on every
// query execution, never amortised across queries. The fix caches the
// filter map keyed by (canonicalised relationship-type set,
// lpg.Graph.TopoGeneration): the cache is valid to reuse exactly as long as
// no edge has been added, removed, or had either undone since the cached
// entry was built.
//
// The critical risk in any such cache is invalidation correctness, not
// hit-rate: the filter's map keys are physical CSR slot POSITIONS, which
// shift for every edge at or after an insertion/deletion point in NodeID
// order. TestEdgeTypeFilterCache_InvalidatesOnPositionShift below
// constructs exactly that shift and proves the cache does not serve a
// stale, now-mismatched position mapping across it.

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// edgeTypeFilterCacheProbe records edge-type-filter-cache hits, misses and
// evictions via the global metrics backend, mirroring cacheProbe's role for
// the plan cache (plan_cache_engine_hit_test.go). NOT parallel-safe:
// installs a global metrics backend.
type edgeTypeFilterCacheProbe struct {
	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

func (p *edgeTypeFilterCacheProbe) IncCounter(name string, delta uint64) {
	switch name {
	case "cypher.edge_type_filter_cache.hits":
		p.hits.Add(delta)
	case "cypher.edge_type_filter_cache.misses":
		p.misses.Add(delta)
	case "cypher.edge_type_filter_cache.evictions":
		p.evictions.Add(delta)
	}
}

func (p *edgeTypeFilterCacheProbe) ObserveLatency(string, time.Duration) {}

func (p *edgeTypeFilterCacheProbe) SetGauge(string, float64) {}

// withEdgeTypeFilterCacheProbe installs a fresh probe, runs fn, restores the
// default no-op backend, then returns the probe for inspection.
func withEdgeTypeFilterCacheProbe(t *testing.T, fn func()) *edgeTypeFilterCacheProbe {
	t.Helper()
	p := &edgeTypeFilterCacheProbe{}
	cmetrics.SetBackend(p)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })
	fn()
	return p
}

func newEdgeTypeFilterEngine(t *testing.T) (*lpg.Graph[string, float64], *cypher.Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	return g, cypher.NewEngine(g)
}

// TestEdgeTypeFilterCache_InvalidatesOnPositionShift is the load-bearing
// correctness proof for this cache. It constructs a graph where a later
// edge addition shifts an EARLIER, already-cached edge's physical CSR
// position, then verifies a repeat query still returns the correct rows —
// proving the epoch-based invalidation, not just an accidental cache-miss
// pattern, is what keeps this correct.
//
// Sequence, by NodeID (assigned in creation order):
//
//	X(0), A(1), B(2), C(3)
//	A -[:LIKES]-> B   (created first; with X still edge-free, this occupies
//	                   forward-CSR position 0 — X's own vertex range is empty)
//
// Query 1: MATCH (n)-[:LIKES]->(m) — populates the LIKES-keyed cache entry
// with {0: "LIKES"} at the pre-mutation topology generation.
//
//	X -[:KNOWS]-> C   (added after; X's NodeID(0) < A's NodeID(1), so in
//	                   forward-CSR order X's edges are enumerated BEFORE A's,
//	                   pushing A's LIKES edge from position 0 to position 1)
//
// Query 2: MATCH (n)-[:LIKES]->(m) again. A cache that failed to invalidate
// on this edge addition would still return {0: "LIKES"} — which the fresh
// CSR now resolves to X's KNOWS edge (a false-positive LIKES match on
// (X,C)) while the real, shifted A-LIKES-B edge at position 1 would be
// silently missing (a false negative). Both failure modes are asserted
// against directly below.
func TestEdgeTypeFilterCache_InvalidatesOnPositionShift(t *testing.T) {
	ctx := context.Background()
	_, eng := newEdgeTypeFilterEngine(t)

	drainRunInTx(t, eng, `CREATE (:P {name: 'X'})`)
	drainRunInTx(t, eng, `CREATE (:P {name: 'A'})`)
	drainRunInTx(t, eng, `CREATE (:P {name: 'B'})`)
	drainRunInTx(t, eng, `CREATE (:P {name: 'C'})`)
	drainRunInTx(t, eng, `MATCH (a:P {name: 'A'}), (b:P {name: 'B'}) CREATE (a)-[:LIKES]->(b)`)

	// Query 1: populates the LIKES cache entry against the pre-shift CSR.
	assertCount(ctx, t, eng, `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n {name: 'A'})-[:LIKES]->(m {name: 'B'}) RETURN count(*) AS n`, 1)

	// Shift: X (NodeID 0, currently edge-free) gains an outgoing edge,
	// pushing every subsequent node's forward-CSR range — including A's —
	// one slot later. This is also an edge addition, so it bumps
	// lpg.Graph.TopoGeneration, which must invalidate the cached LIKES entry.
	drainRunInTx(t, eng, `MATCH (x:P {name: 'X'}), (c:P {name: 'C'}) CREATE (x)-[:KNOWS]->(c)`)

	// Query 2: must still find exactly the real (A,B) LIKES edge — not the
	// stale position-0 mapping now aliased to X's unrelated KNOWS edge, and
	// not miss the real edge at its new position.
	assertCount(ctx, t, eng, `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n {name: 'A'})-[:LIKES]->(m {name: 'B'}) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n {name: 'X'})-[:LIKES]->(m {name: 'C'}) RETURN count(*) AS n`, 0)
	assertCount(ctx, t, eng, `MATCH (n)-[:KNOWS]->(m) RETURN count(*) AS n`, 1)
}

// TestEdgeTypeFilterCache_InvalidatesOnRemoval mirrors the addition-based
// shift test above for a removal: deleting an earlier edge also shifts
// every later edge's CSR position, and must equally invalidate the cache.
func TestEdgeTypeFilterCache_InvalidatesOnRemoval(t *testing.T) {
	ctx := context.Background()
	_, eng := newEdgeTypeFilterEngine(t)

	drainRunInTx(t, eng, `CREATE (:P {name: 'X'})`)
	drainRunInTx(t, eng, `CREATE (:P {name: 'A'})`)
	drainRunInTx(t, eng, `CREATE (:P {name: 'B'})`)
	drainRunInTx(t, eng, `CREATE (:P {name: 'C'})`)
	// X gets an edge FIRST, so A's LIKES edge starts at position 1.
	drainRunInTx(t, eng, `MATCH (x:P {name: 'X'}), (c:P {name: 'C'}) CREATE (x)-[:KNOWS]->(c)`)
	drainRunInTx(t, eng, `MATCH (a:P {name: 'A'}), (b:P {name: 'B'}) CREATE (a)-[:LIKES]->(b)`)

	assertCount(ctx, t, eng, `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`, 1)

	// Remove X's KNOWS edge: A's LIKES edge shifts from position 1 back to
	// position 0. This is an edge removal, so it too bumps TopoGeneration.
	drainRunInTx(t, eng, `MATCH (:P {name: 'X'})-[r:KNOWS]->(:P {name: 'C'}) DELETE r`)

	assertCount(ctx, t, eng, `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n {name: 'A'})-[:LIKES]->(m {name: 'B'}) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n)-[:KNOWS]->(m) RETURN count(*) AS n`, 0)
}

// TestEdgeTypeFilterCache_HitOnRepeatedQuery proves the caching is actually
// occurring (not merely harmless): identical relationship-type filters
// across repeated queries against an unchanged graph must hit, not rebuild.
//
// NOT parallel: installs a global metrics backend.
func TestEdgeTypeFilterCache_HitOnRepeatedQuery(t *testing.T) {
	ctx := context.Background()
	_, eng := newEdgeTypeFilterEngine(t)
	drainRunInTx(t, eng, `CREATE (:P {name: 'A'})-[:LIKES]->(:P {name: 'B'})`)

	const q = `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`
	p := withEdgeTypeFilterCacheProbe(t, func() {
		assertCount(ctx, t, eng, q, 1)
		assertCount(ctx, t, eng, q, 1)
		assertCount(ctx, t, eng, q, 1)
	})

	if got := p.misses.Load(); got != 1 {
		t.Errorf("misses = %d, want exactly 1 (first query only)", got)
	}
	if got := p.hits.Load(); got != 2 {
		t.Errorf("hits = %d, want exactly 2 (second and third query)", got)
	}
}

// TestEdgeTypeFilterCache_MissAfterWrite proves a graph mutation between two
// otherwise-identical queries forces a real rebuild rather than an
// undetected stale hit.
//
// NOT parallel: installs a global metrics backend.
func TestEdgeTypeFilterCache_MissAfterWrite(t *testing.T) {
	ctx := context.Background()
	_, eng := newEdgeTypeFilterEngine(t)
	drainRunInTx(t, eng, `CREATE (:P {name: 'A'})-[:LIKES]->(:P {name: 'B'})`)

	const q = `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`
	p := withEdgeTypeFilterCacheProbe(t, func() {
		assertCount(ctx, t, eng, q, 1)
		drainRunInTx(t, eng, `CREATE (:P {name: 'C'})-[:LIKES]->(:P {name: 'D'})`)
		assertCount(ctx, t, eng, q, 2)
	})

	if got := p.misses.Load(); got != 2 {
		t.Errorf("misses = %d, want exactly 2 (the write must force a second rebuild)", got)
	}
	if got := p.hits.Load(); got != 0 {
		t.Errorf("hits = %d, want 0 (no query repeated without an intervening write)", got)
	}
}

// TestEdgeTypeFilterCache_CapacityOption verifies EngineOptions.
// EdgeTypeFilterCacheCapacity is actually wired to the cache constructor
// rather than silently ignored.
func TestEdgeTypeFilterCache_CapacityOption(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{EdgeTypeFilterCacheCapacity: 1})
	ctx := context.Background()

	drainRunInTx(t, eng, `CREATE (:P {name: 'A'})-[:LIKES]->(:P {name: 'B'})`)
	drainRunInTx(t, eng, `CREATE (:P {name: 'C'})-[:KNOWS]->(:P {name: 'D'})`)

	p := withEdgeTypeFilterCacheProbe(t, func() {
		// Populate LIKES (miss), then KNOWS (miss, evicting LIKES at
		// capacity 1), then LIKES again — must miss again since it was
		// evicted, proving the capacity bound is real and load-bearing.
		assertCount(ctx, t, eng, `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`, 1)
		assertCount(ctx, t, eng, `MATCH (n)-[:KNOWS]->(m) RETURN count(*) AS n`, 1)
		assertCount(ctx, t, eng, `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`, 1)
	})

	if got := p.misses.Load(); got != 3 {
		t.Errorf("misses = %d, want 3 (capacity 1 evicts LIKES before its second query)", got)
	}
	// Capacity 1 evicts on every insertion after the first: installing
	// KNOWS evicts LIKES, then re-installing LIKES evicts KNOWS.
	if got := p.evictions.Load(); got != 2 {
		t.Errorf("evictions = %d, want 2", got)
	}
}

// TestEdgeTypeFilterCache_InvalidatesOnDirectStoreWrite is the store-direct
// counterpart to TestEdgeTypeFilterCache_InvalidatesOnPositionShift: a
// concurrency-architect review of this fix (rmp #1871) found that a caller
// holding the *txn.Store directly (bypassing the Cypher engine's own write
// adapters entirely — [store/txn.Tx.AddEdge]/[store/txn.Tx.Commit], the same
// pattern examples/24_social_network_cli and examples/25_software_house_api
// already use for seeding) could shift an existing edge's forward-CSR
// position without ever bumping [lpg.Graph.TopoGeneration], since only the
// Cypher-facing mutator adapters bumped it. Fixed by also bumping it from
// inside store/txn's own applyOp (via the new [lpg.Graph.BumpTopoGeneration]).
// This test proves the fix: a direct store.Begin/Tx.AddEdge/Tx.Commit
// sequence between two otherwise-identical Cypher queries must not leave the
// second query serving a stale, now-mismatched position mapping.
func TestEdgeTypeFilterCache_InvalidatesOnDirectStoreWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(store)

	// Everything built via the direct store/txn API with explicit keys (not
	// via Cypher, whose auto-generated internal node keys this test cannot
	// predict): X, A, B, C in that creation order, so X's NodeID sorts
	// before A's — A-LIKES->B occupies CSR position 0 while X remains
	// edge-free, exactly as in the Cypher-only position-shift test.
	seed := store.Begin()
	for _, key := range []string{"X", "A", "B", "C"} {
		if err := seed.AddNode(key); err != nil {
			t.Fatalf("seed AddNode(%q): %v", key, err)
		}
		if err := seed.SetNodeLabel(key, "P"); err != nil {
			t.Fatalf("seed SetNodeLabel(%q): %v", key, err)
		}
		// A distinguishing property (not just the label) so a later
		// assertion can pin exactly WHICH pair a filtered match resolves
		// to — a stale, position-mismatched filter and the correct one can
		// both yield a count of 1, just for two DIFFERENT pairs, so a bare
		// count assertion alone cannot tell them apart.
		if err := seed.SetNodeProperty(key, "name", lpg.StringValue(key)); err != nil {
			t.Fatalf("seed SetNodeProperty(%q, name): %v", key, err)
		}
	}
	if err := seed.AddEdge("A", "B", 0); err != nil {
		t.Fatalf("seed AddEdge(A,B): %v", err)
	}
	if err := seed.SetEdgeLabel("A", "B", "LIKES"); err != nil {
		t.Fatalf("seed SetEdgeLabel(A,B,LIKES): %v", err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	// Populate the LIKES cache entry via the Cypher engine, and pin exactly
	// which pair it resolves to BEFORE the shift (so a stale-vs-correct
	// regression below has a known-good baseline to diverge from).
	assertCount(ctx, t, eng, `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n {name: 'A'})-[:LIKES]->(m {name: 'B'}) RETURN count(*) AS n`, 1)

	// Direct store write, entirely bypassing the Cypher engine: X (NodeID 0,
	// still edge-free) gains an outgoing edge, shifting A's LIKES edge from
	// CSR position 0 to position 1 — the same shift the Cypher-only test
	// performs via a CREATE statement, but this time via the lower-level
	// store/txn API a Cypher Engine does not observe through its own
	// mutator adapters at all.
	tx := store.Begin()
	if err := tx.AddEdge("X", "C", 0); err != nil {
		t.Fatalf("direct Tx.AddEdge(X,C): %v", err)
	}
	if err := tx.SetEdgeLabel("X", "C", "KNOWS"); err != nil {
		t.Fatalf("direct Tx.SetEdgeLabel(X,C,KNOWS): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("direct Tx.Commit: %v", err)
	}

	// The repeat query must still find exactly the real (A,B) LIKES edge —
	// NOT a stale position-0 mapping now aliased to X's unrelated new
	// (X,C) edge (which would make this a match on (X,C) instead), and not
	// miss the real edge at its new position (a false 0-count). A bare
	// count==1 assertion alone cannot distinguish these — both the correct
	// and the stale-wrong outcome yield exactly one row, just for a
	// different pair — which is exactly why this test failed to catch the
	// bug in an earlier draft that checked only the count.
	assertCount(ctx, t, eng, `MATCH (n)-[:LIKES]->(m) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n {name: 'A'})-[:LIKES]->(m {name: 'B'}) RETURN count(*) AS n`, 1)
	assertCount(ctx, t, eng, `MATCH (n {name: 'X'})-[:LIKES]->(m {name: 'C'}) RETURN count(*) AS n`, 0)
	assertCount(ctx, t, eng, `MATCH (n)-[:KNOWS]->(m) RETURN count(*) AS n`, 1)
}
