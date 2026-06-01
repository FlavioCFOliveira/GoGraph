package expr

// regexcache.go — bounded, concurrency-safe cache of compiled Cypher `=~`
// patterns (improvement I6).
//
// The Cypher regex operator `=~` previously called regexp.MatchString for
// every evaluated row, recompiling the same pattern once per row. Over a
// large scan with a constant pattern (e.g. WHERE x =~ $p) this wasted CPU on
// redundant compilation. This cache compiles a given pattern at most once and
// reuses the resulting *regexp.Regexp on subsequent rows.
//
// Semantics are identical to compiling fresh on every call: a valid pattern
// matches exactly as regexp.MatchString would, and a pattern that fails to
// compile is recorded with its error so the caller maps it to NULL — the same
// behaviour as before. Compile failures are cached as well, so a repeatedly
// evaluated invalid (possibly hostile) pattern is not recompiled either; this
// does not change observable behaviour, it only avoids redundant work.
//
// # Boundedness
//
// The cache is keyed by the pattern string, which can originate from untrusted
// query parameters. An unbounded cache would therefore be a memory-exhaustion
// vector. The cache enforces a fixed upper bound (regexCacheCapacity) and
// evicts in FIFO order once full, so its size never exceeds the capacity
// regardless of how many distinct patterns are seen.
//
// # Concurrency
//
// The cache is safe for concurrent use by multiple goroutines. All access to
// the underlying map and the eviction queue is guarded by a single sync.Mutex.
// The critical section only performs map and slice operations (no compilation
// or matching), so contention is minimal; compiled regexps are immutable and
// regexp.Regexp.MatchString is itself safe for concurrent use, so matching
// happens outside the lock.

import (
	"regexp"
	"sync"
)

// regexCacheCapacity is the maximum number of distinct compiled patterns the
// shared cache retains. Patterns beyond this bound trigger FIFO eviction. The
// value is a deliberate trade-off: large enough to absorb the realistic set of
// distinct patterns in a workload, small enough to cap memory under adversarial
// pattern churn.
const regexCacheCapacity = 1024

// regexEntry is the cached outcome of compiling one pattern: either a non-nil
// compiled regexp (err == nil) or a nil regexp together with the compile error
// (err != nil). Exactly one of the two states holds.
type regexEntry struct {
	re  *regexp.Regexp
	err error
}

// regexCache is a bounded, concurrency-safe FIFO cache mapping pattern strings
// to their compile outcome. The zero value is not usable; construct with
// newRegexCache.
type regexCache struct {
	mu      sync.Mutex
	entries map[string]regexEntry
	// order records insertion order of the keys currently held in entries,
	// used to pick the eviction victim (oldest first).
	order    []string
	capacity int

	// compileFn compiles a pattern. It is a field so tests can count
	// compilations; production code always uses regexp.Compile.
	compileFn func(string) (*regexp.Regexp, error)
}

// newRegexCache builds a bounded FIFO regex cache with the given capacity.
// A capacity <= 0 is clamped to 1 so the cache always holds at least one entry.
func newRegexCache(capacity int) *regexCache {
	if capacity <= 0 {
		capacity = 1
	}
	return &regexCache{
		entries:   make(map[string]regexEntry, capacity),
		order:     make([]string, 0, capacity),
		capacity:  capacity,
		compileFn: regexp.Compile,
	}
}

// compile returns the compiled regexp for pattern, or the error produced when
// the pattern is invalid. On a cache hit it returns the stored outcome without
// recompiling; on a miss it compiles once, stores the outcome (success or
// failure), evicting the oldest entry first if the cache is at capacity, and
// returns it. The returned *regexp.Regexp (when err == nil) is immutable and
// safe to use concurrently.
func (c *regexCache) compile(pattern string) (*regexp.Regexp, error) {
	c.mu.Lock()
	if e, ok := c.entries[pattern]; ok {
		c.mu.Unlock()
		return e.re, e.err
	}
	c.mu.Unlock()

	// Compile outside the lock: compilation is comparatively expensive and
	// pure, so we never hold the mutex across it. A concurrent compile of the
	// same pattern is possible but harmless — both produce equal outcomes and
	// the store below is idempotent.
	re, err := c.compileFn(pattern)

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check: another goroutine may have stored this pattern meanwhile.
	if e, ok := c.entries[pattern]; ok {
		return e.re, e.err
	}
	if len(c.order) >= c.capacity {
		victim := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, victim)
	}
	c.entries[pattern] = regexEntry{re: re, err: err}
	c.order = append(c.order, pattern)
	return re, err
}

// len reports the number of entries currently held. Intended for tests
// asserting the boundedness invariant; safe for concurrent use.
func (c *regexCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// regexCacheShared is the process-wide cache used by the expression evaluator.
// It is safe for concurrent use; see the type documentation for the contract.
var regexCacheShared = newRegexCache(regexCacheCapacity)
