package btree

import (
	"testing"

	"gograph/graph"
)

// TestIndex_RangeFirst covers the documented branches of RangeFirst:
// the trivial empty-range (hi < lo), the empty-index miss, the
// out-of-bounds miss when every key is below lo, the boundary-miss
// when the smallest matching key would exceed hi, and the happy path
// which must surface the smallest value and the minimum NodeID
// associated with it.
func TestIndex_RangeFirst(t *testing.T) {
	t.Parallel()

	t.Run("empty-index-miss", func(t *testing.T) {
		t.Parallel()
		idx := New[int]()
		if _, _, ok := idx.RangeFirst(0, 10); ok {
			t.Fatal("RangeFirst on empty index must return ok=false")
		}
	})

	t.Run("inverted-range", func(t *testing.T) {
		t.Parallel()
		idx := New[int]()
		idx.Insert(1, graph.NodeID(100))
		if _, _, ok := idx.RangeFirst(10, 1); ok {
			t.Fatal("RangeFirst with hi < lo must return ok=false")
		}
	})

	t.Run("below-lo-miss", func(t *testing.T) {
		t.Parallel()
		idx := New[int]()
		idx.Insert(1, graph.NodeID(100))
		idx.Insert(2, graph.NodeID(200))
		// All keys < 50; no candidate.
		if _, _, ok := idx.RangeFirst(50, 100); ok {
			t.Fatal("RangeFirst above every indexed key must return ok=false")
		}
	})

	t.Run("above-hi-miss", func(t *testing.T) {
		t.Parallel()
		idx := New[int]()
		idx.Insert(100, graph.NodeID(1))
		// Smallest key in the index (100) exceeds hi (50).
		if _, _, ok := idx.RangeFirst(0, 50); ok {
			t.Fatal("RangeFirst below every indexed key must return ok=false")
		}
	})

	t.Run("happy-path-first-key", func(t *testing.T) {
		t.Parallel()
		idx := New[int]()
		idx.Insert(5, graph.NodeID(50))
		idx.Insert(5, graph.NodeID(55))
		idx.Insert(7, graph.NodeID(70))
		idx.Insert(9, graph.NodeID(90))
		v, n, ok := idx.RangeFirst(5, 9)
		if !ok {
			t.Fatal("RangeFirst within range must return ok=true")
		}
		if v != 5 {
			t.Fatalf("RangeFirst value = %d, want 5", v)
		}
		// Minimum of {50, 55} is 50.
		if n != graph.NodeID(50) {
			t.Fatalf("RangeFirst node = %d, want 50", n)
		}
	})

	t.Run("happy-path-skip-below-lo", func(t *testing.T) {
		t.Parallel()
		idx := New[int]()
		idx.Insert(1, graph.NodeID(1))
		idx.Insert(5, graph.NodeID(50))
		idx.Insert(9, graph.NodeID(90))
		// lo=4 must skip the value 1 entry and surface 5.
		v, n, ok := idx.RangeFirst(4, 9)
		if !ok || v != 5 || n != graph.NodeID(50) {
			t.Fatalf("RangeFirst = (%d, %d, %v), want (5, 50, true)", v, n, ok)
		}
	})
}
