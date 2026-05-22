package adjlist

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// constantShardKey is a comparable type whose hash maps to a fixed
// mapper shard regardless of value. It is used by the cap-enforcement
// tests to deterministically funnel every interned identifier into a
// single shard, so the bounded-growth behaviour of
// [Config.MaxShardCapacity] can be exercised without depending on the
// hash distribution of arbitrary keys.
//
// We do this by hashing on a dedicated wrapper type: maphash.Comparable
// reads the value's bytes, so we collapse the routing to one shard by
// always returning the same encoded representation. Because the
// underlying field still differentiates equal values for map keys, the
// mapper continues to assign distinct NodeIDs.
//
// The full deterministic-routing trick is achieved by walking through
// successive distinct keys until we have observed enough on a chosen
// shard. We keep the choice local to each test so test ordering is
// irrelevant.
//
// In practice, picking keys whose hash lands on a chosen mapper shard
// is the simplest reliable approach: we do not need an exotic key
// type, just enough trial keys.

// findShardKeys returns count distinct int keys that all route to the
// adjlist shard chosen by the test (shard 0 by convention). The keys
// are deterministic for a given mapperSeed across the lifetime of a
// process.
func findShardKeys(t *testing.T, want int) []int {
	t.Helper()
	a := New[int, struct{}](Config{Directed: true})
	keys := make([]int, 0, want)
	for i := 0; len(keys) < want && i < 1_000_000; i++ {
		id := a.Mapper().Intern(i)
		if uint64(id)&shardMask == 0 {
			keys = append(keys, i)
		}
	}
	if len(keys) < want {
		t.Fatalf("could not find %d keys routing to shard 0 (got %d)", want, len(keys))
	}
	return keys
}

func TestAdjList_MaxShardCapacity_RejectsBeyondCap(t *testing.T) {
	t.Parallel()
	const cap = 4
	keys := findShardKeys(t, cap+1)

	a := New[int, struct{}](Config{Directed: true, MaxShardCapacity: cap})

	// First `cap` distinct nodes with edges out of them must succeed:
	// each one allocates a slot in shard 0.
	for i := 0; i < cap; i++ {
		if err := a.AddEdge(keys[i], keys[i], struct{}{}); err != nil {
			t.Fatalf("AddEdge[%d] returned err on cap-bounded path: %v", i, err)
		}
	}

	// The (cap+1)-th distinct source must trip the cap. Its intra-shard
	// index is exactly cap, which equals the cap limit, so growth past
	// it must be rejected.
	err := a.AddEdge(keys[cap], keys[cap], struct{}{})
	if err == nil {
		t.Fatalf("AddEdge past MaxShardCapacity returned nil; want ErrShardFull")
	}
	if !errors.Is(err, ErrShardFull) {
		t.Fatalf("err = %v; want errors.Is(err, ErrShardFull) == true", err)
	}

	// The failed call must not have mutated the size counter.
	if got := a.Size(); got != uint64(cap) {
		t.Fatalf("Size after rejected AddEdge = %d; want %d", got, cap)
	}
}

func TestAdjList_MaxShardCapacity_ZeroIsUnbounded(t *testing.T) {
	t.Parallel()
	a := New[int, struct{}](Config{Directed: true, MaxShardCapacity: 0})

	// Push well past any plausible "default" cap to confirm the
	// zero-value config really is uncapped.
	keys := findShardKeys(t, initialShardCap*8)
	for i, k := range keys {
		if err := a.AddEdge(k, k, struct{}{}); err != nil {
			t.Fatalf("AddEdge[%d] returned err under uncapped config: %v", i, err)
		}
	}
}

func TestAdjList_MaxShardCapacity_DefaultsAreUnbounded(t *testing.T) {
	t.Parallel()
	a := New[int, struct{}](Config{Directed: true})
	keys := findShardKeys(t, initialShardCap*4)
	for i, k := range keys {
		if err := a.AddEdge(k, k, struct{}{}); err != nil {
			t.Fatalf("AddEdge[%d] returned err on default Config: %v", i, err)
		}
	}
}

func TestAdjList_MaxShardCapacity_OtherShardsUnaffected(t *testing.T) {
	t.Parallel()
	const cap = 4
	saturating := findShardKeys(t, cap+1)

	a := New[int, struct{}](Config{Directed: true, MaxShardCapacity: cap})

	// Saturate shard 0.
	for i := 0; i < cap; i++ {
		if err := a.AddEdge(saturating[i], saturating[i], struct{}{}); err != nil {
			t.Fatalf("saturating shard 0 at i=%d: %v", i, err)
		}
	}
	if err := a.AddEdge(saturating[cap], saturating[cap], struct{}{}); !errors.Is(err, ErrShardFull) {
		t.Fatalf("expected ErrShardFull on shard 0; got %v", err)
	}

	// Now add edges whose source maps to OTHER shards; these must
	// succeed. We brute-force pick keys until we land on shards != 0.
	added := 0
	for i := 0; i < 100_000 && added < 16; i++ {
		// Use a value far away from anything findShardKeys returned to
		// avoid collisions with already-interned identifiers.
		k := 2_000_000 + i
		id := a.Mapper().Intern(k)
		if uint64(id)&shardMask == 0 {
			continue // skip shard 0
		}
		if err := a.AddEdge(k, k, struct{}{}); err != nil {
			t.Fatalf("AddEdge on unsaturated shard returned err: %v", err)
		}
		added++
	}
	if added == 0 {
		t.Fatal("could not find any keys routing to shards != 0")
	}
}

func TestAdjList_MaxShardCapacity_ConcurrentWriters(t *testing.T) {
	t.Parallel()
	const (
		cap         = 8
		goroutines  = 16
		insertsEach = 64
	)
	a := New[int, struct{}](Config{Directed: true, MaxShardCapacity: cap, Multigraph: true})

	// Pre-discover saturating keys for shard 0; concurrent writers all
	// try to add their edges using those keys, so most calls will be
	// served by the same shard.
	keys := findShardKeys(t, cap+8)

	var wg sync.WaitGroup
	var fullErrors atomic.Int64
	var unexpectedErrors atomic.Int64
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < insertsEach; i++ {
				k := keys[i%len(keys)]
				err := a.AddEdge(k, k, struct{}{})
				switch {
				case err == nil:
					// ok
				case errors.Is(err, ErrShardFull):
					fullErrors.Add(1)
				default:
					unexpectedErrors.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := unexpectedErrors.Load(); got != 0 {
		t.Fatalf("observed %d unexpected (non-ErrShardFull) errors", got)
	}
	// Some attempts must have hit the cap; otherwise the test is not
	// exercising the contract.
	if fullErrors.Load() == 0 {
		t.Fatal("expected at least one ErrShardFull under contention; got none")
	}
}
