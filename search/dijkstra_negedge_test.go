package search

import (
	"errors"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestDijkstra_NegativeEdge verifies that Dijkstra returns ErrNegativeWeight
// whenever the input graph contains at least one strictly negative edge weight,
// regardless of its position in the graph topology.
func TestDijkstra_NegativeEdge(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		edges []float64Edge
		src   int
	}{
		{
			// Negative weight on the source's outgoing edge.
			name:  "source-outgoing",
			edges: []float64Edge{{0, 1, -1.0}},
			src:   0,
		},
		{
			// Negative weight on an interior edge.
			name:  "interior-edge",
			edges: []float64Edge{{0, 1, 5.0}, {1, 2, -1.0}},
			src:   0,
		},
		{
			// Negative weight on the destination's incoming edge.
			name:  "destination-incoming",
			edges: []float64Edge{{0, 1, 5.0}, {1, 2, 3.0}, {0, 2, -2.0}},
			src:   0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := adjlist.New[int, float64](adjlist.Config{Directed: true})
			for _, e := range tc.edges {
				if err := a.AddEdge(e.from, e.to, e.w); err != nil {
					t.Fatalf("AddEdge: %v", err)
				}
			}
			c := csr.BuildFromAdjList(a)
			srcID, ok := a.Mapper().Lookup(tc.src)
			if !ok {
				t.Fatalf("node %d not found in mapper", tc.src)
			}

			_, err := Dijkstra(c, srcID)
			if !errors.Is(err, ErrNegativeWeight) {
				t.Errorf("Dijkstra returned %v, want errors.Is(..., ErrNegativeWeight)", err)
			}
		})
	}
}
