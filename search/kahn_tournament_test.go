package search

// Task 852: Kahn topological sort on transitive tournaments.
//
// A transitive tournament on n vertices has edges i->j for every pair
// (i, j) with i < j. The only valid topological order is the identity
// permutation 0, 1, ..., n-1: vertex 0 is the unique source (indegree
// 0) and vertex n-1 is the unique sink (no out-edges), with no other
// valid linearisation. The Kahn algorithm must produce exactly this
// sequence.
//
// Acceptance criteria for each n in {3, 8, 32}:
//  1. TopologicalSort returns no error.
//  2. len(order) == n.
//  3. The sequence of user keys extracted via Resolve equals
//     [0, 1, ..., n-1] — the unique valid linearisation.

import (
	"fmt"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

func TestTopologicalSort_Tournament(t *testing.T) {
	t.Parallel()

	for _, n := range []int{3, 8, 32} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()

			g, err := shapegen.TransitiveTournament(n).Build(adjlist.Config{Directed: true})
			if err != nil {
				t.Fatalf("TransitiveTournament(%d).Build: %v", n, err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)

			order, err := TopologicalSort(c)
			if err != nil {
				t.Fatalf("TopologicalSort: %v", err)
			}

			if len(order) != n {
				t.Fatalf("len(order) = %d, want %d", len(order), n)
			}

			// The only valid linearisation of the transitive tournament
			// is [0, 1, ..., n-1].  Resolve each NodeID back to its
			// user key and verify the key sequence is the identity.
			mapper := a.Mapper()
			for i, nodeID := range order {
				key, ok := mapper.Resolve(nodeID)
				if !ok {
					t.Fatalf("order[%d] = NodeID %d: Resolve failed", i, nodeID)
				}
				if key != i {
					t.Errorf("order[%d]: key = %d, want %d", i, key, i)
				}
			}
		})
	}
}
