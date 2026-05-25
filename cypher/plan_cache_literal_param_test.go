package cypher_test

import (
	"context"
	"testing"
)

// TestPlanCache_LiteralVsParam_SeparateCacheEntries documents and
// verifies the Engine's current behaviour: a query that embeds a
// literal integer and the equivalent query that uses a named parameter
// are treated as distinct cache entries because the cache key is the
// raw query string and the Engine does not normalise literals to
// parameters.
//
// The parametric variant uses a String param to satisfy the sema
// KindString inference pass (n.prop = $name is always inferred as
// expecting a String value).
//
// If literal-to-parameter normalisation is added in a future sprint,
// this test must be updated to expect a single miss and one hit.
//
// NOT parallel: installs a global metrics backend.
func TestPlanCache_LiteralVsParam_SeparateCacheEntries(t *testing.T) {
	eng := newEmptyEngine(t)
	ctx := context.Background()

	const (
		qLiteral = `MATCH (n) WHERE n.x = 42 RETURN n`
		qParam   = `MATCH (n) WHERE n.name = $name RETURN n`
	)

	p := withCacheProbe(t, func() {
		// Compile the literal variant.
		res, err := eng.RunAny(ctx, qLiteral, nil)
		if err != nil {
			t.Fatalf("Run(literal): %v", err)
		}
		drainCacheResult(t, res)

		// Compile the parametric variant with a String param.
		res, err = eng.RunAny(ctx, qParam, map[string]any{"name": "foo"})
		if err != nil {
			t.Fatalf("Run(param): %v", err)
		}
		drainCacheResult(t, res)
	})

	// Both queries are cold on first access: two misses, zero hits.
	// This documents the absence of literal normalisation.
	if got := p.misses.Load(); got != 2 {
		t.Errorf("cache misses = %d; want 2 (no literal normalisation — separate cache keys)",
			got)
	}
	if got := p.hits.Load(); got != 0 {
		t.Errorf("cache hits = %d; want 0", got)
	}
}

// TestPlanCache_LiteralVariants_EachOccupiesOwnSlot confirms that
// N queries differing only in a literal constant each produce exactly
// one miss and no hits, demonstrating that literal values are not
// stripped from the cache key.
//
// NOT parallel: installs a global metrics backend.
func TestPlanCache_LiteralVariants_EachOccupiesOwnSlot(t *testing.T) {
	const N = 4
	eng := newEmptyEngine(t)
	ctx := context.Background()

	queries := [N]string{
		`MATCH (n) WHERE n.x = 1 RETURN n`,
		`MATCH (n) WHERE n.x = 2 RETURN n`,
		`MATCH (n) WHERE n.x = 3 RETURN n`,
		`MATCH (n) WHERE n.x = 4 RETURN n`,
	}

	p := withCacheProbe(t, func() {
		for _, q := range queries {
			res, err := eng.RunAny(ctx, q, nil)
			if err != nil {
				t.Fatalf("Run %q: %v", q, err)
			}
			drainCacheResult(t, res)
		}
	})

	if got := p.misses.Load(); got != N {
		t.Errorf("cache misses = %d; want %d (one per distinct literal)", got, N)
	}
	if got := p.hits.Load(); got != 0 {
		t.Errorf("cache hits = %d; want 0", got)
	}
}
