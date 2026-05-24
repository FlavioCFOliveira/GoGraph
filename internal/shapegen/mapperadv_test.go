package shapegen

import (
	"sync"
	"testing"

	"gograph/graph"
)

// TestMapperAdv_GenerateShardZeroKeys_AllShardZero exercises AC #1
// and AC #2: the generator produces at least 1e5 distinct strings,
// and inserting all of them into a fresh Mapper yields NodeIDs whose
// shard byte is zero on every key.
func TestMapperAdv_GenerateShardZeroKeys_AllShardZero(t *testing.T) {
	const n = 100_000
	keys := GenerateShardZeroKeys(n)
	if len(keys) < n {
		t.Fatalf("len(keys) = %d, want >= %d", len(keys), n)
	}
	seen := make(map[string]struct{}, n)
	m := graph.NewMapper[string]()
	for i, k := range keys {
		if _, dup := seen[k]; dup {
			t.Fatalf("duplicate key %q at index %d", k, i)
		}
		seen[k] = struct{}{}
		id := m.Intern(k)
		if shard := graph.MapperShardOf(id); shard != 0 {
			t.Fatalf("keys[%d]=%q routed to shard %d, want 0", i, k, shard)
		}
	}
	if len(seen) < n {
		t.Fatalf("distinct keys = %d, want >= %d", len(seen), n)
	}
}

// TestMapperAdv_GenerateShardZeroKeys_Determinism exercises AC #3:
// two independent calls with the same n produce byte-equal slices.
func TestMapperAdv_GenerateShardZeroKeys_Determinism(t *testing.T) {
	t.Parallel()
	const n = 10_000
	a := GenerateShardZeroKeys(n)
	b := GenerateShardZeroKeys(n)
	if len(a) != len(b) {
		t.Fatalf("lengths diverge: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("keys[%d]: %q vs %q", i, a[i], b[i])
		}
	}
}

// TestMapperAdv_GenerateShardZeroKeys_CacheReplay verifies the
// caller-visible cache semantics: requesting fewer keys after a
// larger request returns the same prefix.
func TestMapperAdv_GenerateShardZeroKeys_CacheReplay(t *testing.T) {
	t.Parallel()
	wide := GenerateShardZeroKeys(8_000)
	narrow := GenerateShardZeroKeys(2_000)
	if len(wide) < 8_000 {
		t.Fatalf("len(wide) = %d, want >= 8000", len(wide))
	}
	if len(narrow) != 2_000 {
		t.Fatalf("len(narrow) = %d, want 2000", len(narrow))
	}
	for i, k := range narrow {
		if k != wide[i] {
			t.Fatalf("narrow[%d]=%q != wide[%d]=%q", i, k, i, wide[i])
		}
	}
}

// TestMapperAdv_GenerateShardZeroKeys_NPositive enforces the n > 0
// contract documented in the godoc.
func TestMapperAdv_GenerateShardZeroKeys_NPositive(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("GenerateShardZeroKeys(0) did not panic")
		}
	}()
	_ = GenerateShardZeroKeys(0)
}

// TestMapperAdv_GenerateShardZeroKeys_RaceClean exercises AC #4:
// concurrent calls from many goroutines must not race or corrupt
// each other's view of the corpus.
func TestMapperAdv_GenerateShardZeroKeys_RaceClean(t *testing.T) {
	t.Parallel()
	const (
		workers = 16
		n       = 1_000
	)
	want := GenerateShardZeroKeys(n)
	var wg sync.WaitGroup
	wg.Add(workers)
	results := make([][]string, workers)
	for w := 0; w < workers; w++ {
		w := w
		go func() {
			defer wg.Done()
			results[w] = GenerateShardZeroKeys(n)
		}()
	}
	wg.Wait()
	for w, got := range results {
		if len(got) != len(want) {
			t.Fatalf("worker %d: len = %d, want %d", w, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("worker %d: key[%d] = %q, want %q", w, i, got[i], want[i])
			}
		}
	}
}

// TestMapperAdv_CacheIsValidGuard pins the guard that catches stale
// cache entries: if the first key no longer hashes to shard 0 the
// cache is considered invalid.
func TestMapperAdv_CacheIsValidGuard(t *testing.T) {
	t.Parallel()
	good := GenerateShardZeroKeys(1)
	if !cacheIsValid(good) {
		t.Fatal("cacheIsValid returned false for a freshly generated key")
	}
	// "nonshard0-key" is exceedingly unlikely to land on shard 0 by
	// chance: the test asserts the guard rejects an arbitrary key.
	bad := []string{"nonshard0-key-sentinel"}
	if graph.MapperShardOf(graph.NewMapper[string]().Intern(bad[0])) == 0 {
		t.Skip("sentinel string fortuitously routes to shard 0; the guard cannot be exercised")
	}
	if cacheIsValid(bad) {
		t.Fatal("cacheIsValid returned true for a non-shard-0 key")
	}
	if cacheIsValid(nil) {
		t.Fatal("cacheIsValid returned true for nil input")
	}
}
