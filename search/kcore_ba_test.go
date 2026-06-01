package search

// Task 872: K-core decomposition on a Barabási-Albert graph.
//
// A BA graph with m=4 new edges per step has a rich core structure:
// the maximum coreness is bounded below by m (every hub is in at
// least the m-core) and every vertex's coreness is bounded above by
// its degree. The empirical histogram of coreness values follows a
// heavy-tail distribution: higher cores are nested and thus
// progressively smaller.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestKCore_BarabasiAlbert runs k-core decomposition on a BA(n=1000,
// m=4, seed=42) graph and asserts three structural invariants:
//
//  1. The maximum coreness is >= 4 (every step attaches with m=4
//     edges, so the preferential-attachment hubs land in at least
//     the 4-core).
//  2. Every vertex's coreness is <= its degree (coreness can never
//     exceed degree by definition of the k-core).
//  3. The histogram of coreness values above the m-floor is
//     monotone non-increasing: count[k] >= count[k+1] for all
//     k >= 4.  This reflects the nested, heavy-tail structure of
//     BA cores.
func TestKCore_BarabasiAlbert(t *testing.T) {
	t.Parallel()

	const (
		n    = 1000
		m    = 4
		seed = 42
	)

	g, err := shapegen.BarabasiAlbert(n, m, seed).Build(adjlist.Config{Directed: false})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	a := g.AdjList()
	c := csr.BuildFromAdjList(a)

	coreness := KCore(c)

	// --- invariant 1: max coreness >= m ---
	maxCore := 0
	for _, k := range coreness {
		if k > maxCore {
			maxCore = k
		}
	}
	if maxCore < m {
		t.Fatalf("max coreness = %d, want >= %d", maxCore, m)
	}

	// --- invariant 2: coreness[v] <= degree(v) for every live vertex ---
	verts := c.VerticesSlice()
	maxID := int(c.MaxNodeID())
	for v := 0; v < maxID; v++ {
		deg := int(verts[v+1] - verts[v])
		if deg == 0 {
			// Ghost slot — coreness must be 0.
			if coreness[v] != 0 {
				t.Errorf("ghost v=%d: coreness=%d, want 0", v, coreness[v])
			}
			continue
		}
		if coreness[v] > deg {
			t.Errorf("v=%d: coreness=%d > degree=%d", v, coreness[v], deg)
		}
	}

	// --- invariant 3: histogram is monotone non-increasing for k >= m ---
	hist := make(map[int]int, maxCore+1)
	for _, k := range coreness {
		if k > 0 {
			hist[k]++
		}
	}
	for k := m; k < maxCore; k++ {
		if hist[k] < hist[k+1] {
			t.Errorf("histogram not monotone at k=%d: count[%d]=%d < count[%d]=%d",
				k, k, hist[k], k+1, hist[k+1])
		}
	}
}
