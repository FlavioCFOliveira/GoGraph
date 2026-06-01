package expr

// regexcache_test.go — tests for the bounded, concurrency-safe `=~` pattern
// cache (improvement I6, task #1237).
//
// These are white-box tests (package expr) so they can drive the unexported
// regexCache directly: count compilations through the compileFn seam, assert
// the boundedness invariant, and verify match/invalid-pattern semantics match
// the previous regexp.MatchString behaviour. End-to-end behaviour through
// Eval(`=~`) is covered alongside, using the shared cache.

import (
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"gograph/cypher/ast"
)

// countingCompiler wraps regexp.Compile and counts invocations so a test can
// assert that a repeated pattern compiles only once.
func countingCompiler(n *int32) func(string) (*regexp.Regexp, error) {
	return func(p string) (*regexp.Regexp, error) {
		atomic.AddInt32(n, 1)
		return regexp.Compile(p)
	}
}

// TestRegexCache_RepeatedPatternCompilesOnce verifies AC #1: evaluating the
// same pattern many times triggers exactly one compilation.
func TestRegexCache_RepeatedPatternCompilesOnce(t *testing.T) {
	var compiles int32
	c := newRegexCache(8)
	c.compileFn = countingCompiler(&compiles)

	const pattern = "^h.*o$"
	for i := 0; i < 100; i++ {
		re, err := c.compile(pattern)
		if err != nil {
			t.Fatalf("unexpected compile error: %v", err)
		}
		if !re.MatchString("hello") {
			t.Fatalf("expected pattern %q to match %q", pattern, "hello")
		}
	}
	if got := atomic.LoadInt32(&compiles); got != 1 {
		t.Fatalf("compile count = %d, want 1 (pattern must compile once)", got)
	}
	if c.len() != 1 {
		t.Fatalf("cache size = %d, want 1", c.len())
	}
}

// TestRegexCache_InvalidPatternCompilesOnce verifies that an invalid pattern is
// also cached (compiled once) and consistently returns its compile error, so a
// repeatedly evaluated hostile pattern is not recompiled per row.
func TestRegexCache_InvalidPatternCompilesOnce(t *testing.T) {
	var compiles int32
	c := newRegexCache(8)
	c.compileFn = countingCompiler(&compiles)

	const bad = "[" // never compiles
	for i := 0; i < 50; i++ {
		re, err := c.compile(bad)
		if err == nil {
			t.Fatalf("expected compile error for %q", bad)
		}
		if re != nil {
			t.Fatalf("expected nil regexp for invalid pattern, got %v", re)
		}
	}
	if got := atomic.LoadInt32(&compiles); got != 1 {
		t.Fatalf("compile count = %d, want 1 (invalid pattern must compile once)", got)
	}
}

// TestRegexCache_Bounded verifies AC #2: inserting more distinct patterns than
// the capacity never lets the cache grow beyond it (FIFO eviction).
func TestRegexCache_Bounded(t *testing.T) {
	const capacity = 16
	c := newRegexCache(capacity)

	for i := 0; i < capacity*10; i++ {
		// Each pattern is distinct and valid.
		p := "^x" + strconv.Itoa(i) + "$"
		if _, err := c.compile(p); err != nil {
			t.Fatalf("unexpected compile error for %q: %v", p, err)
		}
		if sz := c.len(); sz > capacity {
			t.Fatalf("after %d inserts cache size = %d, exceeds capacity %d", i+1, sz, capacity)
		}
	}
	if sz := c.len(); sz != capacity {
		t.Fatalf("final cache size = %d, want exactly %d (full)", sz, capacity)
	}
}

// TestRegexCache_NonPositiveCapacityClamped verifies the constructor clamps a
// non-positive capacity to at least 1, keeping the cache usable and bounded.
func TestRegexCache_NonPositiveCapacityClamped(t *testing.T) {
	for _, cap := range []int{0, -5} {
		c := newRegexCache(cap)
		if c.capacity != 1 {
			t.Fatalf("capacity %d clamped to %d, want 1", cap, c.capacity)
		}
		for i := 0; i < 10; i++ {
			if _, err := c.compile("^a" + strconv.Itoa(i) + "$"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if c.len() != 1 {
			t.Fatalf("size = %d, want 1 for clamped capacity", c.len())
		}
	}
}

// TestRegexCache_MatchesMatchString verifies AC #3: cached results are
// identical to regexp.MatchString for both valid and invalid patterns.
func TestRegexCache_MatchesMatchString(t *testing.T) {
	cases := []struct {
		pattern string
		subject string
	}{
		{"^h.*o$", "hello"},
		{"^h.*o$", "world"},
		{"[0-9]+", "abc123"},
		{"[0-9]+", "abc"},
		{"(", "anything"},   // invalid → error
		{"a(b", "anything"}, // invalid → error
		{"", "anything"},    // empty pattern matches everything
	}
	c := newRegexCache(8)
	for _, tc := range cases {
		wantMatched, wantErr := regexp.MatchString(tc.pattern, tc.subject)

		re, err := c.compile(tc.pattern)
		switch {
		case wantErr != nil:
			if err == nil {
				t.Errorf("pattern %q: cache err=nil, MatchString err=%v", tc.pattern, wantErr)
			}
		case err != nil:
			t.Errorf("pattern %q: cache err=%v, MatchString err=nil", tc.pattern, err)
		default:
			if got := re.MatchString(tc.subject); got != wantMatched {
				t.Errorf("pattern %q on %q: cache matched=%v, MatchString matched=%v",
					tc.pattern, tc.subject, got, wantMatched)
			}
		}
	}
}

// TestRegexCache_ConcurrentSamePattern stresses concurrent access with one
// shared pattern: race-clean and at most one compilation (a transient race to
// the lock may compile more than once, but the cache must still settle to one
// entry).
func TestRegexCache_ConcurrentSamePattern(t *testing.T) {
	var compiles int32
	c := newRegexCache(8)
	c.compileFn = countingCompiler(&compiles)

	const pattern = "^foo[0-9]+bar$"
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				re, err := c.compile(pattern)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				_ = re.MatchString("foo42bar")
			}
		}()
	}
	wg.Wait()

	if c.len() != 1 {
		t.Fatalf("cache size = %d, want 1 after concurrent same-pattern access", c.len())
	}
	// Compilation count is bounded by goroutines racing the initial miss;
	// it must be small and never per-call (200*64 = 12800 calls).
	if got := atomic.LoadInt32(&compiles); got > 64 {
		t.Fatalf("compile count = %d, want <= 64 (one per racing goroutine at most)", got)
	}
}

// TestRegexCache_ConcurrentDistinctPatterns stresses concurrent access with
// many distinct patterns against a small cache: must be race-clean and stay
// bounded throughout.
func TestRegexCache_ConcurrentDistinctPatterns(t *testing.T) {
	const capacity = 32
	c := newRegexCache(capacity)

	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				p := fmt.Sprintf("^p%d_%d$", base, i)
				if _, err := c.compile(p); err != nil {
					t.Errorf("unexpected error for %q: %v", p, err)
					return
				}
				if sz := c.len(); sz > capacity {
					t.Errorf("cache size %d exceeded capacity %d", sz, capacity)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if sz := c.len(); sz > capacity {
		t.Fatalf("final cache size %d exceeds capacity %d", sz, capacity)
	}
}

// TestEval_RegexMatch_EndToEnd verifies the `=~` operator through Eval (using
// the shared cache) preserves match and invalid-pattern (NULL) semantics.
func TestEval_RegexMatch_EndToEnd(t *testing.T) {
	mkStr := func(v string) *ast.StringLiteral { return &ast.StringLiteral{Value: v} }
	mkRegex := func(subject, pattern string) *ast.BinaryOp {
		return &ast.BinaryOp{Left: mkStr(subject), Operator: "=~", Right: mkStr(pattern)}
	}

	tests := []struct {
		name    string
		subject string
		pattern string
		want    Value
	}{
		{"match", "hello", "h.*o", BoolValue(true)},
		{"no_match", "hello", "^z", BoolValue(false)},
		{"anchored_match", "abc123", "^abc[0-9]+$", BoolValue(true)},
		{"invalid_pattern_null", "anything", "[", Null},
		{"another_invalid_null", "x", "a(b", Null},
		{"empty_pattern_matches", "x", "", BoolValue(true)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Eval(mkRegex(tc.subject, tc.pattern), nil, nil, nullRegStub{})
			if err != nil {
				t.Fatalf("Eval error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("%q =~ %q: got %v, want %v", tc.subject, tc.pattern, got, tc.want)
			}
		})
	}
}

// nullRegStub is a no-op FunctionRegistry for the white-box end-to-end test.
type nullRegStub struct{}

func (nullRegStub) Resolve(_ string) (BuiltinFn, bool) { return nil, false }
