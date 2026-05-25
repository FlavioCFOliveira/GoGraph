package hash_test

import (
	"testing"

	"gograph/graph"
	"gograph/graph/index/hash"
)

// TestPropertySingleton verifies the degenerate case where 50 000 nodes
// all share the same index value (age == 42). This stresses the roaring
// bitmap path and confirms DistinctValues stays at 1.
func TestPropertySingleton(t *testing.T) {
	t.Parallel()
	propertySingleton(t, 50_000)
}

// propertySingleton is the shared fixture used by both the short layer
// (n=50 000) and the soak layer (n=1 000 000).
func propertySingleton(t *testing.T, n int) {
	t.Helper()

	const value = int64(42)

	idx := hash.New[int64]()
	for i := 0; i < n; i++ {
		idx.Insert(value, graph.NodeID(i))
	}

	// Lookup(42) must return exactly [0, 1, ..., n-1] in sorted order.
	bm := idx.Lookup(value)
	got := bm.ToArray()
	if uint64(len(got)) != uint64(n) {
		t.Fatalf("Lookup(%d): cardinality = %d, want %d", value, len(got), n)
	}
	for i, v := range got {
		if v != uint64(i) {
			t.Fatalf("Lookup(%d)[%d] = %d, want %d", value, i, v, i)
		}
	}

	// Cardinality must match n.
	if c := idx.Cardinality(value); c != uint64(n) {
		t.Errorf("Cardinality(%d) = %d, want %d", value, c, n)
	}

	// Lookup of an absent value must return an empty bitmap (not nil/panic).
	bm2 := idx.Lookup(int64(43))
	if bm2.GetCardinality() != 0 {
		t.Errorf("Lookup(43) cardinality = %d, want 0", bm2.GetCardinality())
	}

	// DistinctValues must be 1 — only one key was ever inserted.
	if dv := idx.DistinctValues(); dv != 1 {
		t.Errorf("DistinctValues() = %d, want 1", dv)
	}
}
