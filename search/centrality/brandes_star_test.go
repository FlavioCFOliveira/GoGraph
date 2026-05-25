package centrality

import (
	"math"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestBetweenness_StarCentreDominates verifies the betweenness
// invariants of an undirected star graph S_n for n in {5, 50, 500}
// leaves (n+1 total nodes):
//
//   - Centre bc = n*(n-1)   (every ordered leaf-pair has a unique
//     shortest path through the centre)
//   - Every leaf bc = 0
//   - Sum of all bc equals the centre's bc
//
// TestBetweenness_Star already covers n=4; this test exercises
// larger inputs to confirm the O(n²) growth is realised in practice.
func TestBetweenness_StarCentreDominates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		nLeaves int
	}{
		{5},
		{50},
		{500},
	}
	for _, tc := range cases {
		tc := tc
		t.Run("", func(t *testing.T) {
			t.Parallel()
			a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
			for i := 1; i <= tc.nLeaves; i++ {
				if err := a.AddEdge(0, i, struct{}{}); err != nil {
					t.Fatalf("AddEdge(0,%d): %v", i, err)
				}
			}
			c := csr.BuildFromAdjList(a)
			bc := Betweenness(c)

			hub, ok := a.Mapper().Lookup(0)
			if !ok {
				t.Fatalf("hub key 0 not found in mapper")
			}
			wantHub := float64(tc.nLeaves) * float64(tc.nLeaves-1)
			if math.Abs(bc[uint64(hub)]-wantHub) > 1e-9 {
				t.Fatalf("n=%d: hub bc = %f, want %f", tc.nLeaves, bc[uint64(hub)], wantHub)
			}

			// Every leaf must have zero betweenness.
			for i := 1; i <= tc.nLeaves; i++ {
				leaf, ok := a.Mapper().Lookup(i)
				if !ok {
					t.Fatalf("leaf key %d not found in mapper", i)
				}
				if bc[uint64(leaf)] != 0 {
					t.Fatalf("n=%d: leaf %d bc = %f, want 0", tc.nLeaves, i, bc[uint64(leaf)])
				}
			}

			// Sum of all bc must equal the centre's bc.
			var sum float64
			for _, v := range bc {
				sum += v
			}
			if math.Abs(sum-wantHub) > 1e-9 {
				t.Fatalf("n=%d: sum(bc) = %f, want %f", tc.nLeaves, sum, wantHub)
			}
		})
	}
}
