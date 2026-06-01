package search

// Task 876: Triangle count on K_n equals C(n, 3).
//
// The complete undirected graph K_n contains exactly C(n,3) = n*(n-1)*(n-2)/6
// triangles. CountTriangles must match this closed-form for n in {3, 8, 32}.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestCountTriangles_Kn verifies that CountTriangles on the complete
// undirected graph K_n reports exactly C(n, 3) total triangles.
// Per-node counts are also checked: each vertex of K_n participates
// in exactly C(n-1, 2) = (n-1)*(n-2)/2 triangles.
func TestCountTriangles_Kn(t *testing.T) {
	t.Parallel()

	for _, n := range []int{3, 8, 32} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()

			g, err := shapegen.Complete(n, false).Build(adjlist.Config{Directed: false})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)

			total, perNode := CountTriangles(c)

			// Total triangles in K_n = n choose 3 = n(n-1)(n-2)/6.
			wantTotal := int64(n) * int64(n-1) * int64(n-2) / 6
			if total != wantTotal {
				t.Fatalf("K%d total triangles = %d, want %d", n, total, wantTotal)
			}

			// Each vertex is in C(n-1, 2) = (n-1)*(n-2)/2 triangles.
			wantPerNode := int64(n-1) * int64(n-2) / 2
			m := a.Mapper()
			for i := 0; i < n; i++ {
				id, ok := m.Lookup(i)
				if !ok {
					t.Fatalf("K%d: key %d not found in mapper", n, i)
				}
				if perNode[id] != wantPerNode {
					t.Errorf("K%d perNode[%d] = %d, want %d", n, i, perNode[id], wantPerNode)
				}
			}
		})
	}
}
