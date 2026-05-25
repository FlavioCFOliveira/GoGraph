package cypher_test

import (
	"context"
	"testing"
)

// TestPlanCache_ParameterOnlyChange_SharesPlan verifies that two Run
// calls that differ only in parameter values share the same cached
// plan. The plan-cache key is the raw query string; parameter values
// are resolved at execution time and do not affect the key.
//
// The parameter is typed as a String value to satisfy the sema
// KindString inference pass, which treats every n.prop = $name
// equality as expecting a String.
//
// NOT parallel: installs a global metrics backend.
func TestPlanCache_ParameterOnlyChange_SharesPlan(t *testing.T) {
	eng := newEmptyEngine(t)
	ctx := context.Background()
	const q = `MATCH (n) WHERE n.name = $name RETURN n`

	p := withCacheProbe(t, func() {
		// First run: name="alice" — must be a miss (cold cache).
		res, err := eng.RunAny(ctx, q, map[string]any{"name": "alice"})
		if err != nil {
			t.Fatalf("first Run: %v", err)
		}
		drainCacheResult(t, res)

		// Second run: name="bob" — same query text, different param value.
		// The cache key is the query string, so this must be a hit.
		res, err = eng.RunAny(ctx, q, map[string]any{"name": "bob"})
		if err != nil {
			t.Fatalf("second Run: %v", err)
		}
		drainCacheResult(t, res)
	})

	if got := p.misses.Load(); got != 1 {
		t.Errorf("cache misses = %d; want 1 (first invocation only)", got)
	}
	if got := p.hits.Load(); got < 1 {
		t.Errorf("cache hits = %d; want >= 1 (second invocation)", got)
	}
}

// TestPlanCache_DifferentQueryText_SeparateEntries confirms that two
// structurally similar queries with different literal constants produce
// distinct cache entries (two misses, zero hits).
//
// NOT parallel: installs a global metrics backend.
func TestPlanCache_DifferentQueryText_SeparateEntries(t *testing.T) {
	eng := newEmptyEngine(t)
	ctx := context.Background()

	p := withCacheProbe(t, func() {
		for _, q := range []string{
			`MATCH (n) WHERE n.id = 1 RETURN n`,
			`MATCH (n) WHERE n.id = 2 RETURN n`,
		} {
			res, err := eng.RunAny(ctx, q, nil)
			if err != nil {
				t.Fatalf("Run %q: %v", q, err)
			}
			drainCacheResult(t, res)
		}
	})

	if got := p.misses.Load(); got != 2 {
		t.Errorf("cache misses = %d; want 2 (one per distinct query text)", got)
	}
	if got := p.hits.Load(); got != 0 {
		t.Errorf("cache hits = %d; want 0", got)
	}
}
