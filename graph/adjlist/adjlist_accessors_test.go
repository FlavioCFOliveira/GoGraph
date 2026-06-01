package adjlist

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestAdjList_Accessors covers Mapper, Directed, Multigraph, and
// MaxNodeID. These are trivial reflection helpers but they sit on
// the public surface so their behaviour must be pinned.
func TestAdjList_Accessors(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true, Multigraph: true})
	if a.Mapper() == nil {
		t.Fatal("Mapper() returned nil")
	}
	if !a.Directed() {
		t.Fatal("Directed() returned false for Directed:true config")
	}
	if !a.Multigraph() {
		t.Fatal("Multigraph() returned false for Multigraph:true config")
	}
	if got := a.MaxNodeID(); got != 0 {
		t.Fatalf("empty AdjList MaxNodeID = %d, want 0", got)
	}
	mustAddNode(t, a, "alice")
	if got := a.MaxNodeID(); got == 0 {
		t.Fatal("MaxNodeID still 0 after first AddNode")
	}
	// Mapper round-trip via accessor.
	id, ok := a.Mapper().Lookup("alice")
	if !ok {
		t.Fatal("Mapper().Lookup(alice) missed")
	}
	if uint64(id) >= uint64(a.MaxNodeID()) {
		t.Fatalf("id %d violates MaxNodeID %d upper bound", id, a.MaxNodeID())
	}
}

// TestAdjList_AccessorsDefault covers the false-returning branches of
// Directed and Multigraph on the zero Config.
func TestAdjList_AccessorsDefault(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{})
	if a.Directed() {
		t.Fatal("Directed() returned true for zero Config")
	}
	if a.Multigraph() {
		t.Fatal("Multigraph() returned true for zero Config")
	}
}

// TestAdjList_LoadEntry_AllPaths covers every documented branch of
// LoadEntry: a node that has never had outgoing edges, a node with
// outgoing edges, and an out-of-range NodeID.
func TestAdjList_LoadEntry_AllPaths(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	mustAddEdge(t, a, "a", "b", 7)
	mustAddEdge(t, a, "a", "c", 8)
	mustAddNode(t, a, "solitary")

	idA, _ := a.Mapper().Lookup("a")
	nb, ws := a.LoadEntry(idA)
	if len(nb) != 2 || len(ws) != 2 {
		t.Fatalf("LoadEntry(a) lengths nb=%d ws=%d, want 2/2", len(nb), len(ws))
	}
	idSol, _ := a.Mapper().Lookup("solitary")
	nb, ws = a.LoadEntry(idSol)
	if nb != nil || ws != nil {
		t.Fatalf("LoadEntry(solitary) = (%v, %v), want (nil, nil)", nb, ws)
	}
	// Out-of-range id: build a NodeID that is definitely past the
	// current MaxNodeID.
	nb, ws = a.LoadEntry(graph.NodeID(uint64(a.MaxNodeID()) + 1<<20))
	if nb != nil || ws != nil {
		t.Fatalf("LoadEntry(huge) = (%v, %v), want (nil, nil)", nb, ws)
	}
}

// TestAdjList_Compact_NoOp documents the v1 contract: Compact is a
// no-op and does not alter observable state.
func TestAdjList_Compact_NoOp(t *testing.T) {
	t.Parallel()
	a := New[string, int](Config{Directed: true})
	mustAddEdge(t, a, "a", "b", 1)
	beforeOrder := a.Order()
	beforeSize := a.Size()
	a.Compact()
	if a.Order() != beforeOrder {
		t.Fatalf("Compact changed Order: %d -> %d", beforeOrder, a.Order())
	}
	if a.Size() != beforeSize {
		t.Fatalf("Compact changed Size: %d -> %d", beforeSize, a.Size())
	}
	if !a.HasEdge("a", "b") {
		t.Fatal("Compact removed a live edge")
	}
}
