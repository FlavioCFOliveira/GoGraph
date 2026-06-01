package lpg

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestGraph_EdgeProperties(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("alice", "bob", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetEdgeProperty("alice", "bob", "since", Int64Value(2020)); err != nil {
		t.Fatalf("SetEdgeProperty since: %v", err)
	}
	if err := g.SetEdgeProperty("alice", "bob", "weight", Float64Value(0.9)); err != nil {
		t.Fatalf("SetEdgeProperty weight: %v", err)
	}

	if v, ok := g.GetEdgeProperty("alice", "bob", "since"); !ok {
		t.Fatalf("missing since")
	} else if i, _ := v.Int64(); i != 2020 {
		t.Fatalf("since = %d", i)
	}

	props := g.EdgeProperties("alice", "bob")
	if len(props) != 2 {
		t.Fatalf("edge props len = %d, want 2", len(props))
	}

	g.DelEdgeProperty("alice", "bob", "weight")
	if _, ok := g.GetEdgeProperty("alice", "bob", "weight"); ok {
		t.Fatalf("weight not deleted")
	}
}

func TestGraph_SetEdgeProperty_NoEdge(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetEdgeProperty("a", "b", "k", Int64Value(1)); err != nil {
		t.Fatalf("SetEdgeProperty on missing edge: %v", err)
	}
	if _, ok := g.GetEdgeProperty("a", "b", "k"); ok {
		t.Fatalf("SetEdgeProperty on missing edge must be a no-op")
	}
}

func TestGraph_GetEdgeProperty_UnknownNodes(t *testing.T) {
	t.Parallel()
	g := New[string, int64](adjlist.Config{Directed: true})
	if _, ok := g.GetEdgeProperty("nope", "nada", "k"); ok {
		t.Fatalf("Get on unknown nodes must return false")
	}
	if err := g.AddEdge("a", "b", 0); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if _, ok := g.GetEdgeProperty("a", "nada", "k"); ok {
		t.Fatalf("Get on unknown dst must return false")
	}
	if _, ok := g.GetEdgeProperty("nope", "b", "k"); ok {
		t.Fatalf("Get on unknown src must return false")
	}
}
