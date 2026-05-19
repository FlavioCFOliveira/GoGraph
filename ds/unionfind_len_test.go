package ds

import "testing"

// TestUnionFind_Len covers Len on the generic map-backed UnionFind:
// it returns 0 for an empty structure, grows as new elements are
// introduced (via MakeSet, Find, Union, or Connected), and does not
// double-count repeat references.
func TestUnionFind_Len(t *testing.T) {
	t.Parallel()
	u := New[int]()
	if got := u.Len(); got != 0 {
		t.Fatalf("empty Len = %d, want 0", got)
	}

	u.MakeSet(1)
	u.MakeSet(2)
	u.MakeSet(2) // idempotent
	if got := u.Len(); got != 2 {
		t.Fatalf("after MakeSet 1,2,2 Len = %d, want 2", got)
	}

	// Find adds a singleton implicitly for an unknown element.
	u.Find(3)
	if got := u.Len(); got != 3 {
		t.Fatalf("after Find(3) Len = %d, want 3", got)
	}

	// Union merges representatives but does not change the count of
	// known elements.
	u.Union(1, 2)
	if got := u.Len(); got != 3 {
		t.Fatalf("after Union(1,2) Len = %d, want 3", got)
	}

	// Connected also adds singletons implicitly.
	u.Connected(4, 5)
	if got := u.Len(); got != 5 {
		t.Fatalf("after Connected(4,5) Len = %d, want 5", got)
	}
}
