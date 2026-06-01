package btree_test

import (
	"math/rand/v2"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
)

// TestRange_AdversarialAscendingDescendingInsertion verifies that the
// index returns correct Range results after an adversarial insertion
// sequence: keys 0..N-1 in ascending order, then keys 2N-1..N in
// descending order (covering N..2N-1).
//
// N = 10000 — large enough to exercise any rebalancing paths.
func TestRange_AdversarialAscendingDescendingInsertion(t *testing.T) {
	t.Parallel()

	const n = 10000

	idx := btree.New[int64]()

	// Phase 1: ascending insertion 0..N-1.
	for k := int64(0); k < n; k++ {
		idx.Insert(k, graph.NodeID(k))
	}

	// Phase 2: descending insertion 2N-1..N (i.e., keys N..2N-1 in reverse).
	for k := int64(2*n - 1); k >= n; k-- {
		idx.Insert(k, graph.NodeID(k))
	}

	// Build oracle: key → nodeID for all 2N keys.
	oracle := make(map[int64]graph.NodeID, 2*n)
	for k := int64(0); k < 2*n; k++ {
		oracle[k] = graph.NodeID(k)
	}

	// Full range must contain all 2N NodeIDs.
	t.Run("full-range", func(t *testing.T) {
		t.Parallel()
		bm := idx.Range(0, 2*n-1)
		if bm.GetCardinality() != 2*n {
			t.Fatalf("full Range cardinality=%d want %d", bm.GetCardinality(), 2*n)
		}
		for k := int64(0); k < 2*n; k++ {
			if !bm.Contains(uint64(k)) {
				t.Errorf("full Range: missing NodeID %d", k)
			}
		}
	})

	// DistinctValues must equal 2N.
	t.Run("distinct-values", func(t *testing.T) {
		t.Parallel()
		if dv := idx.DistinctValues(); dv != 2*n {
			t.Fatalf("DistinctValues=%d want %d", dv, 2*n)
		}
	})

	// 100 random sub-ranges must match the oracle.
	t.Run("random-subranges", func(t *testing.T) {
		t.Parallel()

		r := rand.New(rand.NewPCG(12345, 0)) //nolint:gosec // deterministic test RNG

		oracleRange := func(lo, hi int64) map[graph.NodeID]struct{} {
			out := make(map[graph.NodeID]struct{})
			for k, id := range oracle {
				if k >= lo && k <= hi {
					out[id] = struct{}{}
				}
			}
			return out
		}

		for i := 0; i < 100; i++ {
			a := int64(r.Int64N(2 * n))
			b := int64(r.Int64N(2 * n))
			if a > b {
				a, b = b, a
			}
			want := oracleRange(a, b)
			got := idx.Range(a, b)

			if uint64(len(want)) != got.GetCardinality() {
				t.Errorf("iter %d Range(%d,%d): cardinality=%d want %d",
					i, a, b, got.GetCardinality(), len(want))
				continue
			}
			for id := range want {
				if !got.Contains(uint64(id)) {
					t.Errorf("iter %d Range(%d,%d): missing NodeID %d", i, a, b, id)
				}
			}
		}
	})
}
