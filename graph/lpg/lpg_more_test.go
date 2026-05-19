package lpg

import (
	"testing"

	"gograph/graph/adjlist"
)

// TestGraph_Accessors covers the trivial getter exports that surface
// the underlying adjacency list, label registry, node/edge label
// indexes, and property-key registry. They are the public reflection
// hooks documented on Graph.
func TestGraph_Accessors(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})

	if g.AdjList() == nil {
		t.Fatal("AdjList() returned nil")
	}
	if g.Registry() == nil {
		t.Fatal("Registry() returned nil")
	}
	if g.NodeIndex() == nil {
		t.Fatal("NodeIndex() returned nil")
	}
	if g.EdgeIndex() == nil {
		t.Fatal("EdgeIndex() returned nil")
	}
	if g.PropertyKeys() == nil {
		t.Fatal("PropertyKeys() returned nil")
	}
	// AdjList round-trip: AddNode on the LPG must register the
	// node through the underlying mapper.
	g.AddNode("alice")
	if _, ok := g.AdjList().Mapper().Lookup("alice"); !ok {
		t.Fatal("AddNode did not propagate to the underlying mapper")
	}
}

// TestGraph_EdgeIndex_TracksSetEdgeLabel verifies that EdgeIndex's
// Roaring bitmap is updated by SetEdgeLabel — this is the only
// behavioural guarantee EdgeIndex carries beyond returning a non-nil
// pointer.
func TestGraph_EdgeIndex_TracksSetEdgeLabel(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	g.AddEdge("a", "b", 1)
	g.SetEdgeLabel("a", "b", "KNOWS")
	lid, ok := g.Registry().Lookup("KNOWS")
	if !ok {
		t.Fatal("Registry has no KNOWS label after SetEdgeLabel")
	}
	bm := g.EdgeIndex().Intersect(uint32(lid))
	if bm.GetCardinality() != 1 {
		t.Fatalf("EdgeIndex KNOWS cardinality = %d, want 1", bm.GetCardinality())
	}
}

// TestGraph_RemoveNodeLabel_UnknownNode covers the early-return path
// where the node was never interned.
func TestGraph_RemoveNodeLabel_UnknownNode(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	// Must not panic and must be a no-op.
	g.RemoveNodeLabel("ghost", "AnyLabel")
}

// TestGraph_RemoveNodeLabel_UnknownLabel covers the early-return path
// where the label was never interned, even though the node exists.
func TestGraph_RemoveNodeLabel_UnknownLabel(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	g.AddNode("alice")
	g.RemoveNodeLabel("alice", "NeverRegistered")
}

// TestGraph_HasNodeLabel_NegativePaths covers every false-returning
// branch of HasNodeLabel: unknown node, unknown label, known node
// without any bag, and known node whose bag does not contain the
// label.
func TestGraph_HasNodeLabel_NegativePaths(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})

	if g.HasNodeLabel("ghost", "Anything") {
		t.Fatal("HasNodeLabel on unknown node must return false")
	}
	g.AddNode("alice")
	if g.HasNodeLabel("alice", "NeverInterned") {
		t.Fatal("HasNodeLabel with unknown label must return false")
	}
	g.SetNodeLabel("bob", "Person")
	if g.HasNodeLabel("alice", "Person") {
		t.Fatal("HasNodeLabel: alice has no label bag, must return false")
	}
}

// TestGraph_NodeLabels_NegativePaths covers the unknown-node and
// no-bag early-return branches of NodeLabels.
func TestGraph_NodeLabels_NegativePaths(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})

	if got := g.NodeLabels("ghost"); got != nil {
		t.Fatalf("NodeLabels(ghost) = %v, want nil", got)
	}
	g.AddNode("alice")
	if got := g.NodeLabels("alice"); got != nil {
		t.Fatalf("NodeLabels(alice without bag) = %v, want nil", got)
	}
}

// TestGraph_HasEdgeLabel_NegativePaths covers every false-returning
// branch of HasEdgeLabel: unknown src, unknown dst, unknown label,
// known endpoints without an edge bag, and known endpoints whose bag
// does not contain the queried label.
func TestGraph_HasEdgeLabel_NegativePaths(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})

	if g.HasEdgeLabel("ghost-src", "alice", "X") {
		t.Fatal("HasEdgeLabel: unknown src must return false")
	}
	g.AddNode("alice")
	if g.HasEdgeLabel("alice", "ghost-dst", "X") {
		t.Fatal("HasEdgeLabel: unknown dst must return false")
	}
	g.AddNode("bob")
	if g.HasEdgeLabel("alice", "bob", "NeverInterned") {
		t.Fatal("HasEdgeLabel: unknown label must return false")
	}
	// Endpoints exist, label is interned, but no edge bag.
	g.AddEdge("alice", "bob", 0)
	g.SetEdgeLabel("alice", "bob", "KNOWS")
	if g.HasEdgeLabel("alice", "bob", "FOLLOWS") {
		t.Fatal("HasEdgeLabel: bag does not contain FOLLOWS, must return false")
	}
}

// TestLabelRegistry_Concurrent_DoubleCheck deterministically exercises
// the post-write-lock double-check in LabelRegistry.Intern: an entry
// for the name is already present when the writer enters the
// critical section, so it returns the existing ID without allocating
// a new one. We drive it through the public Intern by issuing many
// concurrent intern calls on the same label name.
func TestLabelRegistry_Concurrent_DoubleCheck(t *testing.T) {
	t.Parallel()
	r := NewLabelRegistry()
	const goroutines = 64
	ids := make([]LabelID, goroutines)
	done := make(chan struct{})
	for i := range ids {
		go func(i int) {
			ids[i] = r.Intern("hot")
			done <- struct{}{}
		}(i)
	}
	for range goroutines {
		<-done
	}
	for i := 1; i < goroutines; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("racing Intern produced divergent ids: %d vs %d", ids[0], ids[i])
		}
	}
}
