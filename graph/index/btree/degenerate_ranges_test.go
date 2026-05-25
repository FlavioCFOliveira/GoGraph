package btree_test

import (
	"math"
	"math/rand/v2"
	"testing"

	"gograph/graph"
	"gograph/graph/index/btree"
)

// TestRange_DegenerateRanges verifies three special range-query shapes
// against an index of 10000 random int64 keys drawn from [0, 100000).
//
// Shape 1 — empty range: a query whose bounds contain no indexed key.
// Shape 2 — single-key range: Range(k, k) for 10 sampled keys.
// Shape 3 — full range: Range(math.MinInt64, math.MaxInt64) covers all n keys.
func TestRange_DegenerateRanges(t *testing.T) {
	t.Parallel()

	const n = 10000

	r := rand.New(rand.NewPCG(42, 0)) //nolint:gosec // deterministic test RNG

	// Build oracle: key → nodeID (1:1 mapping; collisions use last nodeID for that key).
	oracle := make(map[int64]graph.NodeID, n)
	keys := make([]int64, 0, n)

	idx := btree.New[int64]()

	for i := 0; i < n; i++ {
		k := r.Int64N(100000)
		id := graph.NodeID(k)
		if _, exists := oracle[k]; !exists {
			keys = append(keys, k)
		}
		oracle[k] = id
		idx.Insert(k, id)
	}

	// Shape 1: empty range — use a gap guaranteed to contain no key.
	// Range(-1, -2) has hi < lo so it short-circuits to empty. Additionally,
	// no negative key was inserted (source domain is [0, 100000)), so
	// Range(-100, -1) also covers a key-free region.
	t.Run("empty-inverted", func(t *testing.T) {
		t.Parallel()
		bm := idx.Range(-1, -2) // hi < lo → always empty
		if !bm.IsEmpty() {
			t.Fatalf("Range(-1,-2): expected empty, got cardinality=%d", bm.GetCardinality())
		}
	})

	t.Run("empty-negative-region", func(t *testing.T) {
		t.Parallel()
		bm := idx.Range(-100, -1) // no keys in [-100, -1]
		if !bm.IsEmpty() {
			t.Fatalf("Range(-100,-1): expected empty, got cardinality=%d", bm.GetCardinality())
		}
	})

	// Shape 2: single-key range — Range(k, k) must contain exactly the
	// NodeID stored for k in the oracle.
	t.Run("single-key", func(t *testing.T) {
		t.Parallel()

		// Pick 10 keys deterministically from the set of inserted keys.
		pick := rand.New(rand.NewPCG(7, 3)) //nolint:gosec // deterministic test RNG
		for i := 0; i < 10; i++ {
			k := keys[pick.IntN(len(keys))]
			wantID := oracle[k]

			bm := idx.Range(k, k)
			if bm.IsEmpty() {
				t.Errorf("Range(%d,%d): got empty bitmap, want NodeID %d", k, k, wantID)
				continue
			}
			if !bm.Contains(uint64(wantID)) {
				t.Errorf("Range(%d,%d): missing NodeID %d", k, k, wantID)
			}
		}
	})

	// Shape 3: full range — Range(MinInt64, MaxInt64) must include all
	// distinct keys in the oracle (no gaps, no duplicates beyond what was
	// inserted with the same key).
	t.Run("full-range", func(t *testing.T) {
		t.Parallel()

		bm := idx.Range(math.MinInt64, math.MaxInt64)

		wantCard := uint64(len(oracle)) // distinct keys
		if bm.GetCardinality() != wantCard {
			t.Fatalf("full Range cardinality=%d want %d", bm.GetCardinality(), wantCard)
		}
		for _, id := range oracle {
			if !bm.Contains(uint64(id)) {
				t.Errorf("full Range: missing NodeID %d", id)
			}
		}
	})
}
