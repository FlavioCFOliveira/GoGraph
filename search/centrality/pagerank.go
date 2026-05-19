package centrality

import (
	"gograph/graph/csr"
)

// PageRankOptions configures [PageRank].
type PageRankOptions struct {
	Damping       float64
	MaxIterations int
	Tolerance     float64
}

// DefaultPageRankOptions returns the classic Brin-Page parameters
// (damping 0.85, max 100 iterations, tolerance 1e-6).
func DefaultPageRankOptions() PageRankOptions {
	return PageRankOptions{Damping: 0.85, MaxIterations: 100, Tolerance: 1e-6}
}

// PageRank runs the in-memory power-iteration form of PageRank
// over c and returns the per-NodeID rank slice plus the iteration
// count to convergence (capped at MaxIterations).
//
// The implementation mirrors the semi-external variant in
// search/extern.PageRank but operates on the in-memory CSR
// directly. It is the right choice when the graph fits in RAM and
// the caller wants the simplest API.
//
// L1 convergence as one cohesive routine.
//
//nolint:gocyclo // textbook PageRank: defaults + seed + iteration +
func PageRank[W any](c *csr.CSR[W], opts PageRankOptions) (ranks []float64, iterations int) {
	if opts.Damping == 0 {
		opts.Damping = 0.85
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 100
	}
	if opts.Tolerance <= 0 {
		opts.Tolerance = 1e-6
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	n := len(verts) - 1
	if n <= 0 {
		return nil, 0
	}
	live := 0
	outdeg := make([]float64, n)
	for i := 0; i < n; i++ {
		deg := verts[i+1] - verts[i]
		if deg > 0 {
			outdeg[i] = float64(deg)
			live++
		}
	}
	if live == 0 {
		return make([]float64, n), 0
	}
	cur := make([]float64, n)
	next := make([]float64, n)
	for i := range cur {
		if outdeg[i] > 0 {
			cur[i] = 1.0 / float64(live)
		}
	}
	teleport := (1 - opts.Damping) / float64(live)
	for iter := 1; iter <= opts.MaxIterations; iter++ {
		for i := range next {
			if outdeg[i] > 0 {
				next[i] = teleport
			} else {
				next[i] = 0
			}
		}
		for src := 0; src < n; src++ {
			if outdeg[src] == 0 {
				continue
			}
			share := opts.Damping * cur[src] / outdeg[src]
			for k := verts[src]; k < verts[src+1]; k++ {
				next[int(edges[k])] += share
			}
		}
		var delta float64
		for i := range cur {
			d := next[i] - cur[i]
			if d < 0 {
				d = -d
			}
			delta += d
		}
		cur, next = next, cur
		if delta < opts.Tolerance {
			return cur, iter
		}
	}
	return cur, opts.MaxIterations
}
