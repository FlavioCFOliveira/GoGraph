package extern

import (
	"gograph/graph"
	"gograph/store/csrfile"
)

// PageRankOptions configures [PageRank].
type PageRankOptions struct {
	// Damping is the random-jump probability (typical 0.85).
	Damping float64
	// MaxIterations bounds the iteration count.
	MaxIterations int
	// Tolerance is the convergence threshold on the L1 norm of the
	// rank delta.
	Tolerance float64
}

// DefaultPageRankOptions returns the classic Brin-Page configuration.
func DefaultPageRankOptions() PageRankOptions {
	return PageRankOptions{
		Damping:       0.85,
		MaxIterations: 100,
		Tolerance:     1e-6,
	}
}

// PageRank runs the power-iteration form of PageRank over the
// graph captured by an mmap-backed csrfile.Reader. Rank arrays live
// in RAM (size = nVertices); adjacency is streamed sequentially from
// the file each iteration.
//
// Returns the per-vertex rank slice (indexed by NodeID) and the
// iteration count at which convergence was reached.
func PageRank(r *csrfile.Reader, opts PageRankOptions) (ranks []float64, iterations int) {
	opts = normaliseOptions(opts)
	verts := r.Vertices()
	edges := r.Edges()
	if len(verts) <= 1 {
		return nil, 0
	}
	n := len(verts) - 1
	cur, outdeg, live := seedRanks(verts, n)
	if live == 0 {
		return cur, 0
	}
	next := make([]float64, n)
	teleport := (1.0 - opts.Damping) / float64(live)
	for iter := 1; iter <= opts.MaxIterations; iter++ {
		stepIteration(verts, edges, cur, next, outdeg, teleport, opts.Damping)
		delta := l1Delta(cur, next)
		cur, next = next, cur
		if delta < opts.Tolerance {
			return cur, iter
		}
	}
	return cur, opts.MaxIterations
}

func normaliseOptions(opts PageRankOptions) PageRankOptions {
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 100
	}
	if opts.Damping == 0 {
		opts.Damping = 0.85
	}
	if opts.Tolerance <= 0 {
		opts.Tolerance = 1e-6
	}
	return opts
}

func seedRanks(verts []uint64, n int) (ranks, outdeg []float64, live int) {
	ranks = make([]float64, n)
	outdeg = make([]float64, n)
	for i := 0; i < n; i++ {
		if deg := verts[i+1] - verts[i]; deg > 0 {
			outdeg[i] = float64(deg)
			live++
		}
	}
	if live == 0 {
		return ranks, outdeg, 0
	}
	inv := 1.0 / float64(live)
	for i := range ranks {
		if outdeg[i] > 0 {
			ranks[i] = inv
		}
	}
	return ranks, outdeg, live
}

func stepIteration(verts []uint64, edges []graph.NodeID, cur, next, outdeg []float64, teleport, damping float64) {
	for i := range next {
		if outdeg[i] > 0 {
			next[i] = teleport
		} else {
			next[i] = 0
		}
	}
	n := len(outdeg)
	for src := 0; src < n; src++ {
		if outdeg[src] == 0 {
			continue
		}
		share := damping * cur[src] / outdeg[src]
		start := verts[src]
		end := verts[src+1]
		for k := start; k < end; k++ {
			next[int(edges[k])] += share
		}
	}
}

func l1Delta(a, b []float64) float64 {
	var d float64
	for i := range a {
		x := a[i] - b[i]
		if x < 0 {
			x = -x
		}
		d += x
	}
	return d
}
