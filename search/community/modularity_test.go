package community

import (
	"testing"

	"pgregory.net/rapid"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestLeiden_ModularityNonDecrease is a rapid property test asserting
// that Leiden's reported partition has modularity at least as high as
// the singleton partition on the same graph — i.e. the algorithm
// never returns a worse-than-trivial partition.
//
// We generate small Erdos-Renyi-like undirected graphs, run Leiden,
// and compute Q for both the Leiden partition and the singleton
// baseline. Q(Leiden) must satisfy Q(Leiden) >= Q(singleton) - 1e-9.
func TestLeiden_ModularityNonDecrease(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(4, 12).Draw(rt, "n")
		// Build a small undirected graph by drawing edges from a
		// flat-prob coin per pair.
		p := rapid.Float64Range(0.2, 0.7).Draw(rt, "p")
		seed := rapid.Int64().Draw(rt, "seed")
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		// Reproducible PRNG via Park-Miller-style LCG keyed on seed.
		state := uint64(seed)
		nextRand := func() float64 {
			state = state*6364136223846793005 + 1442695040888963407
			return float64(state>>11) / (1 << 53)
		}
		edgesAdded := 0
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if nextRand() < p {
					a.AddEdge(i, j, struct{}{})
					edgesAdded++
				}
			}
		}
		if edgesAdded == 0 {
			return // empty graph — Leiden returns 0 communities, trivially OK
		}
		c := csr.BuildFromAdjList(a)
		part := Leiden(c, DefaultLeidenOptions())

		// Compute Q on the original CSR for the Leiden partition and
		// for the singleton baseline (each node its own community).
		qLeiden := modularityOnCSR(c, part.Community)
		// Singleton baseline: assign each live node a unique ID.
		mask := c.LiveMask()
		singleton := make([]int, len(mask))
		next := 0
		for id, m := range mask {
			if m {
				singleton[id] = next
				next++
			} else {
				singleton[id] = -1
			}
		}
		qSingle := modularityOnCSR(c, singleton)
		if qLeiden < qSingle-1e-9 {
			rt.Fatalf("Leiden Q=%.6f < singleton Q=%.6f (n=%d, p=%.2f, seed=%d, edges=%d)",
				qLeiden, qSingle, n, p, seed, edgesAdded)
		}
	})
}

// modularityOnCSR computes Q for an undirected CSR + partition.
// Skips NodeIDs flagged as -1 in comm.
func modularityOnCSR[W any](c *csr.CSR[W], comm []int) float64 {
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	maxID := int(c.MaxNodeID())
	if maxID == 0 {
		return 0
	}
	m2 := float64(c.Size()) // CSR doubles undirected edges; c.Size() is the doubled count
	if m2 == 0 {
		return 0
	}
	cMax := 0
	for _, x := range comm {
		if x+1 > cMax {
			cMax = x + 1
		}
	}
	sigmaIn := make([]float64, cMax)
	sigmaTot := make([]float64, cMax)
	for v := 0; v < maxID; v++ {
		cv := comm[v]
		if cv < 0 {
			continue
		}
		deg := float64(verts[v+1] - verts[v])
		sigmaTot[cv] += deg
		for k := verts[v]; k < verts[v+1]; k++ {
			u := int(edges[k])
			if comm[u] == cv {
				sigmaIn[cv] += 1.0
			}
		}
	}
	var q float64
	for c := 0; c < cMax; c++ {
		q += sigmaIn[c]/m2 - (sigmaTot[c]/m2)*(sigmaTot[c]/m2)
	}
	return q
}
