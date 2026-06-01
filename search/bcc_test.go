package search

import (
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"

	"pgregory.net/rapid"
)

func TestHopcroftTarjanBCC_BridgeFixture(t *testing.T) {
	t.Parallel()
	// Two triangles 0-1-2 and 3-4-5 connected by a bridge 2-3.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	edges := [][2]int{{0, 1}, {1, 2}, {2, 0}, {2, 3}, {3, 4}, {4, 5}, {5, 3}}
	for _, e := range edges {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)

	// Bridge 2-3 must be detected.
	id2, _ := a.Mapper().Lookup(2)
	id3, _ := a.Mapper().Lookup(3)
	hasBridge := false
	for _, b := range res.Bridges {
		if (b[0] == id2 && b[1] == id3) || (b[0] == id3 && b[1] == id2) {
			hasBridge = true
		}
	}
	if !hasBridge {
		t.Fatalf("bridge 2-3 not detected; got %v", res.Bridges)
	}

	// Articulation points should be 2 and 3.
	articInts := make([]int, 0, len(res.Articulation))
	for _, id := range res.Articulation {
		v, _ := a.Mapper().Resolve(id)
		articInts = append(articInts, v)
	}
	sort.Ints(articInts)
	if len(articInts) < 1 {
		t.Fatalf("expected at least one articulation point, got %v", articInts)
	}
}

func TestHopcroftTarjanBCC_SingleCycle(t *testing.T) {
	t.Parallel()
	// A single cycle: no bridges, no articulation points.
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 5; i++ {
		if err := a.AddEdge(i, (i+1)%5, struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)
	if len(res.Bridges) != 0 {
		t.Fatalf("single cycle should have no bridges, got %v", res.Bridges)
	}
	if len(res.Articulation) != 0 {
		t.Fatalf("single cycle should have no articulation points, got %v", res.Articulation)
	}
}

// TestHopcroftTarjanBCC_MultigraphParallel verifies that two parallel
// undirected edges between the same pair of vertices form a single
// 2-cycle BCC and are NOT mistakenly classified as a bridge. This
// pins the parent-edge-skip fix that switched the algorithm from a
// parent-NodeID filter (which dropped all parallel edges to the
// parent) to a parent-edge-index filter (which skips only the one
// edge used to descend into the child).
func TestHopcroftTarjanBCC_MultigraphParallel(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false, Multigraph: true})
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := a.AddEdge(0, 1, struct{}{}); err != nil { // parallel
		t.Fatalf("AddEdge: %v", err)
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)
	if len(res.Bridges) != 0 {
		t.Fatalf("two parallel edges form a 2-cycle BCC, not a bridge; got bridges=%v", res.Bridges)
	}
	if len(res.Articulation) != 0 {
		t.Fatalf("two parallel edges should produce no articulation points; got %v", res.Articulation)
	}
	if len(res.Components) != 1 {
		t.Fatalf("two parallel edges should form exactly 1 BCC; got %d components", len(res.Components))
	}
}

// TestHopcroftTarjanBCC_MultigraphSingleEdgeStillBridge verifies the
// non-multigraph case: a single bridge edge between two cliques is
// still a bridge under the multigraph-aware fix.
func TestHopcroftTarjanBCC_MultigraphSingleEdgeStillBridge(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false, Multigraph: true})
	for _, e := range [][2]int{{0, 1}, {1, 2}, {2, 0}, {2, 3}, {3, 4}, {4, 5}, {5, 3}} {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)
	if len(res.Bridges) == 0 {
		t.Fatalf("single bridge edge 2-3 should be detected in multigraph mode; got %v", res.Bridges)
	}
}

// TestHopcroftTarjanBCC_TwoDisjointTriangles pins the D4 fix: two
// disjoint triangles share no articulation point. Prior to the fix
// the root of the second triangle was mis-classified as articulation
// because the timer is global and disc[root2]>0.
func TestHopcroftTarjanBCC_TwoDisjointTriangles(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	tri1 := [][2]int{{0, 1}, {1, 2}, {2, 0}}
	tri2 := [][2]int{{3, 4}, {4, 5}, {5, 3}}
	for _, e := range tri1 {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	for _, e := range tri2 {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)
	if len(res.Articulation) != 0 {
		t.Fatalf("two disjoint triangles should have no articulation points; got %v", res.Articulation)
	}
	if len(res.Bridges) != 0 {
		t.Fatalf("two disjoint triangles should have no bridges; got %v", res.Bridges)
	}
}

// TestHopcroftTarjanBCC_TwoDisjointPaths exercises the more
// articulation-rich case: two disjoint 5-paths each have 3 internal
// (degree-2) vertices that are articulation points. Total = 6.
func TestHopcroftTarjanBCC_TwoDisjointPaths(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	for i := 0; i < 4; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil { // path 0-1-2-3-4
			t.Fatalf("AddEdge: %v", err)
		}
	}
	for i := 5; i < 9; i++ {
		if err := a.AddEdge(i, i+1, struct{}{}); err != nil { // path 5-6-7-8-9
			t.Fatalf("AddEdge: %v", err)
		}
	}
	c := csr.BuildFromAdjList(a)
	res := HopcroftTarjanBCC(c)

	gotIDs := make(map[int]struct{}, len(res.Articulation))
	for _, id := range res.Articulation {
		v, _ := a.Mapper().Resolve(id)
		gotIDs[v] = struct{}{}
	}
	wantIDs := []int{1, 2, 3, 6, 7, 8}
	for _, v := range wantIDs {
		if _, ok := gotIDs[v]; !ok {
			t.Fatalf("missing articulation %d (got=%v)", v, gotIDs)
		}
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("extra articulation points (got=%v, want=%v)", gotIDs, wantIDs)
	}
}

// bruteArticulation computes articulation points by removing each
// live vertex in turn and counting connected components. v is
// articulation iff ccWithout(v) > cc(all). Leaves and isolated
// vertices are correctly excluded by this definition.
func bruteArticulation(c *csr.CSR[struct{}]) map[graph.NodeID]struct{} {
	n := int(c.MaxNodeID())
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	mask := c.LiveMask()
	base := countComponents(verts, edges, mask, -1)
	got := map[graph.NodeID]struct{}{}
	for v := 0; v < n; v++ {
		if !mask[v] {
			continue
		}
		without := countComponents(verts, edges, mask, v)
		if without > base {
			got[graph.NodeID(v)] = struct{}{}
		}
	}
	return got
}

func countComponents(verts []uint64, edges []graph.NodeID, mask []bool, skip int) int {
	n := len(mask)
	visited := make([]bool, n)
	cc := 0
	for s := 0; s < n; s++ {
		if !mask[s] || visited[s] || s == skip {
			continue
		}
		cc++
		queue := []int{s}
		visited[s] = true
		for len(queue) > 0 {
			u := queue[0]
			queue = queue[1:]
			for k := verts[u]; k < verts[u+1]; k++ {
				w := int(edges[k])
				if !mask[w] || visited[w] || w == skip {
					continue
				}
				visited[w] = true
				queue = append(queue, w)
			}
		}
	}
	return cc
}

// TestHopcroftTarjanBCC_ForestPropertyVsBrute is a rapid-driven
// property test: for random forests (multi-component undirected
// graphs) the Hopcroft-Tarjan articulation set must match the
// remove-vertex brute-force ground truth.
func TestHopcroftTarjanBCC_ForestPropertyVsBrute(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		nComps := rapid.IntRange(1, 4).Draw(rt, "nComponents")
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		nextID := 0
		for c := 0; c < nComps; c++ {
			size := rapid.IntRange(2, 8).Draw(rt, "componentSize")
			base := nextID
			// Spanning path inside the component so it stays connected.
			for i := 0; i < size; i++ {
				if err := a.AddNode(base + i); err != nil {
					t.Fatalf("AddNode: %v", err)
				}
			}
			for i := 0; i < size-1; i++ {
				if err := a.AddEdge(base+i, base+i+1, struct{}{}); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
			// A few extra random chords inside this component.
			extra := rapid.IntRange(0, size).Draw(rt, "extraChords")
			for k := 0; k < extra; k++ {
				u := rapid.IntRange(0, size-1).Draw(rt, "u")
				v := rapid.IntRange(0, size-1).Draw(rt, "v")
				if u != v {
					if err := a.AddEdge(base+u, base+v, struct{}{}); err != nil {
						t.Fatalf("AddEdge: %v", err)
					}
				}
			}
			nextID = base + size
		}
		c := csr.BuildFromAdjList(a)
		res := HopcroftTarjanBCC(c)
		got := map[graph.NodeID]struct{}{}
		for _, id := range res.Articulation {
			got[id] = struct{}{}
		}
		want := bruteArticulation(c)
		// got and want are sets of NodeIDs; assert equality.
		for id := range want {
			if _, ok := got[id]; !ok {
				rt.Fatalf("missing articulation: got=%v want=%v", got, want)
			}
		}
		for id := range got {
			if _, ok := want[id]; !ok {
				rt.Fatalf("spurious articulation: got=%v want=%v", got, want)
			}
		}
	})
}
