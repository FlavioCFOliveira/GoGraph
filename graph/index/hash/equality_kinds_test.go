package hash_test

import (
	"fmt"
	"math"
	"sort"
	"testing"

	"gograph/graph"
	"gograph/graph/index/hash"
)

// TestEqualityKinds_String verifies Lookup/Cardinality/Contains correctness
// for an Index[string] built from 10 000 deterministic (key, nodeID) pairs.
func TestEqualityKinds_String(t *testing.T) {
	t.Parallel()
	const n = 10_000

	idx := hash.New[string]()
	oracle := make(map[string][]graph.NodeID, n)

	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k%d", i)
		node := graph.NodeID(i)
		idx.Insert(key, node)
		oracle[key] = append(oracle[key], node)
	}

	equalityKindsVerify(t, idx, oracle)

	// Lookup of a key never inserted must return empty bitmap, not panic.
	bm := idx.Lookup("k_never_inserted")
	if bm.GetCardinality() != 0 {
		t.Errorf("Lookup(missing key): cardinality = %d, want 0", bm.GetCardinality())
	}
}

// TestEqualityKinds_Int64 verifies Lookup/Cardinality/Contains correctness
// for an Index[int64] built from 10 000 deterministic (key, nodeID) pairs.
func TestEqualityKinds_Int64(t *testing.T) {
	t.Parallel()
	const n = 10_000

	idx := hash.New[int64]()
	oracle := make(map[int64][]graph.NodeID, n)

	for i := 0; i < n; i++ {
		key := int64(i)
		node := graph.NodeID(i)
		idx.Insert(key, node)
		oracle[key] = append(oracle[key], node)
	}

	equalityKindsVerifyInt64(t, idx, oracle)

	// Lookup of a key never inserted must return empty bitmap, not panic.
	bm := idx.Lookup(int64(-1))
	if bm.GetCardinality() != 0 {
		t.Errorf("Lookup(missing key): cardinality = %d, want 0", bm.GetCardinality())
	}
}

// TestEqualityKinds_Float64 verifies Lookup/Cardinality/Contains correctness
// for an Index[float64] built from 10 000 deterministic (key, nodeID) pairs.
func TestEqualityKinds_Float64(t *testing.T) {
	t.Parallel()
	const n = 10_000

	idx := hash.New[float64]()
	oracle := make(map[float64][]graph.NodeID, n)

	for i := 0; i < n; i++ {
		key := float64(i) * math.Pi
		node := graph.NodeID(i)
		idx.Insert(key, node)
		oracle[key] = append(oracle[key], node)
	}

	equalityKindsVerifyFloat64(t, idx, oracle)

	// Lookup of a key never inserted must return empty bitmap, not panic.
	bm := idx.Lookup(-math.Pi)
	if bm.GetCardinality() != 0 {
		t.Errorf("Lookup(missing key): cardinality = %d, want 0", bm.GetCardinality())
	}
}

// equalityKindsVerify checks Lookup/Cardinality/Contains for Index[string].
func equalityKindsVerify(t *testing.T, idx *hash.Index[string], oracle map[string][]graph.NodeID) {
	t.Helper()
	for key, nodes := range oracle {
		// Sort oracle set for deterministic comparison.
		want := make([]graph.NodeID, len(nodes))
		copy(want, nodes)
		sort.Slice(want, func(a, b int) bool { return want[a] < want[b] })

		bm := idx.Lookup(key)
		got := bm.ToArray()

		if uint64(len(got)) != uint64(len(want)) {
			t.Errorf("Lookup(%q): len = %d, want %d", key, len(got), len(want))
			continue
		}
		for j, w := range want {
			if graph.NodeID(got[j]) != w {
				t.Errorf("Lookup(%q)[%d] = %d, want %d", key, j, got[j], w)
			}
		}

		if c := idx.Cardinality(key); c != uint64(len(want)) {
			t.Errorf("Cardinality(%q) = %d, want %d", key, c, len(want))
		}

		for _, node := range want {
			if !idx.Contains(key, node) {
				t.Errorf("Contains(%q, %d) = false, want true", key, node)
			}
		}
	}
}

// equalityKindsVerifyInt64 checks Lookup/Cardinality/Contains for Index[int64].
func equalityKindsVerifyInt64(t *testing.T, idx *hash.Index[int64], oracle map[int64][]graph.NodeID) {
	t.Helper()
	for key, nodes := range oracle {
		want := make([]graph.NodeID, len(nodes))
		copy(want, nodes)
		sort.Slice(want, func(a, b int) bool { return want[a] < want[b] })

		bm := idx.Lookup(key)
		got := bm.ToArray()

		if uint64(len(got)) != uint64(len(want)) {
			t.Errorf("Lookup(%d): len = %d, want %d", key, len(got), len(want))
			continue
		}
		for j, w := range want {
			if graph.NodeID(got[j]) != w {
				t.Errorf("Lookup(%d)[%d] = %d, want %d", key, j, got[j], w)
			}
		}

		if c := idx.Cardinality(key); c != uint64(len(want)) {
			t.Errorf("Cardinality(%d) = %d, want %d", key, c, len(want))
		}

		for _, node := range want {
			if !idx.Contains(key, node) {
				t.Errorf("Contains(%d, %d) = false, want true", key, node)
			}
		}
	}
}

// equalityKindsVerifyFloat64 checks Lookup/Cardinality/Contains for Index[float64].
func equalityKindsVerifyFloat64(t *testing.T, idx *hash.Index[float64], oracle map[float64][]graph.NodeID) {
	t.Helper()
	for key, nodes := range oracle {
		want := make([]graph.NodeID, len(nodes))
		copy(want, nodes)
		sort.Slice(want, func(a, b int) bool { return want[a] < want[b] })

		bm := idx.Lookup(key)
		got := bm.ToArray()

		if uint64(len(got)) != uint64(len(want)) {
			t.Errorf("Lookup(%v): len = %d, want %d", key, len(got), len(want))
			continue
		}
		for j, w := range want {
			if graph.NodeID(got[j]) != w {
				t.Errorf("Lookup(%v)[%d] = %d, want %d", key, j, got[j], w)
			}
		}

		if c := idx.Cardinality(key); c != uint64(len(want)) {
			t.Errorf("Cardinality(%v) = %d, want %d", key, c, len(want))
		}

		for _, node := range want {
			if !idx.Contains(key, node) {
				t.Errorf("Contains(%v, %d) = false, want true", key, node)
			}
		}
	}
}
