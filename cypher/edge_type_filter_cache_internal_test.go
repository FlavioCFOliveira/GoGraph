package cypher

// edge_type_filter_cache_internal_test.go — regression coverage for rmp
// #1871 (2026-07-02 production-readiness audit round 2, finding
// "buildEdgeTypeFilter rebuilds O(V+E) whole-graph map on every query
// execution regardless of selectivity").
//
// White-box (package cypher, not cypher_test) so these tests can drive
// edgeTypeFilterCache.getOrBuild and canonicalRelTypesKey directly, with a
// synthetic epoch sequence and a build-call counter, instead of needing a
// full Engine plus real graph mutations to observe the cache's own
// hit/miss/replace bookkeeping in isolation. End-to-end correctness against
// a real, mutating graph (proving the epoch signal is actually wired to
// something meaningful, not just internally consistent) lives in
// edge_type_filter_cache_test.go (package cypher_test).

import (
	"sync"
	"testing"
)

// TestEdgeTypeFilterCache_HitOnSameEpoch verifies that a second getOrBuild
// call with an unchanged epoch reuses the first call's result without
// invoking build again.
func TestEdgeTypeFilterCache_HitOnSameEpoch(t *testing.T) {
	t.Parallel()
	c := newEdgeTypeFilterCache(0)
	calls := 0
	build := func() map[uint64]string {
		calls++
		return map[uint64]string{0: "LIKES"}
	}

	first := c.getOrBuild("LIKES", atEpoch(1), build)
	second := c.getOrBuild("LIKES", atEpoch(1), build)

	if calls != 1 {
		t.Fatalf("build called %d times, want 1 (second call must hit)", calls)
	}
	if len(first) != 1 || first[0] != "LIKES" || len(second) != 1 || second[0] != "LIKES" {
		t.Fatalf("unexpected filter contents: first=%v second=%v", first, second)
	}
}

// TestEdgeTypeFilterCache_MissOnAdvancedEpoch verifies that a getOrBuild
// call whose epoch differs from the stored one always rebuilds, even though
// the key is unchanged — this is the exact mechanism that must fire after
// any edge addition/removal for the same relationship-type combination.
func TestEdgeTypeFilterCache_MissOnAdvancedEpoch(t *testing.T) {
	t.Parallel()
	c := newEdgeTypeFilterCache(0)
	calls := 0

	c.getOrBuild("LIKES", atEpoch(1), func() map[uint64]string {
		calls++
		return map[uint64]string{0: "LIKES"}
	})
	got := c.getOrBuild("LIKES", atEpoch(2), func() map[uint64]string {
		calls++
		return map[uint64]string{5: "LIKES"}
	})

	if calls != 2 {
		t.Fatalf("build called %d times, want 2 (epoch advance must force a rebuild)", calls)
	}
	if _, ok := got[5]; !ok {
		t.Fatalf("getOrBuild returned the stale pre-epoch-advance filter: %v", got)
	}
}

// TestEdgeTypeFilterCache_DistinctKeysDoNotCollide verifies that two
// different relationship-type keys are tracked independently: rebuilding
// one does not evict or affect the other while capacity allows both.
func TestEdgeTypeFilterCache_DistinctKeysDoNotCollide(t *testing.T) {
	t.Parallel()
	c := newEdgeTypeFilterCache(0)
	likesCalls, knowsCalls := 0, 0

	c.getOrBuild("LIKES", atEpoch(1), func() map[uint64]string {
		likesCalls++
		return map[uint64]string{0: "LIKES"}
	})
	c.getOrBuild("KNOWS", atEpoch(1), func() map[uint64]string {
		knowsCalls++
		return map[uint64]string{1: "KNOWS"}
	})
	// Re-fetch LIKES at the same epoch: must still be a hit, unaffected by
	// the intervening KNOWS build.
	c.getOrBuild("LIKES", atEpoch(1), func() map[uint64]string {
		likesCalls++
		return map[uint64]string{0: "LIKES"}
	})

	if likesCalls != 1 {
		t.Errorf("LIKES built %d times, want 1", likesCalls)
	}
	if knowsCalls != 1 {
		t.Errorf("KNOWS built %d times, want 1", knowsCalls)
	}
	if got := c.Len(); got != 2 {
		t.Errorf("cache Len() = %d, want 2 distinct entries", got)
	}
}

// TestEdgeTypeFilterCache_EvictsLeastRecentlyUsed verifies the LRU bound:
// once at capacity, the least-recently-touched key is evicted first.
func TestEdgeTypeFilterCache_EvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	c := newEdgeTypeFilterCache(2)
	build := func(v string) func() map[uint64]string {
		return func() map[uint64]string { return map[uint64]string{0: v} }
	}

	c.getOrBuild("A", atEpoch(1), build("A"))
	c.getOrBuild("B", atEpoch(1), build("B"))
	// Touch A so B becomes the least-recently-used entry.
	c.getOrBuild("A", atEpoch(1), build("A"))
	// Installing a third distinct key at capacity 2 must evict B, not A.
	c.getOrBuild("C", atEpoch(1), build("C"))

	if got := c.Len(); got != 2 {
		t.Fatalf("cache Len() = %d, want 2 (bounded at capacity)", got)
	}
	// Check final membership directly (via a build-call counter, so this
	// inspection cannot itself perturb the LRU order under test): A and C
	// must survive, B must not.
	if _, ok := c.by["A"]; !ok {
		t.Errorf("A was evicted despite being the more recently used entry")
	}
	if _, ok := c.by["C"]; !ok {
		t.Errorf("C (the just-installed entry) is missing from the cache")
	}
	if _, ok := c.by["B"]; ok {
		t.Errorf("B is still present; want it evicted as the least-recently-used entry")
	}

	// Confirm the eviction is behaviourally real, not just a bookkeeping
	// artefact: fetching B now must rebuild rather than hit.
	bCalls := 0
	c.getOrBuild("B", atEpoch(1), func() map[uint64]string {
		bCalls++
		return map[uint64]string{0: "B"}
	})
	if bCalls != 1 {
		t.Errorf("build called %d times for evicted key B, want 1 (a real miss)", bCalls)
	}
}

// TestEdgeTypeFilterCache_DefaultCapacity verifies the non-positive-capacity
// fallback matches the documented default, mirroring newPlanCache's own
// contract so misconfiguration cannot silently disable the bound.
func TestEdgeTypeFilterCache_DefaultCapacity(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{0, -1, -100} {
		c := newEdgeTypeFilterCache(capacity)
		if got := c.Capacity(); got != DefaultEdgeTypeFilterCacheCapacity {
			t.Errorf("newEdgeTypeFilterCache(%d).Capacity() = %d, want %d", capacity, got, DefaultEdgeTypeFilterCacheCapacity)
		}
	}
}

// TestEdgeTypeFilterCache_ConcurrentBuildersOnSameKey exercises the
// documented "let every racer rebuild independently" concurrency contract:
// many goroutines missing on the identical key at the identical epoch must
// all receive a valid, correct filter, and the cache must end up holding
// exactly one entry with no data race (run with -race).
func TestEdgeTypeFilterCache_ConcurrentBuildersOnSameKey(t *testing.T) {
	t.Parallel()
	c := newEdgeTypeFilterCache(0)
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			got := c.getOrBuild("LIKES", atEpoch(1), func() map[uint64]string {
				return map[uint64]string{0: "LIKES"}
			})
			if got[0] != "LIKES" {
				t.Errorf("racer received wrong filter: %v", got)
			}
		}()
	}
	wg.Wait()
	if got := c.Len(); got != 1 {
		t.Errorf("cache Len() = %d after concurrent racers, want 1", got)
	}
}

// TestCanonicalRelTypesKey_OrderAndDuplicateIndependent verifies the cache
// key collapses any two relTypes slices naming the same set, regardless of
// input order or duplicate entries — matching buildEdgeTypeFilter's own
// set-based (not order-based) accept-list semantics.
func TestCanonicalRelTypesKey_OrderAndDuplicateIndependent(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"LIKES", "KNOWS"},
		{"KNOWS", "LIKES"},
		{"LIKES", "KNOWS", "LIKES"},
		{"KNOWS", "KNOWS", "LIKES", "LIKES"},
	}
	want := canonicalRelTypesKey(cases[0])
	for _, c := range cases[1:] {
		if got := canonicalRelTypesKey(c); got != want {
			t.Errorf("canonicalRelTypesKey(%v) = %q, want %q", c, got, want)
		}
	}
}

// TestCanonicalRelTypesKey_DistinctSetsDiffer is the sanity counterpart to
// the order-independence test above: genuinely different type sets must NOT
// collapse to the same key.
func TestCanonicalRelTypesKey_DistinctSetsDiffer(t *testing.T) {
	t.Parallel()
	a := canonicalRelTypesKey([]string{"LIKES"})
	b := canonicalRelTypesKey([]string{"KNOWS"})
	empty := canonicalRelTypesKey(nil)
	if a == b {
		t.Errorf("canonicalRelTypesKey(LIKES) == canonicalRelTypesKey(KNOWS) = %q", a)
	}
	if a == empty || b == empty {
		t.Errorf("a non-empty type set collided with the empty (accept-all) key")
	}
}

// TestCanonicalRelTypesKey_DoesNotMutateInput guards against a regression
// where canonicalRelTypesKey sorts the caller's own slice in place — relTypes
// slices originate from a cached IR plan node (ir.Expand.RelTypes etc.) that
// may be reused across many query executions; mutating it would corrupt
// every other concurrent or subsequent use of that cached plan.
func TestCanonicalRelTypesKey_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	original := []string{"ZEBRA", "APPLE", "MANGO"}
	snapshot := append([]string(nil), original...)
	_ = canonicalRelTypesKey(original)
	for i := range original {
		if original[i] != snapshot[i] {
			t.Fatalf("canonicalRelTypesKey mutated its input: got %v, want %v", original, snapshot)
		}
	}
}
