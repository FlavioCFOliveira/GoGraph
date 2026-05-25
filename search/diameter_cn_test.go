package search

// Task 701: Diameter of C_n (cycle graph) equals floor(n/2).
//
// For an undirected cycle graph C_n the diameter is floor(n/2): the
// antipodal vertex is at distance floor(n/2) hops and no two vertices
// are farther apart. This file verifies Diameter(c) returns a lower
// bound lo == n/2 for n in {3, 4, 16, 17}.
//
// shapegen.Cycle(n, false) produces an undirected C_n. Diameter
// expects a symmetric (undirected) CSR, so directed=false is required.
//
// The existing TestDiameter_Cycle in diameter_test.go covers the hand-
// built C_5; this file covers the shapegen-based fixture with different
// sizes and distinct function names to avoid any name collision.
//
// Acceptance criteria:
//   - lo == n/2 (integer division) for every tested n.
//   - When exact == true: lo == hi == n/2.

import (
	"testing"

	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

func TestDiameter_Cn_Shapegen(t *testing.T) {
	t.Parallel()

	for _, n := range []int{3, 4, 16, 17} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()

			g, err := shapegen.Cycle(n, false).Build(defaultCfg())
			if err != nil {
				t.Fatalf("Cycle(%d).Build: %v", n, err)
			}

			c := csr.BuildFromAdjList(g.AdjList())
			lo, hi, exact := Diameter(c)

			want := n / 2
			if lo != want {
				t.Fatalf("Diameter(C_%d): lo=%d, want %d", n, lo, want)
			}
			if exact && hi != want {
				t.Fatalf("Diameter(C_%d): exact==true but hi=%d, want %d", n, hi, want)
			}
		})
	}
}
