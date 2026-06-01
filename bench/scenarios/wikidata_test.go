//go:build nightly

// Package scenarios_test — T785: knowledge-graph traversal on a synthetic
// Wikidata-like sub-dump (nightly layer).
//
// TestWikidata_VarlenMatch_Nightly builds a synthetic knowledge graph and runs
// a variable-length MATCH query (depth 1..3) via the Cypher engine directly,
// verifying:
//
//  1. The query returns without error and produces at least 1 result row.
//  2. Plan-cache hit ratio > 0: running the identical query twice causes a
//     cache hit on the second run (verified via a metrics probe).
//  3. goleak clean (via TestMain in main_test.go).
//
// Graph construction note: the Cypher engine's MATCH+CREATE two-node pattern
// (e.g. MATCH (a:Entity),(b:Entity) CREATE (a)-[…]->(b)) exhibits a known
// limitation for same-label cross-products — it returns 0 rows. Edges are
// therefore created inline within the same CREATE clause as the source node.
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

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// cacheHitProbe is a [metrics.Backend] that counts plan-cache hits and misses.
// It is used only in TestWikidata_VarlenMatch_Nightly and must not run
// concurrently with other tests that replace the global metrics backend.
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
// Graph layout:
//   - 100 chains of the form:
//     (Entity {id:i})-[:RELATED_TO]->(Entity {id:i+100})-[:RELATED_TO]->(Entity {id:i+200})
//     giving 300 Entity nodes with 200 RELATED_TO edges.
//   - All source nodes have id < 100, satisfying the WHERE e.id < 10 predicate
//     for i in [0,9].
//
// The varlen query finds Entity nodes reachable from Entity id < 10 within 3
// RELATED_TO hops.
func TestWikidata_VarlenMatch_Nightly(t *testing.T) {
	// NOT parallel: installs the global metrics backend.

	const (
		// Number of 3-node chains to create.
		// Each chain: src(id=i) → mid(id=i+chainLen) → dst(id=i+2*chainLen)
		chainLen = 100
	)

	// ── Build the LPG ─────────────────────────────────────────────────────────
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	// Create each chain as a single inline CREATE clause. Inline CREATE avoids
	// the same-label MATCH cross-product limitation (Sprint 70 known gap).
	for i := range chainLen {
		srcID := i
		midID := i + chainLen
		dstID := i + 2*chainLen

		q := fmt.Sprintf(
			`CREATE (src:Entity {id: %d, name: 'entity-%d'})`+
				`-[:RELATED_TO]->`+
				`(mid:Entity {id: %d, name: 'entity-%d'})`+
				`-[:RELATED_TO]->`+
				`(dst:Entity {id: %d, name: 'entity-%d'})`,
			srcID, srcID,
			midID, midID,
			dstID, dstID,
		)

		res, err := eng.RunInTxAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("CREATE chain %d: %v", i, err)
		}
		for res.Next() {
		}
		if err := res.Close(); err != nil {
			t.Fatalf("CREATE chain %d Close: %v", i, err)
		}
	}

	t.Logf("graph built: %d chains, %d Entity nodes, %d RELATED_TO edges",
		chainLen, chainLen*3, chainLen*2)

	// ── Install cache probe ───────────────────────────────────────────────────
	probe := &cacheHitProbe{}
	metrics.SetBackend(probe)
	t.Cleanup(func() { metrics.SetBackend(nil) })

	// ── Varlen MATCH query ────────────────────────────────────────────────────
	// Targets Entity source nodes with id < 10 (chains 0..9) and finds all
	// reachable Entity nodes within 1..3 RELATED_TO hops.
	// Expected: each chain i<10 contributes 2 reachable nodes (mid, dst).
	// Distinct results: at most 20 distinct target ids (ids 100..109 and 200..209).
	const varlenQuery = `MATCH (e:Entity)-[:RELATED_TO*1..3]->(target:Entity)
WHERE e.id < 10
RETURN DISTINCT target.id
ORDER BY target.id`

	// First run — expected cache miss (cold plan cache).
	rowCount := runWikidataQuery(ctx, t, eng, varlenQuery)
	t.Logf("varlen MATCH result rows (run 1): %d", rowCount)

	if rowCount < 1 {
		t.Errorf("varlen MATCH: got 0 rows; want >= 1 (chains 0..9 each contribute mid+dst nodes)")
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
