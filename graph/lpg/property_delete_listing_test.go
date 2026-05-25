package lpg_test

import (
	"fmt"
	"sort"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/internal/shapegen"
)

// walkKeys returns every user-facing int key present in g, in Walk order.
func walkKeys(g *lpg.Graph[int, int64]) []int {
	var keys []int
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, k int) bool {
		keys = append(keys, k)
		return true
	})
	return keys
}

// TestLPG_Property_DeleteListing covers DelNodeProperty, DelEdgeProperty, and
// NodeLabels listing across three topologies: K1 (isolated node), Pn (path),
// and Star (hub). No build tag — short layer.
func TestLPG_Property_DeleteListing(t *testing.T) {
	t.Parallel()

	t.Run("DelNodeProperty/K1", func(t *testing.T) {
		t.Parallel()
		testDelNodeProperty(t, shapegen.SingleNode())
	})

	t.Run("DelNodeProperty/Path", func(t *testing.T) {
		t.Parallel()
		testDelNodeProperty(t, shapegen.Path(8, true))
	})

	t.Run("DelNodeProperty/Star", func(t *testing.T) {
		t.Parallel()
		testDelNodeProperty(t, shapegen.Star(8, true))
	})

	t.Run("DelEdgeProperty/Path", func(t *testing.T) {
		t.Parallel()
		testDelEdgeProperty(t, shapegen.Path(8, true))
	})

	t.Run("DelEdgeProperty/Star", func(t *testing.T) {
		t.Parallel()
		testDelEdgeProperty(t, shapegen.Star(8, true))
	})

	t.Run("NodeLabels/K1", func(t *testing.T) {
		t.Parallel()
		testNodeLabels(t, shapegen.SingleNode())
	})

	t.Run("NodeLabels/Path", func(t *testing.T) {
		t.Parallel()
		testNodeLabels(t, shapegen.Path(8, true))
	})

	t.Run("NodeLabels/Star", func(t *testing.T) {
		t.Parallel()
		testNodeLabels(t, shapegen.Star(8, true))
	})
}

// testDelNodeProperty verifies DelNodeProperty semantics on the graph produced
// by shape:
//
//  1. Set 5 properties on every node (keys "k0".."k4", distinct per-node values).
//  2. Delete a subset ("k1", "k3").
//  3. Verify deleted keys return ok=false from GetNodeProperty.
//  4. Verify non-deleted keys still return correct values.
//  5. Delete an already-absent key — must not panic, must be a no-op.
func testDelNodeProperty(t *testing.T, shape shapegen.Shape[int, int64]) {
	t.Helper()

	g, err := shape.Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("shape.Build: %v", err)
	}

	keys := walkKeys(g)

	const propCount = 5
	deletedIdx := map[int]bool{1: true, 3: true}

	// Set propCount properties on every node.
	for _, nodeKey := range keys {
		for i := 0; i < propCount; i++ {
			propKey := fmt.Sprintf("k%d", i)
			val := lpg.StringValue(fmt.Sprintf("v%d_node%d", i, nodeKey))
			if setErr := g.SetNodeProperty(nodeKey, propKey, val); setErr != nil {
				t.Fatalf("SetNodeProperty(%d, %q): %v", nodeKey, propKey, setErr)
			}
		}
	}

	// Delete the subset.
	for _, nodeKey := range keys {
		for idx := range deletedIdx {
			g.DelNodeProperty(nodeKey, fmt.Sprintf("k%d", idx))
		}
	}

	// Verify presence/absence and value correctness.
	for _, nodeKey := range keys {
		for i := 0; i < propCount; i++ {
			propKey := fmt.Sprintf("k%d", i)
			val, ok := g.GetNodeProperty(nodeKey, propKey)
			if deletedIdx[i] {
				if ok {
					t.Errorf("node %d: deleted key %q still present", nodeKey, propKey)
				}
				continue
			}
			if !ok {
				t.Errorf("node %d: retained key %q unexpectedly absent", nodeKey, propKey)
				continue
			}
			want := fmt.Sprintf("v%d_node%d", i, nodeKey)
			got, _ := val.String()
			if got != want {
				t.Errorf("node %d: key %q value = %q, want %q", nodeKey, propKey, got, want)
			}
		}
	}

	// Delete already-absent keys — must not panic, surviving keys unaffected.
	for _, nodeKey := range keys {
		g.DelNodeProperty(nodeKey, "k1")          // already deleted
		g.DelNodeProperty(nodeKey, "no_such_key") // never existed
		if _, ok := g.GetNodeProperty(nodeKey, "k0"); !ok {
			t.Errorf("node %d: k0 missing after double-delete no-op", nodeKey)
		}
	}
}

// testDelEdgeProperty verifies DelEdgeProperty semantics on the graph produced
// by shape. For each directed edge:
//
//  1. Set a property "ep" on every edge.
//  2. Delete the property on edges at even indices.
//  3. Verify presence/absence and value correctness.
//  4. Delete already-absent property — must not panic.
func testDelEdgeProperty(t *testing.T, shape shapegen.Shape[int, int64]) {
	t.Helper()

	g, err := shape.Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("shape.Build: %v", err)
	}

	type edge struct{ src, dst int }
	var edges []edge
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, src int) bool {
		for dst := range g.AdjList().Neighbours(src) {
			edges = append(edges, edge{src, dst})
		}
		return true
	})

	if len(edges) == 0 {
		t.Skip("shape has no edges; skipping edge-property test")
	}

	const propKey = "ep"

	// Set property on every edge.
	for i, e := range edges {
		g.SetEdgeProperty(e.src, e.dst, propKey, lpg.Int64Value(int64(i)))
	}

	// Delete at even indices.
	deleted := make(map[int]bool, len(edges)/2+1)
	for i, e := range edges {
		if i%2 == 0 {
			g.DelEdgeProperty(e.src, e.dst, propKey)
			deleted[i] = true
		}
	}

	// Verify.
	for i, e := range edges {
		val, ok := g.GetEdgeProperty(e.src, e.dst, propKey)
		if deleted[i] {
			if ok {
				t.Errorf("edge (%d->%d): deleted property %q still present", e.src, e.dst, propKey)
			}
			continue
		}
		if !ok {
			t.Errorf("edge (%d->%d): retained property %q unexpectedly absent", e.src, e.dst, propKey)
			continue
		}
		v, _ := val.Int64()
		if v != int64(i) {
			t.Errorf("edge (%d->%d): property value = %d, want %d", e.src, e.dst, v, i)
		}
	}

	// Delete already-absent property — must not panic.
	for _, e := range edges {
		g.DelEdgeProperty(e.src, e.dst, propKey) // may already be gone
		g.DelEdgeProperty(e.src, e.dst, "no_such_key")
	}
}

// testNodeLabels verifies NodeLabels listing semantics on the graph produced by
// shape:
//
//  1. Set 3 labels on every node.
//  2. NodeLabels must return exactly those 3 labels in sorted order.
//  3. RemoveNodeLabel one label; NodeLabels must return 2 labels.
//  4. RemoveNodeLabel all remaining labels; len(NodeLabels)==0.
func testNodeLabels(t *testing.T, shape shapegen.Shape[int, int64]) {
	t.Helper()

	g, err := shape.Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("shape.Build: %v", err)
	}

	keys := walkKeys(g)

	labelSet := []string{"Alpha", "Beta", "Gamma"}
	wantAll := []string{"Alpha", "Beta", "Gamma"} // already sorted

	// Attach 3 labels to every node.
	for _, nodeKey := range keys {
		for _, lbl := range labelSet {
			if lblErr := g.SetNodeLabel(nodeKey, lbl); lblErr != nil {
				t.Fatalf("SetNodeLabel(%d, %q): %v", nodeKey, lbl, lblErr)
			}
		}
	}

	// Verify sorted listing == wantAll.
	for _, nodeKey := range keys {
		got := g.NodeLabels(nodeKey)
		sort.Strings(got)
		if len(got) != len(wantAll) {
			t.Errorf("node %d: NodeLabels len=%d, want %d: %v", nodeKey, len(got), len(wantAll), got)
			continue
		}
		for j := range wantAll {
			if got[j] != wantAll[j] {
				t.Errorf("node %d: NodeLabels[%d]=%q, want %q", nodeKey, j, got[j], wantAll[j])
			}
		}
	}

	// Remove Beta; expect {Alpha, Gamma}.
	wantAfterBeta := []string{"Alpha", "Gamma"}
	for _, nodeKey := range keys {
		g.RemoveNodeLabel(nodeKey, "Beta")
		got := g.NodeLabels(nodeKey)
		sort.Strings(got)
		if len(got) != len(wantAfterBeta) {
			t.Errorf("node %d: after Remove(Beta) NodeLabels len=%d, want %d: %v",
				nodeKey, len(got), len(wantAfterBeta), got)
			continue
		}
		for j := range wantAfterBeta {
			if got[j] != wantAfterBeta[j] {
				t.Errorf("node %d: after Remove(Beta) NodeLabels[%d]=%q, want %q",
					nodeKey, j, got[j], wantAfterBeta[j])
			}
		}
	}

	// Remove all remaining labels; expect length==0.
	for _, nodeKey := range keys {
		g.RemoveNodeLabel(nodeKey, "Alpha")
		g.RemoveNodeLabel(nodeKey, "Gamma")
		got := g.NodeLabels(nodeKey)
		if len(got) != 0 {
			t.Errorf("node %d: after removing all labels NodeLabels len=%d, want 0: %v",
				nodeKey, len(got), got)
		}
	}
}
