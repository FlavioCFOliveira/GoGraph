package hash_test

import (
	"math/rand/v2"
	"sort"
	"testing"

	"gograph/graph"
	"gograph/graph/index/hash"
)

// TestShard0Storm builds an Index[int64] with 65 536 sequential keys,
// each mapped to 3 NodeIDs, then verifies Lookup/Cardinality/Contains
// on 100 random samples, checks DistinctValues, and validates that
// deleting half the keys leaves the oracle consistent.
func TestShard0Storm(t *testing.T) {
	t.Parallel()

	const (
		keys        = 65_536
		nodesPerKey = 3
	)

	idx := hash.New[int64]()
	oracle := make(map[int64][]graph.NodeID, keys)

	for k := int64(0); k < keys; k++ {
		for j := int64(0); j < nodesPerKey; j++ {
			node := graph.NodeID(k*nodesPerKey + j)
			idx.Insert(k, node)
			oracle[k] = append(oracle[k], node)
		}
	}

	// Full distinct-values check before any deletions.
	if dv := idx.DistinctValues(); dv != keys {
		t.Fatalf("DistinctValues() = %d, want %d", dv, keys)
	}

	// Verify 100 random samples against the oracle.
	rng := rand.New(rand.NewPCG(0xdeadbeef, 0xcafe)) //nolint:gosec // deterministic test RNG
	shard0StormVerifySamples(t, idx, oracle, rng, 100)

	// Delete half the keys (even keys only) and update oracle.
	for k := int64(0); k < keys; k += 2 {
		for j := int64(0); j < nodesPerKey; j++ {
			node := graph.NodeID(k*nodesPerKey + j)
			idx.Delete(k, node)
		}
		delete(oracle, k)
	}

	// After deletion, DistinctValues must equal keys/2.
	if dv := idx.DistinctValues(); dv != keys/2 {
		t.Fatalf("DistinctValues() after delete = %d, want %d", dv, keys/2)
	}

	// Re-verify 100 random samples on surviving (odd) keys.
	rng2 := rand.New(rand.NewPCG(0xfeedface, 0xbabe)) //nolint:gosec // deterministic test RNG
	shard0StormVerifySamples(t, idx, oracle, rng2, 100)

	// Deleted keys must now return empty bitmaps and zero cardinality.
	for k := int64(0); k < keys; k += 2 {
		bm := idx.Lookup(k)
		if bm.GetCardinality() != 0 {
			t.Errorf("Lookup(%d) after delete: cardinality = %d, want 0", k, bm.GetCardinality())
		}
		if c := idx.Cardinality(k); c != 0 {
			t.Errorf("Cardinality(%d) after delete = %d, want 0", k, c)
		}
	}
}

// shard0StormVerifySamples picks sampleCount random keys from oracle and
// checks Lookup, Cardinality, and Contains against it.
func shard0StormVerifySamples(
	t *testing.T,
	idx *hash.Index[int64],
	oracle map[int64][]graph.NodeID,
	rng *rand.Rand,
	sampleCount int,
) {
	t.Helper()

	// Collect oracle keys for random selection.
	keys := make([]int64, 0, len(oracle))
	for k := range oracle {
		keys = append(keys, k)
	}

	for s := 0; s < sampleCount && len(keys) > 0; s++ {
		key := keys[rng.IntN(len(keys))]
		wantNodes := oracle[key]

		want := make([]graph.NodeID, len(wantNodes))
		copy(want, wantNodes)
		sort.Slice(want, func(a, b int) bool { return want[a] < want[b] })

		bm := idx.Lookup(key)
		got := bm.ToArray()

		if uint64(len(got)) != uint64(len(want)) {
			t.Errorf("sample key=%d: Lookup len = %d, want %d", key, len(got), len(want))
			continue
		}
		for j, w := range want {
			if graph.NodeID(got[j]) != w {
				t.Errorf("sample key=%d: Lookup[%d] = %d, want %d", key, j, got[j], w)
			}
		}

		if c := idx.Cardinality(key); c != uint64(len(want)) {
			t.Errorf("sample key=%d: Cardinality = %d, want %d", key, c, len(want))
		}

		for _, node := range want {
			if !idx.Contains(key, node) {
				t.Errorf("sample key=%d: Contains(%d) = false, want true", key, node)
			}
		}
	}
}
