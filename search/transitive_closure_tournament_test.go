package search

// Task 879: Transitive closure on the transitive tournament T_n.
//
// The transitive tournament on n nodes has exactly the directed edges
// (i -> j) for all pairs with i < j. It follows that node i can reach
// node j iff i < j. TransitiveClosure must match this predicate for
// every ordered pair in the graph.

import (
	"fmt"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

// TestTransitiveClosure_Tournament verifies that the TC oracle on the
// transitive tournament T_n satisfies Reachable(i, j) == (i < j) for
// every ordered pair. Sizes tested: 3, 8, 32.
func TestTransitiveClosure_Tournament(t *testing.T) {
	t.Parallel()

	for _, n := range []int{3, 8, 32} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()

			// TransitiveTournament forces Directed=true internally;
			// we pass Directed:true for clarity and consistency.
			g, err := shapegen.TransitiveTournament(n).Build(adjlist.Config{Directed: true})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)
			tc := TransitiveClosure(c)

			mapper := a.Mapper()
			for i := 0; i < n; i++ {
				src, ok := mapper.Lookup(i)
				if !ok {
					t.Fatalf("key %d not found in mapper", i)
				}
				for j := 0; j < n; j++ {
					if i == j {
						continue
					}
					dst, ok := mapper.Lookup(j)
					if !ok {
						t.Fatalf("key %d not found in mapper", j)
					}
					got := tc.Reachable(src, dst)
					want := i < j // tournament: i reaches j iff i < j
					if got != want {
						t.Errorf("T%d Reachable(%d->%d) = %v, want %v",
							n, i, j, got, want)
					}
				}
			}
		})
	}
}
