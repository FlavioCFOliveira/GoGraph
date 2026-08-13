package lpg

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// These tests drive the four per-edge side stores through their public API
// with MORE parallel edges between one pair than [smallInstMax], which is the
// path sprint 339 introduced and the one the pre-existing store suites never
// reached: every fixture in the tree creates one or two parallel edges, so the
// promotion from the small tier to the map tier happened only inside a unit
// test of the container itself. Here it happens inside a live store, under the
// shard lock, with the MVCC side-version index attached.

const parallelEdges = smallInstMax + 4 // 12: four instances past promotion

func newParallelGraph(t *testing.T) *Graph[string, float64] {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode a: %v", err)
	}
	if err := g.AddNode("b"); err != nil {
		t.Fatalf("AddNode b: %v", err)
	}
	return g
}

// TestInstanceStores_ManyParallelEdgesByOrdinal writes, reads back and deletes
// more per-CREATE instances than the small tier holds.
func TestInstanceStores_ManyParallelEdgesByOrdinal(t *testing.T) {
	g := newParallelGraph(t)
	for i := 1; i <= parallelEdges; i++ {
		g.SetEdgeLabelAt("a", "b", int64(i), fmt.Sprintf("T%d", i))
		if err := g.SetEdgePropertyAt("a", "b", int64(i), "seq", Int64Value(int64(i))); err != nil {
			t.Fatalf("SetEdgePropertyAt %d: %v", i, err)
		}
	}

	for i := 1; i <= parallelEdges; i++ {
		labels := g.EdgeLabelsAt("a", "b", int64(i))
		want := fmt.Sprintf("T%d", i)
		if len(labels) != 1 || labels[0] != want {
			t.Fatalf("EdgeLabelsAt(%d) = %v, want [%s]", i, labels, want)
		}
		props := g.EdgePropertiesAt("a", "b", int64(i))
		v, ok := props["seq"]
		if !ok {
			t.Fatalf("EdgePropertiesAt(%d) has no seq: %v", i, props)
		}
		if got, _ := v.Int64(); got != int64(i) {
			t.Fatalf("EdgePropertiesAt(%d).seq = %d, want %d", i, got, i)
		}
	}

	// Remove one instance from the middle: every sibling must survive. A
	// swap-based removal in the small tier makes this the case most likely to
	// go wrong, and the promoted map tier must behave identically.
	g.RemoveEdgeInstance("a", "b", 5)
	if labels := g.EdgeLabelsAt("a", "b", 5); len(labels) != 0 {
		t.Fatalf("EdgeLabelsAt(5) after removal = %v, want none", labels)
	}
	for i := 1; i <= parallelEdges; i++ {
		if i == 5 {
			continue
		}
		labels := g.EdgeLabelsAt("a", "b", int64(i))
		want := fmt.Sprintf("T%d", i)
		if len(labels) != 1 || labels[0] != want {
			t.Fatalf("sibling %d lost after removing instance 5: %v, want [%s]", i, labels, want)
		}
	}
}

// TestInstanceStores_ManyParallelEdgesByHandle is the stable-handle analogue.
func TestInstanceStores_ManyParallelEdgesByHandle(t *testing.T) {
	g := newParallelGraph(t)
	handles := make([]uint64, 0, parallelEdges)
	for i := 1; i <= parallelEdges; i++ {
		h, err := g.AddEdgeH("a", "b", float64(i))
		if err != nil {
			t.Fatalf("AddEdgeH %d: %v", i, err)
		}
		handles = append(handles, h)
		g.SetEdgeLabelByHandle("a", "b", h, fmt.Sprintf("H%d", i))
		if err := g.SetEdgePropertyByHandle("a", "b", h, "seq", Int64Value(int64(i))); err != nil {
			t.Fatalf("SetEdgePropertyByHandle %d: %v", i, err)
		}
	}

	for i, h := range handles {
		labels := g.EdgeLabelsByHandle("a", "b", h)
		want := fmt.Sprintf("H%d", i+1)
		if len(labels) != 1 || labels[0] != want {
			t.Fatalf("EdgeLabelsByHandle(handle %d) = %v, want [%s]", h, labels, want)
		}
		props := g.EdgePropertiesByHandle("a", "b", h)
		v, ok := props["seq"]
		if !ok {
			t.Fatalf("EdgePropertiesByHandle(handle %d) has no seq: %v", h, props)
		}
		if got, _ := v.Int64(); got != int64(i+1) {
			t.Fatalf("EdgePropertiesByHandle(handle %d).seq = %d, want %d", h, got, i+1)
		}
	}

	g.RemoveEdgeInstanceByHandle("a", "b", handles[5])
	if labels := g.EdgeLabelsByHandle("a", "b", handles[5]); len(labels) != 0 {
		t.Fatalf("EdgeLabelsByHandle after removal = %v, want none", labels)
	}
	for i, h := range handles {
		if i == 5 {
			continue
		}
		labels := g.EdgeLabelsByHandle("a", "b", h)
		want := fmt.Sprintf("H%d", i+1)
		if len(labels) != 1 || labels[0] != want {
			t.Fatalf("sibling handle %d lost after removing another: %v, want [%s]", h, labels, want)
		}
	}
}

// TestInstanceStores_RemoveEdgeDropsEveryInstance covers the whole-pair drop,
// which is the operation the nested map was kept for: it must still clear every
// instance of the pair in both stores, past the promotion threshold.
func TestInstanceStores_RemoveEdgeDropsEveryInstance(t *testing.T) {
	g := newParallelGraph(t)
	for i := 1; i <= parallelEdges; i++ {
		g.SetEdgeLabelAt("a", "b", int64(i), fmt.Sprintf("T%d", i))
		if err := g.SetEdgePropertyAt("a", "b", int64(i), "seq", Int64Value(int64(i))); err != nil {
			t.Fatalf("SetEdgePropertyAt %d: %v", i, err)
		}
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.RemoveEdge("a", "b")
	if g.AdjList().HasEdge("a", "b") {
		t.Fatal("RemoveEdge left the edge in place")
	}

	for i := 1; i <= parallelEdges; i++ {
		if labels := g.EdgeLabelsAt("a", "b", int64(i)); len(labels) != 0 {
			t.Fatalf("instance %d survived RemoveEdge with labels %v", i, labels)
		}
		if props := g.EdgePropertiesAt("a", "b", int64(i)); len(props) != 0 {
			t.Fatalf("instance %d survived RemoveEdge with properties %v", i, props)
		}
	}
	// Re-creating the pair must start clean rather than resurrect the old
	// instance state — the invariant clearEdgePairState exists to hold.
	g.SetEdgeLabelAt("a", "b", 1, "Fresh")
	labels := g.EdgeLabelsAt("a", "b", 1)
	if len(labels) != 1 || labels[0] != "Fresh" {
		t.Fatalf("re-created instance 1 = %v, want [Fresh]", labels)
	}
}

// TestHandleStores_PopulatedInBothStorageModes pins the correction rmp #2402
// made to the godoc on [Graph.edgeHandleLabelShards], which claimed the
// by-handle stores were "Populated only in multigraph mode".
//
// They are not. The adjacency stamps a handle on the new slot in either mode,
// so the read path resolves by handle in either mode, and the memory audit of
// 2026-08-11 measured a Cypher relationship costing the same either way. This
// test asserts the store's behaviour directly rather than through a byte count,
// so the claim cannot drift back without a failure.
func TestHandleStores_PopulatedInBothStorageModes(t *testing.T) {
	for _, multigraph := range []bool{true, false} {
		t.Run(fmt.Sprintf("multigraph=%t", multigraph), func(t *testing.T) {
			g := New[string, float64](adjlist.Config{Directed: true, Multigraph: multigraph})
			for _, n := range []string{"a", "b"} {
				if err := g.AddNode(n); err != nil {
					t.Fatalf("AddNode %s: %v", n, err)
				}
			}
			h, err := g.AddEdgeH("a", "b", 1)
			if err != nil {
				t.Fatalf("AddEdgeH: %v", err)
			}
			if h == 0 {
				t.Fatal("AddEdgeH returned handle 0; both modes mint a handle")
			}
			g.SetEdgeLabelByHandle("a", "b", h, "KNOWS")
			if err := g.SetEdgePropertyByHandle("a", "b", h, "since", Int64Value(2026)); err != nil {
				t.Fatalf("SetEdgePropertyByHandle: %v", err)
			}

			if got := g.EdgeLabelsByHandle("a", "b", h); len(got) != 1 || got[0] != "KNOWS" {
				t.Fatalf("EdgeLabelsByHandle = %v, want [KNOWS] — the by-handle store must be readable in this mode", got)
			}
			props := g.EdgePropertiesByHandle("a", "b", h)
			if v, ok := props["since"]; !ok {
				t.Fatalf("EdgePropertiesByHandle = %v, want a since property", props)
			} else if n, _ := v.Int64(); n != 2026 {
				t.Fatalf("since = %d, want 2026", n)
			}
		})
	}
}
