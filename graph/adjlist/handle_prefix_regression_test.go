package adjlist

// handle_prefix_regression_test.go — regression coverage for the fast-path
// handle-column growth bug in upsertEdgeLocked (graph/adjlist/adjlist.go).
//
// A node that accrued several handle-less edges (AddEdge) left its handle
// column nil/short while its neighbour backing array grew and retained spare
// capacity. A subsequent handle-bearing edge (AddEdgeH) then took the
// spare-capacity fast path with a still-short handle column. Before the fix
// the fast path sized the new column from len(current.handles) instead of
// oldLen, so growCap of the short length could be < newLen, panicking with
// "makeslice: cap out of range". This was a hard data-compatibility break:
// recovery (store/snapshot.ApplyCSRToGraph) re-inserts exactly this mix of
// handle-bearing and handle-less parallel edges per node, so any graph written
// before the regression could no longer be opened.
//
// # Since rmp #2317 the short column is UNREACHABLE, and these tests say so
//
// Every slot now carries a handle: AddEdge mints one rather than leaving the 0
// sentinel, so the column is allocated with the first edge and is always
// length-aligned with neighbours. The mixed sequence below can therefore no
// longer produce a short column at all.
//
// The tests are kept, and re-aimed at the invariant that replaced the sentinel:
// mixing minted and caller-supplied handles must keep the column aligned, must
// preserve a supplied handle VERBATIM (recovery re-stamps the handle the WAL
// recorded), and must never hand two slots the same identity.
//
// Layer: short.

import "testing"

// TestAdjList_HandleAfterHandlelessPrefix_NoPanic reproduces the minimal
// sequence: a prefix of minted-handle edges long enough that the neighbour array
// has been grown (cap > len), followed by a caller-supplied-handle append on the
// fast path. It must not panic, the handle column must stay length-aligned with
// neighbours, every slot must carry a non-zero handle, and the supplied one must
// arrive verbatim.
func TestAdjList_HandleAfterHandlelessPrefix_NoPanic(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})

	// Five auto-minted edges: neighbours grows to len 5 / cap 8, and the handle
	// column grows with it.
	const handleless = 5
	for i := 0; i < handleless; i++ {
		dst := string(rune('a' + i))
		if err := a.AddEdge("hub", dst, 1); err != nil {
			t.Fatalf("AddEdge #%d: %v", i, err)
		}
	}

	// Caller-supplied handle on the spare-capacity fast path. Pre-fix, against a
	// still-nil column, this panicked with makeslice: cap out of range.
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
	seen := make(map[uint64]int, len(h))
	for i, got := range h {
		if got == 0 {
			t.Errorf("handle[%d] = 0: every slot must carry an identity (rmp #2317)", i)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("handle[%d] = %d duplicates slot %d: identities must be distinct", i, got, prev)
		}
		seen[got] = i
	}
	if got := h[handleless]; got != wantHandle {
		t.Errorf("handle[%d] = %d, want %d (a supplied handle is preserved verbatim)",
			handleless, got, wantHandle)
	}
}

// TestAdjList_HandleAfterHandlelessPrefix_Degrees sweeps the minted-handle prefix
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
				if hs[i] == 0 {
					t.Fatalf("prefix=%d: handle[%d] = 0: every slot must carry an identity", prefix, i)
				}
				if hs[i] == h {
					t.Fatalf("prefix=%d: minted handle[%d] collided with the supplied handle %d",
						prefix, i, h)
				}
			}
		})
	}
}
