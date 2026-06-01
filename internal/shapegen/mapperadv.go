package shapegen

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// mapperadv.go provides adversarial natural-key generators that
// stress the [graph.Mapper] sharding scheme. The interesting workload
// is a flood of distinct keys that all collide on a single shard:
// it pins worst-case lock contention and validates that the FNV-1a
// shard routing introduced in commit ba813d0 holds under sustained
// pressure.
//
// # Determinism and cache
//
// [GenerateShardZeroKeys] returns the same byte-for-byte slice on
// every call for a given n, regardless of process or platform.
// Generation is fast (≈ 100 ns per accepted key on x86_64) but the
// first call materialises a sizeable corpus (n = 1e5 needs ≈ 25 M
// rejected candidates because acceptance is 1 / shard-count). To
// amortise that cost across `go test` reinvocations, the function
// transparently caches its output under
// testdata/shapegen/mapper_shard0_keys.txt; subsequent calls with the
// same or smaller n replay from the cache.
//
// The cache file is checked into the repository: it is a
// deterministic artefact of (a) the FNV-1a constants pinned in
// [graph.fnv1aString] and (b) the candidate-string format pinned by
// this file. Either change invalidates the cache; the generator
// detects the invalidation by hashing the first cached entry and
// regenerates the file when the first cached key no longer routes
// to shard 0.
//
// # Runtime shard-count discovery
//
// The function reads the active shard count via
// [graph.MapperShardCount] instead of hardcoding 256. Bumping the
// constant in graph/mapper.go automatically widens the acceptance
// filter without touching this file.

// shardZeroKeyCachePath is the on-disk cache path relative to the
// shapegen package. It is exported as a const so tests can reference
// it without fishing for the path through filepath.Join.
const shardZeroKeyCachePath = "testdata/shapegen/mapper_shard0_keys.txt"

// shardZeroKeyCache is the in-memory amortisation layer that backs
// repeated GenerateShardZeroKeys calls within the same process. The
// slice is grown monotonically on the longest n requested so far;
// every shorter request is served by reslicing the prefix.
var shardZeroKeyCache struct {
	mu   sync.Mutex
	keys []string
}

// GenerateShardZeroKeys returns n distinct strings, every one of
// which hashes to mapper shard 0 under the FNV-1a routing scheme
// pinned by [graph.Mapper]. The returned slice is deterministic
// across processes and platforms.
//
// The first call materialises (or refreshes) the on-disk cache; later
// calls within the same process replay from memory in O(n).
//
// n must be > 0; the helper panics on n <= 0 because the catalogue
// has no use case for an empty corpus.
func GenerateShardZeroKeys(n int) []string {
	if n <= 0 {
		panic(fmt.Sprintf("shapegen: GenerateShardZeroKeys requires n > 0, got %d", n))
	}
	shardZeroKeyCache.mu.Lock()
	defer shardZeroKeyCache.mu.Unlock()

	if len(shardZeroKeyCache.keys) < n {
		shardZeroKeyCache.keys = ensureShardZeroKeys(n)
	}
	out := make([]string, n)
	copy(out, shardZeroKeyCache.keys[:n])
	return out
}

// ensureShardZeroKeys returns at least n shard-0 keys, hitting the
// on-disk cache when valid and regenerating it when stale or short.
// The function is internal to this file and is invoked under the
// in-memory cache lock.
func ensureShardZeroKeys(n int) []string {
	path := shardZeroKeyCacheAbsPath()
	cached, ok := loadShardZeroCache(path)
	if ok && len(cached) >= n && cacheIsValid(cached) {
		return cached
	}
	keys := computeShardZeroKeys(n)
	_ = saveShardZeroCache(path, keys)
	return keys
}

// shardZeroKeyCacheAbsPath resolves the package-relative cache path
// against the directory that hosts this source file. Using a
// runtime.Caller anchor keeps the resolution stable regardless of
// the cwd `go test` is invoked from.
func shardZeroKeyCacheAbsPath() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), shardZeroKeyCachePath)
}

// loadShardZeroCache reads the cache file at path and returns its
// contents one line per key. Returns (nil, false) when the file is
// absent or unreadable.
func loadShardZeroCache(path string) ([]string, bool) {
	f, err := os.Open(path) // #nosec G304 -- path is a fixed package-relative testdata file.
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var keys []string
	for scanner.Scan() {
		keys = append(keys, scanner.Text())
	}
	if scanner.Err() != nil {
		return nil, false
	}
	return keys, true
}

// cacheIsValid verifies that the first cached key still routes to
// shard 0 under the active hash configuration. Any drift in
// [graph.fnv1aString] or [graph.MapperShardCount] flips the bit and
// triggers full regeneration.
func cacheIsValid(keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	m := graph.NewMapper[string]()
	id := m.Intern(keys[0])
	return graph.MapperShardOf(id) == 0
}

// saveShardZeroCache writes the keys to path, one per line. The
// directory tree is created on demand so the function works from
// fresh checkouts. Errors are surfaced but never panic — a failure
// to save is harmless: subsequent invocations regenerate.
func saveShardZeroCache(path string, keys []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- testdata path inside the repo.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	for _, k := range keys {
		if _, err := w.WriteString(k); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	return w.Flush()
}

// computeShardZeroKeys brute-forces strings whose FNV-1a hash modulo
// the active mapper shard count equals 0. Candidate strings are
// "k-<i>" for i = 0, 1, 2, ...; the prefix is short to keep the inner
// loop cache-friendly and the suffix increments monotonically so the
// generated set is deterministic across runs.
//
// Acceptance probability is 1 / [graph.MapperShardCount], so n = 1e5
// requires roughly n * shardCount candidate evaluations. At shard
// count 256 this is ≈ 25 M evaluations; FNV-1a over 6-12 byte
// strings runs ≈ 100 Mops/s on x86_64, putting the worst-case
// generation cost at ≈ 250 ms.
func computeShardZeroKeys(n int) []string {
	keys := make([]string, 0, n)
	m := graph.NewMapper[string]()
	// Pre-size the candidate buffer so the inner loop avoids
	// strconv.Itoa's allocator path on every iteration.
	buf := make([]byte, 0, 16)
	for i := 0; len(keys) < n; i++ {
		buf = buf[:0]
		buf = append(buf, 'k', '-')
		buf = strconv.AppendInt(buf, int64(i), 10)
		s := string(buf)
		if graph.MapperShardOf(m.Intern(s)) == 0 {
			keys = append(keys, s)
		}
		// Safety lever: if the loop somehow runs away (shard count
		// changed mid-flight, hash bug, etc.) bound the cost so the
		// caller fails fast instead of hanging the suite.
		if i > 1_000*n+1_000_000 {
			panic(fmt.Sprintf(
				"shapegen: GenerateShardZeroKeys exceeded %d candidates while collecting %d keys (collected %d)",
				i, n, len(keys),
			))
		}
	}
	return keys
}
