package search

import (
	"fmt"
	"testing"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

// TestTarjanSCC_Tournament verifies that a transitive tournament on n
// vertices produces n singleton SCCs emitted in reverse topological order.
//
// In a transitive tournament (edges i->j for i<j), every vertex is its own
// SCC. Tarjan emits SCCs in reverse topological order, so a vertex with no
// outgoing edges (the "sink") appears first in the output. We verify this
// structurally: for every edge u->v in the original graph, v's position in
// the emission slice is strictly less than u's position (v was emitted first).
func TestTarjanSCC_Tournament(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 8, 32} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()

			g, err := shapegen.TransitiveTournament(n).Build(adjlist.Config{Directed: true})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)
			sccs := TarjanSCC(c)

			// Assertion 1: exactly n SCCs.
			if len(sccs) != n {
				t.Fatalf("n=%d: SCC count = %d, want %d", n, len(sccs), n)
			}

			// Assertion 2: every SCC is a singleton.
			for i, comp := range sccs {
				if len(comp) != 1 {
					t.Fatalf("n=%d: SCC[%d] size = %d, want 1", n, i, len(comp))
				}
			}

			// Assertion 3: emission order is a valid reverse topological order.
			// Build a position map: NodeID -> emission index.
			emitPos := make(map[graph.NodeID]int, n)
			for i, comp := range sccs {
				emitPos[comp[0]] = i
			}

			// For every edge u->v in the CSR, v must have been emitted before u
			// (i.e., emitPos[v] < emitPos[u]).
			verts := c.VerticesSlice()
			edges := c.EdgesSlice()
			maxID := uint64(c.MaxNodeID())
			for from := uint64(0); from < maxID; from++ {
				for k := verts[from]; k < verts[from+1]; k++ {
					to := edges[k]
					if emitPos[graph.NodeID(to)] >= emitPos[graph.NodeID(from)] {
						t.Fatalf("n=%d: edge %d->%d violates reverse topo order: pos[to]=%d >= pos[from]=%d",
							n, from, to, emitPos[graph.NodeID(to)], emitPos[graph.NodeID(from)])
					}
				}
			}
		})
	}
}
