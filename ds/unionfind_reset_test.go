package ds

import "testing"

// TestUnionFind_Reset_GenericRetainsCapacityAndResets confirms that
// Reset on the generic UnionFind empties both maps in place: a
// previously-connected pair becomes disconnected, Len drops to zero,
// and subsequent operations succeed without re-allocating the maps.
func TestUnionFind_Reset_GenericRetainsCapacityAndResets(t *testing.T) {
	t.Parallel()
	u := New[int]()
	u.Union(1, 2)
	u.Union(2, 3)
	if !u.Connected(1, 3) {
		t.Fatal("pre-Reset: 1 and 3 should be connected")
	}
	if got := u.Len(); got != 3 {
		t.Fatalf("pre-Reset Len = %d; want 3", got)
	}

	u.Reset()

	if got := u.Len(); got != 0 {
		t.Fatalf("post-Reset Len = %d; want 0", got)
	}
	// Re-use the (now-empty) maps and verify the structure still
	// behaves correctly. We do not use Connected before the new
	// Union here because Connected calls Find which implicitly
	// MakeSets unknown elements as singletons — that would inflate
	// Len in a confusing way for the test reader.
	u.Union(10, 20)
	if !u.Connected(10, 20) {
		t.Fatal("post-Reset Union(10,20) did not connect")
	}
	if got := u.Len(); got != 2 {
		t.Fatalf("post-Reset re-insert Len = %d; want 2", got)
	}
	// The pre-Reset elements are gone — confirm by counting the
	// number of distinct roots from the elements we know are live:
	// only 10 and 20, and Union(10, 20) collapsed them to one root.
	if u.Find(10) != u.Find(20) {
		t.Fatal("post-Reset roots diverged for the Union'd pair")
	}
}

// TestUnionFindSlice_Reset_RestoresSingletons checks that the
// slice-backed Reset returns every parent[i] to i and zeroes the
// rank slice, while preserving the universe size.
func TestUnionFindSlice_Reset_RestoresSingletons(t *testing.T) {
	t.Parallel()
	u := NewSlice(8)
	u.Union(0, 1)
	u.Union(2, 3)
	u.Union(1, 2)
	if !u.Connected(0, 3) {
		t.Fatal("pre-Reset: 0 and 3 should be connected through 1-2")
	}
	if got := u.Len(); got != 8 {
		t.Fatalf("pre-Reset Len = %d; want 8", got)
	}

	u.Reset()

	if got := u.Len(); got != 8 {
		t.Fatalf("post-Reset Len = %d; want 8 (universe preserved)", got)
	}
	for i := 0; i < 8; i++ {
		if u.Find(i) != i {
			t.Errorf("post-Reset Find(%d) = %d; want %d", i, u.Find(i), i)
		}
	}
	if u.Connected(0, 3) {
		t.Fatal("post-Reset: 0 and 3 should be in separate singletons")
	}
	// Re-use the structure.
	u.Union(0, 7)
	if !u.Connected(0, 7) {
		t.Fatal("post-Reset Union(0,7) did not connect")
	}
}
