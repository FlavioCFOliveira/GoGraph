package graphml_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/io/graphml"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// toStringAdj converts an *adjlist.AdjList[int, int64] to
// *adjlist.AdjList[string, int64] so it can be handed to the GraphML
// writer, which is specialised on string node keys. Node ids are
// rendered with fmt.Sprintf("%d", key).
func toStringAdj(a *adjlist.AdjList[int, int64]) *adjlist.AdjList[string, int64] {
	s := adjlist.New[string, int64](adjlist.Config{
		Directed:   a.Directed(),
		Multigraph: a.Multigraph(),
	})
	maxID := uint64(a.MaxNodeID())
	// Pre-resolve all names in one Walk so the edge loop pays no
	// per-node Mapper.Resolve cost.
	names := make([]string, maxID)
	live := make([]bool, maxID)
	a.Mapper().Walk(func(id graph.NodeID, key int) bool {
		names[uint64(id)] = fmt.Sprintf("%d", key)
		live[uint64(id)] = true
		return true
	})
	// Emit edges; AddEdge interns source/destination implicitly so
	// isolated nodes (no edges) are handled by the second pass below.
	for id := uint64(0); id < maxID; id++ {
		if !live[id] {
			continue
		}
		nb, ws := a.LoadEntry(graph.NodeID(id))
		for i, n := range nb {
			if uint64(n) >= maxID || !live[uint64(n)] {
				continue
			}
			// Ignore errors: AddEdge on a fresh AdjList[string,int64]
			// is infallible for simple non-multigraphs in practice.
			_ = s.AddEdge(names[id], names[uint64(n)], ws[i])
		}
	}
	// Intern isolated nodes (nodes with no out-edges that were not
	// added implicitly by AddEdge above).
	for id := uint64(0); id < maxID; id++ {
		if live[id] {
			_ = s.AddNode(names[id])
		}
	}
	return s
}

// TestGraphMLRoundtrip_ClassicShapes writes a classic graph as GraphML
// then reads it back and verifies Order, Size and directed edge
// membership are preserved.
func TestGraphMLRoundtrip_ClassicShapes(t *testing.T) {
	t.Parallel()

	type shapeCase struct {
		shape    shapegen.Shape[int, int64]
		directed bool
	}

	cases := []shapeCase{
		// P₅ — directed path on 5 nodes.
		{shape: shapegen.Path(5, true), directed: true},
		// C₅ — directed cycle on 5 nodes.
		{shape: shapegen.Cycle(5, true), directed: true},
		// S₅ — directed star: centre node 0 + 4 leaves (n=5 total).
		{shape: shapegen.Star(5, true), directed: true},
		// K₄ — directed complete graph on 4 nodes.
		{shape: shapegen.Complete(4, true), directed: true},
		// K₃,₃ — undirected complete bipartite graph.
		{shape: shapegen.CompleteBipartite(3, 3), directed: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.shape.Name(), func(t *testing.T) {
			t.Parallel()

			cfg := adjlist.Config{Directed: tc.directed}
			g, err := tc.shape.Build(cfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			orig := g.AdjList()
			sa := toStringAdj(orig)

			var buf bytes.Buffer
			if err := graphml.Write(&buf, sa); err != nil {
				t.Fatalf("Write: %v", err)
			}

			got, _, err := graphml.ReadInto(&buf)
			if err != nil {
				t.Fatalf("ReadInto: %v", err)
			}

			if got.Order() != sa.Order() {
				t.Errorf("Order: got %d, want %d", got.Order(), sa.Order())
			}
			if got.Size() != sa.Size() {
				t.Errorf("Size: got %d, want %d", got.Size(), sa.Size())
			}

			// For directed graphs verify every original edge is present
			// in the round-trip.
			if tc.directed {
				maxID := uint64(sa.MaxNodeID())
				names := make([]string, maxID)
				live := make([]bool, maxID)
				sa.Mapper().Walk(func(id graph.NodeID, key string) bool {
					names[uint64(id)] = key
					live[uint64(id)] = true
					return true
				})
				for id := uint64(0); id < maxID; id++ {
					if !live[id] {
						continue
					}
					nb, _ := sa.LoadEntry(graph.NodeID(id))
					src := names[id]
					for _, n := range nb {
						if uint64(n) >= maxID || !live[uint64(n)] {
							continue
						}
						dst := names[uint64(n)]
						if !got.HasEdge(src, dst) {
							t.Errorf("missing edge (%s, %s) after round-trip", src, dst)
						}
					}
				}
			}
		})
	}
}
