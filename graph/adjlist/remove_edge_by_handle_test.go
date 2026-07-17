package adjlist

// remove_edge_by_handle_test.go — unit coverage for AdjList.RemoveEdgeByHandle,
// the instance-precise parallel-slot removal introduced for rmp #2018.

import "testing"

// TestAdjList_RemoveEdgeByHandle_DirectedMultigraph removes the SECOND parallel
// slot by its handle and asserts the FIRST slot (a different handle) survives
// with its original handle preserved.
func TestAdjList_RemoveEdgeByHandle_DirectedMultigraph(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	if err := a.AddEdgeH("a", "b", 1, 10); err != nil {
		t.Fatalf("AddEdgeH h10: %v", err)
	}
	if err := a.AddEdgeH("a", "b", 2, 20); err != nil {
		t.Fatalf("AddEdgeH h20: %v", err)
	}
	if got := a.Size(); got != 2 {
		t.Fatalf("Size = %d, want 2", got)
	}

	// Remove the second parallel slot (handle 20); the first (handle 10) survives.
	if !a.RemoveEdgeByHandle("a", "b", 20) {
		t.Fatal("RemoveEdgeByHandle(20) returned false, want true")
	}
	if got := a.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1 after removing one of two parallel slots", got)
	}
	if !a.HasEdge("a", "b") {
		t.Fatal("a->b should still have one parallel slot")
	}

	// The single surviving slot must carry handle 10 (the first), not 20.
	srcID, ok := a.Mapper().Lookup("a")
	if !ok {
		t.Fatal("src 'a' not interned")
	}
	nbs, _, handles := a.LoadEntryH(srcID)
	if len(nbs) != 1 || len(handles) != 1 || handles[0] != 10 {
		t.Fatalf("surviving slot: neighbours=%v handles=%v, want single slot with handle 10", nbs, handles)
	}
}

// TestAdjList_RemoveEdgeByHandle_NoMatch confirms a handle with no matching
// slot is a no-op that returns false and leaves the edge counter unchanged.
func TestAdjList_RemoveEdgeByHandle_NoMatch(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	if err := a.AddEdgeH("a", "b", 1, 10); err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	if a.RemoveEdgeByHandle("a", "b", 999) {
		t.Fatal("RemoveEdgeByHandle(999) returned true, want false (no matching handle)")
	}
	if a.RemoveEdgeByHandle("a", "z", 10) { // unknown dst
		t.Fatal("RemoveEdgeByHandle on unknown dst returned true, want false")
	}
	if a.RemoveEdgeByHandle("z", "a", 10) { // unknown src
		t.Fatal("RemoveEdgeByHandle on unknown src returned true, want false")
	}
	if got := a.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1 (no-ops must not decrement)", got)
	}
}

// TestAdjList_RemoveEdgeByHandle_UndirectedMirror confirms both directions of an
// undirected parallel edge are retired by the same handle.
func TestAdjList_RemoveEdgeByHandle_UndirectedMirror(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: false, Multigraph: true})
	if err := a.AddEdgeH("a", "b", 1, 10); err != nil {
		t.Fatalf("AddEdgeH h10: %v", err)
	}
	if err := a.AddEdgeH("a", "b", 2, 20); err != nil {
		t.Fatalf("AddEdgeH h20: %v", err)
	}
	if !a.RemoveEdgeByHandle("a", "b", 20) {
		t.Fatal("RemoveEdgeByHandle(20) returned false, want true")
	}
	// One logical undirected edge removed: size decremented once.
	if got := a.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1", got)
	}
	// The surviving handle-10 edge is present in both directions.
	if !a.HasEdge("a", "b") || !a.HasEdge("b", "a") {
		t.Fatal("surviving undirected edge must be present in both directions")
	}
	srcID, _ := a.Mapper().Lookup("b")
	nbs, _, handles := a.LoadEntryH(srcID)
	if len(nbs) != 1 || handles[0] != 10 {
		t.Fatalf("mirror b->a slot: neighbours=%v handles=%v, want single slot handle 10", nbs, handles)
	}
}
