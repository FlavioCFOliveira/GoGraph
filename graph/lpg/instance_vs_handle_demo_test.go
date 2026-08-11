package lpg

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// instance_vs_handle_demo_test.go — the demonstrations rmp #2403 asks for.
//
// The spike asks whether the ordinal-keyed per-instance stores can be retired
// in favour of the handle-keyed ones. Two properties decide it, and the task
// requires each to be answered by a demonstration rather than by reading code:
// what the by-ordinal surface does when a SIBLING edge of the same pair is
// deleted, and whether both surfaces can be resolved as of a snapshot.

// TestDemo2403_OrdinalSurvivesSiblingDeleteButHandleIsSlotPrecise shows the
// asymmetry that motivated the handle store.
//
// The ordinal store is keyed by CREATE ORDER and no removal path mutates it, so
// after a sibling is removed the ordinals still name the same metadata they
// always did — they simply no longer correspond one-to-one with the surviving
// adjacency slots. The handle store is keyed by the immutable per-edge handle,
// so a surviving edge resolves to its own metadata regardless of what happened
// to its siblings.
func TestDemo2403_OrdinalSurvivesSiblingDeleteButHandleIsSlotPrecise(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}

	// Three parallel edges of distinct types, written through BOTH surfaces
	// exactly as cypher/exec.CreateRelationship does.
	handles := make([]uint64, 0, 3)
	types := []string{"T1", "T2", "T3"}
	for i, rt := range types {
		h, err := g.AddEdgeH("a", "b", float64(i))
		if err != nil {
			t.Fatalf("AddEdgeH: %v", err)
		}
		handles = append(handles, h)
		idx := g.IncEdgeCreateCount("a", "b")
		g.SetEdgeLabelAt("a", "b", idx, rt)
		g.SetEdgeLabelByHandle("a", "b", h, rt)
	}

	// Remove the MIDDLE instance through the handle-precise path, which is what
	// DELETE uses for a bound relationship.
	g.RemoveEdgeInstanceByHandle("a", "b", handles[1])

	// The handle surface is instance-precise: the removed one is gone, the
	// survivors still resolve to their OWN types.
	if got := g.EdgeLabelsByHandle("a", "b", handles[1]); len(got) != 0 {
		t.Fatalf("by-handle: removed instance still reports %v", got)
	}
	for _, i := range []int{0, 2} {
		got := g.EdgeLabelsByHandle("a", "b", handles[i])
		if len(got) != 1 || got[0] != types[i] {
			t.Fatalf("by-handle: survivor %d = %v, want [%s]", i, got, types[i])
		}
	}

	// The ordinal surface is untouched by the removal: all three ordinals still
	// answer. That is not a defect in itself — no removal path mutates this
	// store, by design (see cypher/undo_record.go) — but it means an ordinal no
	// longer identifies a LIVE edge, which is why a read path that re-derived
	// the ordinal positionally from slot order broke after a delete.
	for i, rt := range types {
		got := g.EdgeLabelsAt("a", "b", int64(i+1))
		if len(got) != 1 || got[0] != rt {
			t.Fatalf("by-ordinal: instance %d = %v, want [%s] (the store is delete-agnostic)", i+1, got, rt)
		}
	}
}

// TestDemo2403_BothSurfacesResolveAsOfASnapshot answers the second question:
// whether retiring the ordinal store would lose any MVCC capability. It would
// not — both surfaces carry their own side-version chains and both reconstruct
// a pre-image at a reader's start timestamp.
func TestDemo2403_BothSurfacesResolveAsOfASnapshot(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.armMVCC()
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode %s: %v", n, err)
		}
	}
	h, err := g.AddEdgeH("a", "b", 1)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	idx := g.IncEdgeCreateCount("a", "b")
	g.SetEdgeLabelAt("a", "b", idx, "BEFORE")
	g.SetEdgeLabelByHandle("a", "b", h, "BEFORE")

	snap := g.BeginRead()
	defer g.EndRead(snap)

	g.SetEdgeLabelAt("a", "b", idx, "AFTER")
	g.SetEdgeLabelByHandle("a", "b", h, "AFTER")

	// Both surfaces must show the reader its own instant, not the later write.
	ordinal := g.EdgeLabelsAtAsOf("a", "b", idx, snap)
	if len(ordinal) != 1 || ordinal[0] != "BEFORE" {
		t.Fatalf("by-ordinal as-of = %v, want [BEFORE]", ordinal)
	}
	byHandle := g.EdgeLabelsByHandleAsOf("a", "b", h, snap)
	if len(byHandle) != 1 || byHandle[0] != "BEFORE" {
		t.Fatalf("by-handle as-of = %v, want [BEFORE]", byHandle)
	}
}
