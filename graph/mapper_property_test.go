package graph

import (
	"sort"
	"testing"

	"pgregory.net/rapid"
)

// TestMapper_InsertionOrderIndependent asserts that two Mappers populated
// with the same key set in different orders assign each key to the same
// shard. The shard byte of a NodeID is a deterministic function of the
// key content (FNV-1a hash), so it must not vary with insertion order.
//
// The intra-shard index, on the other hand, encodes position of first
// insertion within that shard; it therefore legitimately differs when the
// same keys arrive in a different order. This test does NOT assert NodeID
// equality — it only asserts shard-byte equality and multiset equivalence
// of the shard-byte distribution across the two Mappers.
func TestMapper_InsertionOrderIndependent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 200).Draw(rt, "n")

		// Generate n distinct int keys: 0 … n-1.
		keys := make([]int, n)
		for i := range keys {
			keys[i] = i
		}

		// Mapper 1: keys inserted in natural order.
		m1 := NewMapper[int]()
		ids1 := make(map[int]NodeID, n)
		for _, k := range keys {
			ids1[k] = m1.Intern(k)
		}

		// Mapper 2: keys inserted in reversed order (a deterministic
		// permutation that is never equal to the natural order for n > 1).
		m2 := NewMapper[int]()
		ids2 := make(map[int]NodeID, n)
		for i := len(keys) - 1; i >= 0; i-- {
			ids2[keys[i]] = m2.Intern(keys[i])
		}

		// Per-key invariant: the shard byte must agree across both Mappers
		// because it is derived solely from the key content.
		for _, k := range keys {
			shard1 := MapperShardOf(ids1[k])
			shard2 := MapperShardOf(ids2[k])
			if shard1 != shard2 {
				rt.Errorf("key %d: shard1=%d shard2=%d (insertion order changed shard)", k, shard1, shard2)
			}
		}

		// Multiset invariant: the set of {shard byte} values (one per key)
		// must be identical regardless of insertion order because the same
		// keys produce the same shard assignments.
		shards1 := make([]int, n)
		shards2 := make([]int, n)
		for i, k := range keys {
			shards1[i] = int(MapperShardOf(ids1[k]))
			shards2[i] = int(MapperShardOf(ids2[k]))
		}
		sort.Ints(shards1)
		sort.Ints(shards2)
		for i := range shards1 {
			if shards1[i] != shards2[i] {
				rt.Errorf("shard distribution mismatch at index %d: %d vs %d",
					i, shards1[i], shards2[i])
				break
			}
		}

		// Lookup invariant: after all insertions, Lookup by key must return
		// the NodeID that Intern originally assigned in the same Mapper.
		for _, k := range keys {
			if got, ok := m1.Lookup(k); !ok || got != ids1[k] {
				rt.Errorf("m1.Lookup(%d) = (%d, %v), want (%d, true)", k, got, ok, ids1[k])
			}
			if got, ok := m2.Lookup(k); !ok || got != ids2[k] {
				rt.Errorf("m2.Lookup(%d) = (%d, %v), want (%d, true)", k, got, ok, ids2[k])
			}
		}
	})
}
