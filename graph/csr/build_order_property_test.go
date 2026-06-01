package csr_test

import (
	"math/rand/v2"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// directedEdge is a directed weighted edge used in build-order tests.
type directedEdge struct {
	src, dst int
	w        int64
}

// TestCSR_BuildOrder_Independent_1000Iterations uses rapid.Check to
// verify that two CSRs built from the same edge multiset — inserted in
// different orders — contain identical sorted edge multisets. The
// property ensures BuildFromAdjList's output is independent of the
// adjlist insertion order.
func TestCSR_BuildOrder_Independent_1000Iterations(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(2, 20).Draw(rt, "n")
		m := rapid.IntRange(0, n*(n-1)).Draw(rt, "m")
		seed := rapid.Uint64().Draw(rt, "seed")

		// Generate m random directed edges (repetition allowed for multigraph).
		rng := rand.New(rand.NewPCG(seed, seed^0xDEAD)) //nolint:gosec // deterministic test RNG
		edges := make([]directedEdge, m)
		for i := range edges {
			edges[i] = directedEdge{
				src: rng.IntN(n),
				dst: rng.IntN(n),
				w:   int64(i),
			}
		}

		// Build adjlist a1 with edges in original order.
		a1 := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
		for _, e := range edges {
			_ = a1.AddEdge(e.src, e.dst, e.w) //nolint:errcheck // multigraph AddEdge is infallible
		}

		// Build adjlist a2 with edges in shuffled order.
		shuffled := make([]directedEdge, len(edges))
		copy(shuffled, edges)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		a2 := adjlist.New[int, int64](adjlist.Config{Directed: true, Multigraph: true})
		for _, e := range shuffled {
			_ = a2.AddEdge(e.src, e.dst, e.w) //nolint:errcheck // multigraph AddEdge is infallible
		}

		c1 := csr.BuildFromAdjList(a1)
		c2 := csr.BuildFromAdjList(a2)

		if c1.Order() != c2.Order() {
			rt.Errorf("Order: c1=%d c2=%d", c1.Order(), c2.Order())
			return
		}
		if c1.Size() != c2.Size() {
			rt.Errorf("Size: c1=%d c2=%d", c1.Size(), c2.Size())
			return
		}

		// Collect and sort edge pairs from both CSRs; order within each
		// source's adjacency list may differ depending on insertion order,
		// so compare sorted multisets.
		e1 := collectCSREdges(c1)
		e2 := collectCSREdges(c2)

		sort.Slice(e1, func(i, j int) bool {
			if e1[i].u != e1[j].u {
				return e1[i].u < e1[j].u
			}
			return e1[i].v < e1[j].v
		})
		sort.Slice(e2, func(i, j int) bool {
			if e2[i].u != e2[j].u {
				return e2[i].u < e2[j].u
			}
			return e2[i].v < e2[j].v
		})

		if len(e1) != len(e2) {
			rt.Errorf("edge count mismatch: c1=%d c2=%d", len(e1), len(e2))
			return
		}
		for i := range e1 {
			if e1[i] != e2[i] {
				rt.Errorf("edge[%d]: c1=(%d,%d) c2=(%d,%d)",
					i, e1[i].u, e1[i].v, e2[i].u, e2[i].v)
				break
			}
		}
	})
}
