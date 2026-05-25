package cypher_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	cmetrics "gograph/internal/metrics"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// cacheProbe is a metrics.Backend that records plan-cache hits, misses
// and evictions so tests can assert cache behaviour through the Engine
// without requiring a dedicated accessor on the Engine type.
//
// The global metrics backend is swapped atomically; tests that install
// this probe must NOT run in parallel (shared global state).
type cacheProbe struct {
	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

func (p *cacheProbe) IncCounter(name string, delta uint64) {
	switch name {
	case "cypher.plan_cache.hits":
		p.hits.Add(delta)
	case "cypher.plan_cache.misses":
		p.misses.Add(delta)
	case "cypher.plan_cache.evictions":
		p.evictions.Add(delta)
	}
}

func (p *cacheProbe) ObserveLatency(string, time.Duration) {}

// withCacheProbe installs a fresh cacheProbe, runs fn, restores the
// default no-op backend, then returns the probe for inspection.
func withCacheProbe(t *testing.T, fn func()) *cacheProbe {
	t.Helper()
	p := &cacheProbe{}
	cmetrics.SetBackend(p)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })
	fn()
	return p
}

// drainCacheResult drains and closes a Result, failing the test on error.
func drainCacheResult(t *testing.T, res *cypher.Result) {
	t.Helper()
	for res.Next() {
		_ = res.Record()
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Result.Close: %v", err)
	}
}

// newEmptyEngine builds an Engine over a zero-node graph. Sufficient
// for plan-cache tests that do not inspect query results.
func newEmptyEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g)
}

// newEngineWithCapacity builds an Engine with a specific plan cache
// capacity and returns both the underlying graph and the engine.
func newEngineWithCapacity(t *testing.T, capacity int) (*lpg.Graph[string, float64], *cypher.Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{PlanCacheCapacity: capacity})
	return g, eng
}

// distinctQuery returns a MATCH query that is syntactically valid but
// unique per (goroutineID, index) pair so it occupies a distinct cache
// slot. The WHERE clause uses a literal integer to ensure the query
// text differs without requiring parameter evaluation.
func distinctQuery(gID, idx int) string {
	return fmt.Sprintf("MATCH (n) WHERE n.seq = %d RETURN n", gID*10_000+idx)
}

// TestPlanCache_Engine_CacheHitOnSecondQuery verifies that the Engine
// populates the plan cache on the first Run and returns the cached
// plan on every subsequent Run of the same query text.
//
// NOT parallel: installs a global metrics backend.
func TestPlanCache_Engine_CacheHitOnSecondQuery(t *testing.T) {
	eng := newEmptyEngine(t)
	ctx := context.Background()
	const q = `MATCH (n) RETURN n`

	p := withCacheProbe(t, func() {
		// First invocation must be a miss: the plan is not yet cached.
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("first Run: %v", err)
		}
		drainResult(t, res)

		// Second invocation with identical query text must be a hit.
		res, err = eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("second Run: %v", err)
		}
		drainResult(t, res)
	})

	if got := p.misses.Load(); got != 1 {
		t.Errorf("cache misses = %d; want 1 (first invocation only)", got)
	}
	if got := p.hits.Load(); got < 1 {
		t.Errorf("cache hits = %d; want >= 1 (second invocation)", got)
	}
}

// TestPlanCache_Engine_RepeatedQuery_SingleEntry confirms that N
// executions of the same query produce exactly 1 miss and N-1 hits,
// demonstrating that the same slot is reused without duplication.
//
// NOT parallel: installs a global metrics backend.
func TestPlanCache_Engine_RepeatedQuery_SingleEntry(t *testing.T) {
	const N = 5
	eng := newEmptyEngine(t)
	ctx := context.Background()
	const q = `MATCH (n) RETURN n`

	p := withCacheProbe(t, func() {
		for i := 0; i < N; i++ {
			res, err := eng.RunAny(ctx, q, nil)
			if err != nil {
				t.Fatalf("Run %d: %v", i, err)
			}
			drainResult(t, res)
		}
	})

	if got := p.misses.Load(); got != 1 {
		t.Errorf("misses = %d; want exactly 1", got)
	}
	if got := p.hits.Load(); got != N-1 {
		t.Errorf("hits = %d; want %d", got, N-1)
	}
}
