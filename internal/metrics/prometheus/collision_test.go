package prometheus

import (
	"strings"
	"testing"
)

// TestRegistry_RawNameCollisionCollapses pins the #1519 invariant: two distinct
// raw names that sanitize to the same metric name share ONE counter and emit
// exactly one exposition line. The raw-name fast-path cache holds both raw keys,
// but the canonical sanitized-keyed map collapses them via LoadOrStore, so the
// counter is shared (its value sums both increments) and WriteText emits a
// single series — matching the pre-#1519 sanitized-keyed map semantics.
func TestRegistry_RawNameCollisionCollapses(t *testing.T) {
	r := New()
	r.IncCounter("a.b", 3) // sanitizes to a_b
	r.IncCounter("a/b", 4) // also sanitizes to a_b -> must share the counter

	var sb strings.Builder
	if err := r.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	text := sb.String()

	// The two increments collapse into one counter valued 7.
	if !strings.Contains(text, "a_b 7\n") {
		t.Errorf("want a single collapsed counter line %q; got:\n%s", "a_b 7", text)
	}
	// Exactly one series declared for the sanitized name (no duplicate counter
	// for the second raw name).
	if n := strings.Count(text, "# TYPE a_b counter\n"); n != 1 {
		t.Errorf("want exactly 1 TYPE line for a_b, got %d:\n%s", n, text)
	}
	// Neither increment leaked into a separate, un-collapsed counter.
	if strings.Contains(text, "a_b 3\n") || strings.Contains(text, "a_b 4\n") {
		t.Errorf("the increments must not appear as separate counters; got:\n%s", text)
	}
}

// TestRegistry_RawCacheStableAcrossCalls asserts the raw-name fast path returns
// the same counter on repeated calls (so increments accumulate), exercising the
// counterByRaw cache hit after the first establishing call.
func TestRegistry_RawCacheStableAcrossCalls(t *testing.T) {
	r := New()
	for i := 0; i < 5; i++ {
		r.IncCounter("repeated.name", 2)
	}
	var sb strings.Builder
	if err := r.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(sb.String(), "repeated_name 10\n") {
		t.Errorf("want repeated_name 10 (5×2 on one cached counter); got:\n%s", sb.String())
	}
}
