package centrality

import (
	"math"
	"testing"

	"pgregory.net/rapid"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestBetweenness_PropertyMatchesParallel checks that Betweenness and
// BetweennessParallel produce identical results within float64
// precision on random small graphs. Any divergence would indicate a
// race, a reduction bug, or a summation-order issue in the parallel
// variant.
//
// The random graph is built by adding m random edges among n nodes
// (both drawn by rapid), so the topology varies from sparse to dense
// and from disconnected to fully connected across the rapid corpus.
func TestBetweenness_PropertyMatchesParallel(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(5, 30).Draw(rt, "n")
		m := rapid.IntRange(n-1, 4*n).Draw(rt, "m")

		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		// Ensure all n nodes are registered so the mapper is dense
		// even if some receive no edges.
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				rt.Fatalf("AddNode(%d): %v", i, err)
			}
		}
		for range m {
			u := rapid.IntRange(0, n-1).Draw(rt, "u")
			v := rapid.IntRange(0, n-1).Draw(rt, "v")
			if err := a.AddEdge(u, v, struct{}{}); err != nil {
				rt.Fatalf("AddEdge(%d,%d): %v", u, v, err)
			}
		}

		c := csr.BuildFromAdjList(a)
		serial := Betweenness(c)
		parallel := BetweennessParallel(c, 4)

		if len(serial) != len(parallel) {
			rt.Fatalf("length mismatch: serial=%d parallel=%d", len(serial), len(parallel))
		}
		for i, sv := range serial {
			pv := parallel[i]
			if math.Abs(sv-pv) > 1e-9 {
				rt.Fatalf("node %d: serial=%.15f parallel=%.15f diff=%e",
					i, sv, pv, math.Abs(sv-pv))
			}
		}
	})
}

// TestBetweenness_PropertyNonNegative asserts bc(v) >= 0 for all
// vertices on any random undirected graph. Negative betweenness is
// always a bug in the Brandes accumulation.
func TestBetweenness_PropertyNonNegative(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(2, 40).Draw(rt, "n")
		m := rapid.IntRange(0, 4*n).Draw(rt, "m")

		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		for i := 0; i < n; i++ {
			if err := a.AddNode(i); err != nil {
				rt.Fatalf("AddNode(%d): %v", i, err)
			}
		}
		for range m {
			u := rapid.IntRange(0, n-1).Draw(rt, "u")
			v := rapid.IntRange(0, n-1).Draw(rt, "v")
			if err := a.AddEdge(u, v, struct{}{}); err != nil {
				rt.Fatalf("AddEdge(%d,%d): %v", u, v, err)
			}
		}

		c := csr.BuildFromAdjList(a)
		bc := Betweenness(c)
		for i, v := range bc {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				rt.Fatalf("bc[%d] = %f: must be finite and non-negative", i, v)
			}
		}
	})
}
