package expr

// security_regexcache_bound_test.go — white-box ReDoS-defence fence for the
// `=~` compiled-regex cache (finding #1479, SEC-2026-06-14 audit).
//
// Before #1479 the `=~` operator was dead (the parser emitted plain `=`), so
// the bounded compiled-regex cache (regexcache.go, capacity regexCacheCapacity)
// was unreachable from the public engine — its ReDoS / compile-cost defence
// was never exercised. Now that `=~` is live and anchored, an attacker can
// drive an unbounded number of DISTINCT patterns through the cache (each
// pattern, possibly from a query parameter, becomes a cache key). This test
// proves the now-reachable cache stays bounded by its fixed capacity under
// exactly that hostile churn, so distinct-pattern flooding cannot exhaust
// memory.
//
// It drives the shared cache through the same Eval(`=~`) path the engine uses,
// then asserts the unexported size invariant directly — the white-box vantage
// the public engine cannot reach. The end-to-end operator behaviour is fenced
// separately in cypher/security_regex_operator_test.go.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
)

// TestSec_Cypher_RegexCacheBoundedUnderHostilePatterns fires more distinct
// patterns than the cache capacity through the live `=~` evaluation path and
// asserts the shared compiled-regex cache never exceeds regexCacheCapacity.
func TestSec_Cypher_RegexCacheBoundedUnderHostilePatterns(t *testing.T) {
	// Use the SAME process-wide cache the engine uses, exercised via the SAME
	// evalStringOp("=~") path, so this proves the live path is bounded — not a
	// throwaway cache instance.
	const distinct = regexCacheCapacity * 2 // far exceeds the bound

	mkRegex := func(subject, pattern string) *ast.BinaryOp {
		return &ast.BinaryOp{
			Left:     &ast.StringLiteral{Value: subject},
			Operator: "=~",
			Right:    &ast.StringLiteral{Value: pattern},
		}
	}

	for i := 0; i < distinct; i++ {
		// Each pattern is distinct (a unique anchored form → a unique cache
		// key) and valid. The subject is irrelevant to the cache; only the
		// pattern is keyed.
		pattern := "hostile-" + strconv.Itoa(i) + "-[0-9]+"
		if _, err := Eval(mkRegex("subject", pattern), nil, nil, nullRegStub{}); err != nil {
			t.Fatalf("Eval of =~ with pattern %q returned error: %v", pattern, err)
		}
		if sz := regexCacheShared.len(); sz > regexCacheCapacity {
			t.Fatalf("after %d distinct hostile patterns the shared regex cache size = %d, exceeds capacity %d",
				i+1, sz, regexCacheCapacity)
		}
	}

	// After driving 2x capacity distinct patterns the cache must be full but
	// never over capacity — the FIFO eviction kept it bounded.
	if sz := regexCacheShared.len(); sz != regexCacheCapacity {
		t.Fatalf("final shared regex cache size = %d, want exactly %d (full and bounded)",
			sz, regexCacheCapacity)
	}
}
