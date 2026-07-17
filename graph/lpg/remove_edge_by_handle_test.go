package lpg

// remove_edge_by_handle_test.go — unit coverage for Graph.RemoveEdgeByHandle,
// the instance-precise parallel-edge removal (adjacency slot + per-handle
// metadata) introduced for rmp #2018.
//
// Layer: short. goleak-clean (graphs are local).

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestGraph_RemoveEdgeByHandle_InstancePrecise removes the SECOND parallel
// instance by its handle and asserts the FIRST survives with its own per-handle
// type and its adjacency slot, and the removed instance's metadata is gone.
func TestGraph_RemoveEdgeByHandle_InstancePrecise(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	h1, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH h1: %v", err)
	}
	h2, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH h2: %v", err)
	}
	g.SetEdgeLabelByHandle("a", "b", h1, "T1")
	g.SetEdgeLabelByHandle("a", "b", h2, "T2")

	if !g.RemoveEdgeByHandle("a", "b", h2) {
		t.Fatal("RemoveEdgeByHandle(h2) returned false, want true")
	}

	// h2's per-handle metadata is gone; h1's is intact.
	if got := g.EdgeLabelsByHandle("a", "b", h2); len(got) != 0 {
		t.Fatalf("removed instance h2 still carries labels %v", got)
	}
	if got := g.EdgeLabelsByHandle("a", "b", h1); len(got) != 1 || got[0] != "T1" {
		t.Fatalf("sibling h1 labels = %v, want [T1]", got)
	}

	// Exactly one adjacency slot survives, carrying handle h1.
	srcID, _ := g.AdjList().Mapper().Lookup("a")
	nbs, _, handles := g.AdjList().LoadEntryH(srcID)
	if len(nbs) != 1 || handles[0] != h1 {
		t.Fatalf("surviving slot neighbours=%v handles=%v, want single slot handle %d", nbs, handles, h1)
	}
}

// TestGraph_RemoveEdgeByHandle_LastInstanceClearsPairState confirms edge
// tombstone hygiene: removing the last instance clears the per-pair coalesced
// labels so a later re-add between the same endpoints does not resurrect them.
func TestGraph_RemoveEdgeByHandle_LastInstanceClearsPairState(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	h, err := g.AddEdgeH("a", "b", 0)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	g.SetEdgeLabelByHandle("a", "b", h, "T")
	g.SetEdgeLabel("a", "b", "T") // per-pair coalesced label

	if !g.RemoveEdgeByHandle("a", "b", h) {
		t.Fatal("RemoveEdgeByHandle returned false, want true")
	}
	if g.AdjList().HasEdge("a", "b") {
		t.Fatal("edge should be fully removed after retiring the last instance")
	}

	// Re-add a fresh edge between the same endpoints: it must not inherit the
	// removed edge's per-pair label.
	if _, err := g.AddEdgeH("a", "b", 0); err != nil {
		t.Fatalf("re-add AddEdgeH: %v", err)
	}
	if got := g.EdgeLabels("a", "b"); len(got) != 0 {
		t.Fatalf("re-added edge resurrected stale per-pair labels %v", got)
	}
}

// TestGraph_RemoveEdgeByHandle_ZeroFallsBackToRemoveEdge confirms a 0 handle
// (no stable identity) falls back to the first-match RemoveEdge and reports
// whether an edge was present.
func TestGraph_RemoveEdgeByHandle_ZeroFallsBackToRemoveEdge(t *testing.T) {
	t.Parallel()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if _, err := g.AddEdgeH("a", "b", 0); err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	if !g.RemoveEdgeByHandle("a", "b", 0) {
		t.Fatal("RemoveEdgeByHandle(handle=0) on a present edge returned false, want true")
	}
	if g.AdjList().HasEdge("a", "b") {
		t.Fatal("edge should be removed by the handle=0 fallback")
	}
	if g.RemoveEdgeByHandle("a", "b", 0) {
		t.Fatal("RemoveEdgeByHandle(handle=0) on an absent edge returned true, want false")
	}
}
