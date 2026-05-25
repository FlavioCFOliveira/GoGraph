package cypher_test

import (
	"context"
	"sync"
	"testing"
)

// TestPlanCache_ConcurrentCompile_SingleSlot verifies that 16
// goroutines issuing the same query text concurrently through the
// Engine produce exactly one plan-cache entry. Because the Engine does
// not serialise compilation (each goroutine may parse independently
// before any peer has stored its result), the number of cache misses
// may be between 1 and 16. The invariant is:
//
//   - hits + misses = goroutines (every call accounts for exactly one)
//   - zero evictions (a single query cannot displace itself)
//   - after the concurrent phase, a further serial Run is a hit
//     (the cache is warm, exactly one slot is occupied)
//
// The test is run with the race detector; any data race in the
// plan-cache or Engine plumbing will surface here.
//
// NOT parallel at the test level: it installs a global metrics backend.
// Inner goroutines run concurrently to exercise the contended path.
func TestPlanCache_ConcurrentCompile_SingleSlot(t *testing.T) {
	const goroutines = 16
	eng := newEmptyEngine(t)
	ctx := context.Background()
	const q = `MATCH (n) RETURN n`

	// Phase 1: concurrent invocations — all goroutines run before any
	// assertions so the metrics backend captures the full set of events.
	p := withCacheProbe(t, func() {
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for range goroutines {
			go func() {
				defer wg.Done()
				res, err := eng.RunAny(ctx, q, nil)
				if err != nil {
					return
				}
				for res.Next() {
					_ = res.Record()
				}
				_ = res.Close()
			}()
		}
		wg.Wait()
	})

	// Check concurrent-phase invariants.
	total := p.hits.Load() + p.misses.Load()
	if total != goroutines {
		t.Errorf("hits(%d)+misses(%d) = %d; want %d (one per Run call)",
			p.hits.Load(), p.misses.Load(), total, goroutines)
	}
	if got := p.evictions.Load(); got != 0 {
		t.Errorf("evictions = %d; want 0 (single query cannot evict itself)", got)
	}
	if got := p.misses.Load(); got < 1 {
		t.Errorf("misses = 0; want >= 1 (at least the first compile must miss)")
	}

	// Phase 2: serial run after the cache is warm — must be a hit.
	p2 := withCacheProbe(t, func() {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("serial Run after concurrent phase: %v", err)
		}
		drainCacheResult(t, res)
	})
	if got := p2.hits.Load(); got != 1 {
		t.Errorf("post-concurrent serial Run: hits = %d; want 1 (cache must be warm)", got)
	}
	if got := p2.misses.Load(); got != 0 {
		t.Errorf("post-concurrent serial Run: misses = %d; want 0", got)
	}
}

// TestPlanCache_ConcurrentDistinctQueries_BoundedUnderContention
// verifies that many goroutines each compiling a unique query do not
// cause a data race and that the total call accounting is consistent.
//
// NOT parallel: installs a global metrics backend.
func TestPlanCache_ConcurrentDistinctQueries_BoundedUnderContention(t *testing.T) {
	const (
		goroutines    = 8
		queriesEach   = 20 // total 160 distinct queries
		cacheCapacity = 32
	)
	_, eng := newEngineWithCapacity(t, cacheCapacity)
	ctx := context.Background()

	p := withCacheProbe(t, func() {
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for gID := range goroutines {
			go func(gID int) {
				defer wg.Done()
				for i := range queriesEach {
					q := distinctQuery(gID, i)
					res, err := eng.RunAny(ctx, q, nil)
					if err != nil {
						continue
					}
					for res.Next() {
						_ = res.Record()
					}
					_ = res.Close()
				}
			}(gID)
		}
		wg.Wait()
	})

	total := p.hits.Load() + p.misses.Load()
	if total != goroutines*queriesEach {
		t.Errorf("hits+misses = %d; want %d (one per Run call)",
			total, goroutines*queriesEach)
	}
}
