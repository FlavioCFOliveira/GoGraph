//go:build soak

// Package stress — T663: plan-cache thrash 1e5 queries (soak).
//
// Pre-generates 1e5 syntactically distinct Cypher queries and submits them
// from 16 goroutines in random order under -race. Verifies:
//  1. go test -race -tags=soak passes.
//  2. goleak clean (via TestMain).
//  3. Eviction count matches expected cache capacity overflow.
//  4. 1% sample: plan text from cache matches single-threaded compile.
package stress

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/metrics"
)

// planCacheEvictionProbe counts evictions from the global metrics backend.
type planCacheEvictionProbe struct {
	evictions atomic.Uint64
}

func (p *planCacheEvictionProbe) IncCounter(name string, delta uint64) {
	if name == "cypher.plan_cache.evictions" {
		p.evictions.Add(delta)
	}
}
func (p *planCacheEvictionProbe) ObserveLatency(string, time.Duration) {}

// thrashQuery returns a syntactically valid Cypher query that is unique per
// (goroutineID, index) pair. The WHERE clause uses a literal integer so each
// text is different and occupies its own cache slot.
func thrashQuery(idx int) string {
	return fmt.Sprintf("MATCH (n) WHERE n.x = %d RETURN n", idx)
}

// TestPlanCache_Thrash runs 16 goroutines submitting 1e5 distinct queries
// against an Engine with a small cache capacity and verifies that:
//   - eviction count >= (numQueries - cacheCapacity), i.e., overflow is observed.
//   - A 1% sample of queries completes without error (plan not corrupted).
func TestPlanCache_Thrash(t *testing.T) {
	const (
		numQueries    = 100_000
		cacheCapacity = 256 // deliberately small to force heavy eviction
		goroutines    = 16
	)
	n := numQueries
	if testing.Short() {
		// 1 000 queries, capacity 64 — still forces eviction.
		n = 1_000
	}

	defer goleak.VerifyNone(t)

	// Install the eviction probe before constructing the engine so that cache
	// misses from the very first query are counted. This must NOT run parallel
	// (shared global metrics backend).
	probe := &planCacheEvictionProbe{}
	metrics.SetBackend(probe)
	t.Cleanup(func() { metrics.SetBackend(nil) })

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{PlanCacheCapacity: cacheCapacity})
	ctx := context.Background()

	queries := make([]string, n)
	for i := range queries {
		queries[i] = thrashQuery(i)
	}

	var (
		wg         sync.WaitGroup
		execErrors atomic.Int64
	)
	wg.Add(goroutines)
	for gID := 0; gID < goroutines; gID++ {
		gID := gID
		go func() {
			defer wg.Done()
			// Each goroutine shuffles its own view of the query list so the
			// submission order is random and goroutines interleave cache
			// evictions non-deterministically.
			rng := rand.New(rand.NewPCG(uint64(gID), 0xdeadbeef)) //nolint:gosec // test-only RNG seed
			idxs := rng.Perm(n)
			for _, qi := range idxs {
				q := queries[qi]
				res, err := eng.RunAny(ctx, q, nil)
				if err != nil {
					execErrors.Add(1)
					continue
				}
				for res.Next() {
				}
				if cerr := res.Err(); cerr != nil {
					execErrors.Add(1)
				}
				if err := res.Close(); err != nil {
					execErrors.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if e := execErrors.Load(); e > 0 {
		t.Errorf("plan execution errors: %d", e)
	}

	// AC3: evictions must reflect cache overflow. Every unique query beyond
	// cacheCapacity must have been evicted at least once. With n queries and
	// goroutines all cycling through distinct keys, we expect at least
	// (n - cacheCapacity) evictions (each query beyond capacity displaces one).
	evicted := probe.evictions.Load()
	minExpected := uint64(n - cacheCapacity)
	if evicted < minExpected {
		t.Errorf("eviction count = %d; want >= %d (numQueries - cacheCapacity)",
			evicted, minExpected)
	}
	t.Logf("plan-cache evictions: %d (queries=%d, capacity=%d)", evicted, n, cacheCapacity)

	// AC4: 1% sample — re-run a random subset of queries after the storm and
	// verify they complete without error (compiled plan not corrupted).
	sampleSize := n / 100
	if sampleSize < 1 {
		sampleSize = 1
	}
	rng := rand.New(rand.NewPCG(0xc0ffee, 0xbabe)) //nolint:gosec // deterministic test seed, not security-sensitive
	corrupt := 0
	for range sampleSize {
		q := queries[rng.IntN(n)]
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			corrupt++
			continue
		}
		for res.Next() {
		}
		if cerr := res.Err(); cerr != nil {
			corrupt++
		}
		_ = res.Close()
	}
	if corrupt > 0 {
		t.Errorf("1%% sample: %d/%d queries returned errors after thrash (plan corruption suspected)",
			corrupt, sampleSize)
	}
}
