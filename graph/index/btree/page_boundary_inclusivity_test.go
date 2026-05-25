package btree_test

import (
	"testing"

	"gograph/graph"
	"gograph/graph/index/btree"
)

// TestRange_PageBoundaryInclusivity verifies that Range(lo, hi) honours
// inclusive bounds at both endpoints when the bound values coincide with
// keys that span what would be internal page boundaries in a tree-backed
// implementation.
//
// The index contains N=2000 sequential int64 keys: 0, 100, 200, …, (N-1)*100.
// Each key k maps to exactly one NodeID = graph.NodeID(k).
func TestRange_PageBoundaryInclusivity(t *testing.T) {
	t.Parallel()

	const n = 2000
	const step = int64(100)

	// Build oracle: key → nodeID.
	oracle := make(map[int64]graph.NodeID, n)
	for i := int64(0); i < n; i++ {
		oracle[i*step] = graph.NodeID(i * step)
	}

	idx := btree.New[int64]()
	for k, id := range oracle {
		idx.Insert(k, id)
	}

	// oracleRange returns the set of NodeIDs whose key k satisfies lo <= k <= hi.
	oracleRange := func(lo, hi int64) map[graph.NodeID]struct{} {
		out := make(map[graph.NodeID]struct{})
		for k, id := range oracle {
			if k >= lo && k <= hi {
				out[id] = struct{}{}
			}
		}
		return out
	}

	cases := []struct {
		name string
		lo   int64
		hi   int64
	}{
		{"lo=0 hi=100", 0, 100},
		{"lo=100 hi=200", 100, 200},
		{"lo=50 hi=150", 50, 150},
		{"single key 0", 0, 0},
		{"single key 200", 200, 200},
		{"no negative keys", -1, -1},
		{"above all keys", 999999, 1000000},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := oracleRange(tc.lo, tc.hi)
			got := idx.Range(tc.lo, tc.hi)

			// Cardinality must match.
			if uint64(len(want)) != got.GetCardinality() {
				t.Fatalf("Range(%d,%d): cardinality=%d want %d",
					tc.lo, tc.hi, got.GetCardinality(), len(want))
			}

			// Every expected NodeID must be present.
			for id := range want {
				if !got.Contains(uint64(id)) {
					t.Errorf("Range(%d,%d): missing NodeID %d", tc.lo, tc.hi, id)
				}
			}

			// RangeFirst must agree with the first element of Range.
			v, firstID, ok := idx.RangeFirst(tc.lo, tc.hi)
			if len(want) == 0 {
				if ok {
					t.Errorf("RangeFirst(%d,%d): got ok=true on empty range", tc.lo, tc.hi)
				}
			} else {
				if !ok {
					t.Fatalf("RangeFirst(%d,%d): got ok=false on non-empty range", tc.lo, tc.hi)
				}
				// v must be a key present in oracle within [lo, hi].
				id, exists := oracle[v]
				if !exists {
					t.Errorf("RangeFirst(%d,%d): returned value %d not in oracle", tc.lo, tc.hi, v)
				}
				if id != firstID {
					t.Errorf("RangeFirst(%d,%d): nodeID=%d want %d", tc.lo, tc.hi, firstID, id)
				}
				if !got.Contains(uint64(firstID)) {
					t.Errorf("RangeFirst(%d,%d): nodeID %d not in Range result", tc.lo, tc.hi, firstID)
				}
			}
		})
	}
}
