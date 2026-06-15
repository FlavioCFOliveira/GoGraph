package adjlist

// handle_prefix_regression_test.go — regression coverage for the fast-path
// handle-column growth bug in upsertEdgeLocked (graph/adjlist/adjlist.go).
//
// A node that accrues several handle-less edges (AddEdge) leaves its handle
// column nil/short while its neighbour backing array grows and retains spare
// capacity. A subsequent handle-bearing edge (AddEdgeH) then takes the
// spare-capacity fast path with a still-short handle column. Before the fix
// the fast path sized the new column from len(current.handles) instead of
// oldLen, so growCap of the short length could be < newLen, panicking with
// "makeslice: cap out of range". This was a hard data-compatibility break:
// recovery (store/snapshot.ApplyCSRToGraph) re-inserts exactly this mix of
// handle-bearing and handle-less parallel edges per node, so any graph written
// before the regression could no longer be opened.
//
// Layer: short.

import "testing"

// TestAdjList_HandleAfterHandlelessPrefix_NoPanic reproduces the minimal
// sequence: a handle-less prefix long enough that the neighbour array has been
// grown (cap > len), followed by a handle-bearing append on the fast path. It
// must not panic, and the resulting handle column must stay length-aligned with
// neighbours — the leading (handle-less) slots are the 0 "no handle" sentinel
// and the handle-bearing slot carries its handle.
func TestAdjList_HandleAfterHandlelessPrefix_NoPanic(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})

	// Five handle-less edges: neighbours grows to len 5 / cap 8, while the
	// handle column stays nil (len 0).
	const handleless = 5
	for i := 0; i < handleless; i++ {
		dst := string(rune('a' + i))
		if err := a.AddEdge("hub", dst, 1); err != nil {
			t.Fatalf("AddEdge #%d: %v", i, err)
		}
	}

	// Handle-bearing edge on the spare-capacity fast path with a still-nil
	// handle column. Pre-fix this panicked with makeslice: cap out of range.
	const wantHandle = uint64(42)
	if err := a.AddEdgeH("hub", "z", 1, wantHandle); err != nil {
		t.Fatalf("AddEdgeH after handle-less prefix: %v", err)
	}

	nb, h := neighboursOf(t, a, "hub")
	if len(nb) != handleless+1 {
		t.Fatalf("neighbours len = %d, want %d", len(nb), handleless+1)
	}
	if len(h) != len(nb) {
		t.Fatalf("handles len = %d, want %d (aligned with neighbours)", len(h), len(nb))
	}
	for i := 0; i < handleless; i++ {
		if h[i] != 0 {
			t.Errorf("handle[%d] = %d, want 0 (handle-less sentinel)", i, h[i])
		}
	}
	if got := h[handleless]; got != wantHandle {
		t.Errorf("handle[%d] = %d, want %d", handleless, got, wantHandle)
	}
}

// TestAdjList_HandleAfterHandlelessPrefix_Degrees sweeps the handle-less prefix
// length across the geometric capacity boundaries (growCap min 4, then ×2) so
// the regression is caught whichever capacity tier the fast path lands in.
func TestAdjList_HandleAfterHandlelessPrefix_Degrees(t *testing.T) {
	t.Parallel()
	for _, prefix := range []int{1, 3, 4, 5, 7, 8, 9, 16, 17} {
		prefix := prefix
		t.Run("", func(t *testing.T) {
			t.Parallel()
			a := New[int, int](Config{Directed: true, Multigraph: true})
			for i := 0; i < prefix; i++ {
				if err := a.AddEdge(0, i+1, 1); err != nil {
					t.Fatalf("AddEdge #%d: %v", i, err)
				}
			}
			h := uint64(prefix + 100)
			if err := a.AddEdgeH(0, -1, 1, h); err != nil {
				t.Fatalf("AddEdgeH (prefix=%d): %v", prefix, err)
			}
			id, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatal("Lookup(0) missed")
			}
			nb, _, hs := a.LoadEntryH(id)
			if len(hs) != len(nb) {
				t.Fatalf("prefix=%d: handles len %d != neighbours len %d", prefix, len(hs), len(nb))
			}
			if hs[len(hs)-1] != h {
				t.Fatalf("prefix=%d: last handle = %d, want %d", prefix, hs[len(hs)-1], h)
			}
			for i := 0; i < prefix; i++ {
				if hs[i] != 0 {
					t.Fatalf("prefix=%d: handle[%d] = %d, want 0", prefix, i, hs[i])
				}
			}
		})
	}
}
