package search

// Task 695: Diameter of P_n (path graph) equals n-1.
//
// For an undirected path graph P_n on n vertices the diameter is
// exactly n-1: the two endpoints are at hop distance n-1 from each
// other and every other pair of vertices is closer. This file verifies
// Diameter(c) returns a lower bound lo == n-1 for n in {2, 16, 1024}.
//
// shapegen.Path(n, false) produces an undirected P_n. Diameter expects
// a symmetric (undirected) CSR, so directed=false is required.
//
// Acceptance criteria:
//   - lo == n-1 for every tested n.
//   - When exact == true: lo == hi == n-1.
//   - When exact == false: lo is still a valid lower bound (lo <= n-1).

import (
	"testing"

	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

func TestDiameter_Pn_Shapegen(t *testing.T) {
	t.Parallel()

	for _, n := range []int{2, 16, 1024} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()

			g, err := shapegen.Path(n, false).Build(defaultCfg())
			if err != nil {
				t.Fatalf("Path(%d).Build: %v", n, err)
			}

			c := csr.BuildFromAdjList(g.AdjList())
			lo, hi, exact := Diameter(c)

			want := n - 1
			if lo != want {
				t.Fatalf("Diameter(P_%d): lo=%d, want %d", n, lo, want)
			}
			if exact && hi != want {
				t.Fatalf("Diameter(P_%d): exact==true but hi=%d, want %d", n, hi, want)
			}
		})
	}
}
