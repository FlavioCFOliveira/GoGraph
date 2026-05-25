package search

// Task 706: Diameter of K_n (complete graph) equals 1.
//
// For an undirected complete graph K_n (n >= 2) every vertex is
// adjacent to every other vertex, so the diameter is exactly 1: no
// pair of vertices requires more than one hop. This file verifies
// Diameter(c) returns lo == 1 for n in {2, 16, 128}.
//
// shapegen.Complete(n, false) produces an undirected K_n. Diameter
// expects a symmetric (undirected) CSR, so directed=false is required.
//
// Acceptance criteria:
//   - lo == 1 for every tested n.
//   - When exact == true: hi == 1 as well.

import (
	"testing"

	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

func TestDiameter_Kn_Shapegen(t *testing.T) {
	t.Parallel()

	for _, n := range []int{2, 16, 128} {
		n := n
		t.Run("n="+itoa(n), func(t *testing.T) {
			t.Parallel()

			g, err := shapegen.Complete(n, false).Build(defaultCfg())
			if err != nil {
				t.Fatalf("Complete(%d).Build: %v", n, err)
			}

			c := csr.BuildFromAdjList(g.AdjList())
			lo, hi, exact := Diameter(c)

			const want = 1
			if lo != want {
				t.Fatalf("Diameter(K_%d): lo=%d, want %d", n, lo, want)
			}
			if exact && hi != want {
				t.Fatalf("Diameter(K_%d): exact==true but hi=%d, want %d", n, hi, want)
			}
		})
	}
}
