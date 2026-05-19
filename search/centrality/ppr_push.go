package centrality

import (
	"gograph/graph"
	"gograph/graph/csr"
)

// PPRPushOptions controls [PersonalisedPushPageRank].
type PPRPushOptions struct {
	// Damping is the random-jump probability (alpha; typical 0.85).
	Damping float64
	// Epsilon stops propagation when residue/outdeg falls below it.
	Epsilon float64
	// MaxSteps caps the number of push operations for safety.
	MaxSteps int
}

// DefaultPPRPushOptions returns the Andersen-Chung-Lang reference
// parameters (damping 0.85, epsilon 1e-6, max 1e7 steps).
func DefaultPPRPushOptions() PPRPushOptions {
	return PPRPushOptions{Damping: 0.85, Epsilon: 1e-6, MaxSteps: 10_000_000}
}

// PersonalisedPushPageRank computes the personalised PageRank
// vector seeded at src using the local-push algorithm
// (Andersen-Chung-Lang, FOCS 2006). Returns the rank vector indexed
// by NodeID.
//
// The algorithm pays only for the edges it touches, so on large
// graphs with a small high-probability cluster it runs in roughly
// O(1/epsilon) time rather than O(V+E).
func PersonalisedPushPageRank[W any](c *csr.CSR[W], src graph.NodeID, opts PPRPushOptions) []float64 {
	if opts.Damping == 0 {
		opts.Damping = 0.85
	}
	if opts.Epsilon == 0 {
		opts.Epsilon = 1e-6
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 10_000_000
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	n := len(verts) - 1
	if n <= 0 || uint64(src)+1 >= uint64(len(verts)) {
		return nil
	}
	rank := make([]float64, n)
	res := make([]float64, n)
	res[uint64(src)] = 1
	queue := []int{int(src)}
	inQ := make([]bool, n)
	inQ[uint64(src)] = true

	steps := 0
	for len(queue) > 0 && steps < opts.MaxSteps {
		v := queue[0]
		queue = queue[1:]
		inQ[v] = false
		rv := res[v]
		deg := float64(verts[v+1] - verts[v])
		if deg == 0 {
			rank[v] += rv
			res[v] = 0
			continue
		}
		if rv/deg < opts.Epsilon {
			continue
		}
		rank[v] += (1 - opts.Damping) * rv
		share := opts.Damping * rv / deg
		res[v] = 0
		for k := verts[v]; k < verts[v+1]; k++ {
			w := int(edges[k])
			res[w] += share
			if res[w]/float64(verts[w+1]-verts[w]+1) >= opts.Epsilon && !inQ[w] {
				queue = append(queue, w)
				inQ[w] = true
			}
		}
		steps++
	}
	// Drain residue into rank for any node not pushed (limits result error).
	for i, r := range res {
		rank[i] += r * (1 - opts.Damping)
	}
	return rank
}
