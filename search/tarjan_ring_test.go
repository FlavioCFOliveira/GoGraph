package search

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestTarjanSCC_SingleRing verifies that a single directed ring Cn is
// recognised as exactly one SCC of size n. Sizes tested: 3, 16, 1024.
func TestTarjanSCC_SingleRing(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 16, 1024} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()

			g, err := shapegen.Cycle(n, true).Build(adjlist.Config{Directed: true})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			c := csr.BuildFromAdjList(g.AdjList())
			sccs := TarjanSCC(c)

			// Assertion 1: exactly one SCC.
			if len(sccs) != 1 {
				t.Fatalf("n=%d: SCC count = %d, want 1", n, len(sccs))
			}

			// Assertion 2: the single SCC contains all n vertices.
			if len(sccs[0]) != n {
				t.Fatalf("n=%d: SCC size = %d, want %d", n, len(sccs[0]), n)
			}
		})
	}
}
