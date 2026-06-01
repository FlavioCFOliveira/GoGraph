package lpg_test

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestGraph_EdgeLabels_Coverage covers EdgeLabels including the nil paths.
func TestGraph_EdgeLabels_Coverage(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})

	// Unknown src → nil.
	if got := g.EdgeLabels("x", "y"); got != nil {
		t.Errorf("expected nil for unknown src, got %v", got)
	}

	if err := g.AddNode("alice"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	// Unknown dst → nil.
	if got := g.EdgeLabels("alice", "bob"); got != nil {
		t.Errorf("expected nil for unknown dst, got %v", got)
	}

	if err := g.AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	// Edge exists but has no labels → empty (non-nil) or nil.
	// Either way the function should not panic.
	_ = g.EdgeLabels("alice", "bob")

	g.SetEdgeLabel("alice", "bob", "KNOWS")
	g.SetEdgeLabel("alice", "bob", "FOLLOWS")
	got := g.EdgeLabels("alice", "bob")
	if len(got) != 2 {
		t.Errorf("expected 2 labels, got %v", got)
	}
}

// TestGraph_IndexManager covers IndexManager and SetIndexManager.
func TestGraph_IndexManager_Coverage(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{})

	// Initially nil.
	if g.IndexManager() != nil {
		t.Error("expected nil IndexManager by default")
	}

	mgr := index.NewManager()
	g.SetIndexManager(mgr)
	if g.IndexManager() != mgr {
		t.Error("expected IndexManager to be set")
	}

	// Restore nil.
	g.SetIndexManager(nil)
	if g.IndexManager() != nil {
		t.Error("expected nil after clearing IndexManager")
	}
}
