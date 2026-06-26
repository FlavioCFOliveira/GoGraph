package csr_test

// build_live_test.go — regression gate for #1790 (sprint 250): a tombstone-only
// RemoveNode on an lpg.Graph used to leave ghost edges in a CSR built for
// search, because BuildFromAdjList reads the raw adjacency and has no tombstone
// awareness. BuildFromAdjListLive(adj, g.LiveNodeFilter()) must omit every arc
// incident to a tombstoned node so the snapshot reflects exactly the live
// topology.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// arcs reconstructs every (src,dst) arc in c from its offsets + edges arrays.
func arcs(c *csr.CSR[float64]) [][2]graph.NodeID {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	var out [][2]graph.NodeID
	for src := 0; src+1 < len(verts); src++ {
		for p := verts[src]; p < verts[src+1]; p++ {
			out = append(out, [2]graph.NodeID{graph.NodeID(src), edges[p]})
		}
	}
	return out
}

func TestBuildLive_NoGhostEdgesAfterTombstone_1790(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for _, k := range []string{"a", "b", "c"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode %s: %v", k, err)
		}
	}
	if err := g.AddEdge("a", "b", 5); err != nil {
		t.Fatalf("AddEdge a->b: %v", err)
	}
	if err := g.AddEdge("b", "c", 5); err != nil {
		t.Fatalf("AddEdge b->c: %v", err)
	}

	// Tombstone b WITHOUT stripping its incident edges (the direct-Go-API path).
	g.RemoveNode("b")
	bID, ok := g.AdjList().Mapper().Lookup("b")
	if !ok {
		t.Fatal("expected b to remain interned (NodeID stability)")
	}

	// Raw build is tombstone-agnostic: it still carries b's two ghost arcs.
	raw := csr.BuildFromAdjList(g.AdjList())
	if raw.Size() != 2 {
		t.Fatalf("raw build size = %d, want 2 (documents tombstone-agnostic contract)", raw.Size())
	}

	// Live build must omit every arc incident to the tombstoned node.
	live := csr.BuildFromAdjListLive(g.AdjList(), g.LiveNodeFilter())
	for _, a := range arcs(live) {
		if a[0] == bID || a[1] == bID {
			t.Errorf("live CSR contains ghost arc %v incident to tombstoned node b (id=%d)", a, bID)
		}
	}
	if live.Size() != 0 {
		t.Errorf("live CSR size = %d, want 0 (both edges were incident to b)", live.Size())
	}
}

func TestBuildLive_NilFilterMatchesRaw_1790(t *testing.T) {
	// On a tombstone-free graph, LiveNodeFilter returns nil and the live build
	// is byte-identical to the raw build (zero-overhead fast path).
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for _, k := range []string{"x", "y", "z"} {
		_ = g.AddNode(k)
	}
	_ = g.AddEdge("x", "y", 1)
	_ = g.AddEdge("y", "z", 2)
	_ = g.AddEdge("x", "z", 3)

	if f := g.LiveNodeFilter(); f != nil {
		t.Fatalf("LiveNodeFilter on a tombstone-free graph must be nil, got non-nil")
	}
	raw := csr.BuildFromAdjList(g.AdjList())
	live := csr.BuildFromAdjListLive(g.AdjList(), g.LiveNodeFilter())
	if raw.Size() != live.Size() || raw.Size() != 3 {
		t.Fatalf("size mismatch: raw=%d live=%d want 3", raw.Size(), live.Size())
	}
	rawArcs, liveArcs := arcs(raw), arcs(live)
	if len(rawArcs) != len(liveArcs) {
		t.Fatalf("arc count mismatch raw=%d live=%d", len(rawArcs), len(liveArcs))
	}
	for i := range rawArcs {
		if rawArcs[i] != liveArcs[i] {
			t.Errorf("arc %d differs: raw=%v live=%v", i, rawArcs[i], liveArcs[i])
		}
	}
}
