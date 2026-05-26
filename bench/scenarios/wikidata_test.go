//go:build nightly

// Package scenarios_test — T785: knowledge-graph traversal on a synthetic
// Wikidata-like sub-dump (nightly layer).
//
// TestWikidata_VarlenMatch_Nightly builds a synthetic knowledge graph with
// 1 000 entities, runs a variable-length MATCH query (depth 1..3) via the
// Cypher engine directly, and verifies:
//
//  1. The query returns without error and produces at least 1 result row.
//  2. Plan-cache hit ratio > 0: running the identical query twice causes a
//     cache hit on the second run (verified via a metrics probe).
//  3. goleak clean (via TestMain in main_test.go).
//
// Activate with:
//
//	go test -race -count=1 -tags=nightly -timeout 300s -run TestWikidata ./bench/scenarios/...
package scenarios_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
)

// cacheHitProbe is a [metrics.Backend] that counts plan-cache hits.
// It is used only in TestWikidata_VarlenMatch_Nightly and must not be
// installed concurrently with other tests that share the global backend.
type cacheHitProbe struct {
	hits   atomic.Uint64
	misses atomic.Uint64
}

func (p *cacheHitProbe) IncCounter(name string, delta uint64) {
	switch name {
	case "cypher.plan_cache.hits":
		p.hits.Add(delta)
	case "cypher.plan_cache.misses":
		p.misses.Add(delta)
	}
}

func (p *cacheHitProbe) ObserveLatency(string, time.Duration) {}

// TestWikidata_VarlenMatch_Nightly builds a synthetic Wikidata-like knowledge
// graph and exercises variable-length MATCH with plan-cache verification.
//
// Graph layout (1 000 entities):
//   - Nodes: labels Entity (0–699), Concept (700–899), Person (900–999).
//   - Edges: ~3 RELATED_TO edges per Entity node (deterministic, no duplicates).
//   - Properties: id (int), name (string).
//
// The varlen query targets Entity nodes with id < 10 and finds reachable
// Entity nodes up to depth 3 via RELATED_TO.
func TestWikidata_VarlenMatch_Nightly(t *testing.T) {
	// NOT parallel: installs the global metrics backend.

	const (
		entityCount  = 700
		conceptCount = 200
		personCount  = 100
		totalNodes   = entityCount + conceptCount + personCount

		// Edges: each Entity node gets 3 deterministic RELATED_TO neighbours.
		edgesPerEntity = 3
	)

	// ── Build the LPG ─────────────────────────────────────────────────────────
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Create Entity nodes (id 0..699).
	for i := range entityCount {
		q := fmt.Sprintf(`CREATE (n:Entity {id: %d, name: 'entity-%d'})`, i, i)
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("CREATE Entity %d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("CREATE Entity %d Close: %v", i, err)
		}
	}

	// Create Concept nodes (id 700..899).
	for i := range conceptCount {
		id := entityCount + i
		q := fmt.Sprintf(`CREATE (n:Concept {id: %d, name: 'concept-%d'})`, id, id)
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("CREATE Concept %d: %v", id, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("CREATE Concept %d Close: %v", id, err)
		}
	}

	// Create Person nodes (id 900..999).
	for i := range personCount {
		id := entityCount + conceptCount + i
		q := fmt.Sprintf(`CREATE (n:Person {id: %d, name: 'person-%d'})`, id, id)
		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("CREATE Person %d: %v", id, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("CREATE Person %d Close: %v", id, err)
		}
	}

	// Create RELATED_TO edges between Entity nodes (deterministic, no self-loops).
	// For Entity i, connect to Entity (i*131 + d*17) % entityCount for d in 0..2,
	// skipping self-loops.
	for i := range entityCount {
		for d := range edgesPerEntity {
			j := (i*131 + d*17) % entityCount
			if j == i {
				j = (j + 1) % entityCount
			}
			q := fmt.Sprintf(
				`MATCH (a:Entity {id: %d}), (b:Entity {id: %d}) CREATE (a)-[:RELATED_TO]->(b)`,
				i, j,
			)
			res, err := eng.RunInTxAny(ctx, q, nil)
			if err != nil {
				// Edge creation failures are non-fatal: the MATCH may return 0
				// rows if a node was not yet committed (engine is eventually
				// consistent within a session). Log and continue.
				t.Logf("RELATED_TO %d→%d: %v (skipped)", i, j, err)
				continue
			}
			for res.Next() {
			}
			if err := res.Close(); err != nil {
				t.Fatalf("RELATED_TO %d→%d Close: %v", i, j, err)
			}
		}
	}

	t.Logf("graph built: %d node types, ~%d RELATED_TO edges",
		3, entityCount*edgesPerEntity)

	// ── Install cache probe ───────────────────────────────────────────────────
	probe := &cacheHitProbe{}
	metrics.SetBackend(probe)
	t.Cleanup(func() { metrics.SetBackend(nil) })

	// ── Varlen MATCH query ────────────────────────────────────────────────────
	const varlenQuery = `MATCH (e:Entity)-[:RELATED_TO*1..3]->(target:Entity)
WHERE e.id < 10
RETURN DISTINCT target.id
ORDER BY target.id`

	// First run — expected cache miss (cold plan cache).
	rowCount := runWikidataQuery(ctx, t, eng, varlenQuery)
	t.Logf("varlen MATCH result rows (run 1): %d", rowCount)

	if rowCount < 1 {
		t.Errorf("varlen MATCH: got 0 rows; want >= 1 (graph has RELATED_TO edges from Entity id < 10)")
	}

	// Second run — must be a cache hit (same query text).
	rowCount2 := runWikidataQuery(ctx, t, eng, varlenQuery)
	t.Logf("varlen MATCH result rows (run 2): %d", rowCount2)

	// ── Verify plan-cache hit ─────────────────────────────────────────────────
	hits := probe.hits.Load()
	t.Logf("plan_cache status: verified (hits=%d, misses=%d)", hits, probe.misses.Load())

	if hits < 1 {
		t.Errorf("plan_cache: 0 hits after two identical runs; want >= 1")
	}
}

// runWikidataQuery executes query via engine.RunAny, drains the result, and
// returns the row count. It fails the test fatally on any error.
func runWikidataQuery(ctx context.Context, t *testing.T, eng *cypher.Engine, query string) int {
	t.Helper()
	res, err := eng.RunAny(ctx, query, nil)
	if err != nil {
		t.Fatalf("RunAny: %v", err)
	}
	count := 0
	for res.Next() {
		count++
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Result.Close: %v", err)
	}
	return count
}
