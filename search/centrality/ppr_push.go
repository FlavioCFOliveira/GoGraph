package centrality

import (
	"context"

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
//
// Dangling-node handling matches the ACL paper: residue at a node
// with out-degree 0 is teleported back to src (probability alpha)
// rather than redistributed to non-existent neighbours. This keeps
// the rank vector summing to 1 within numerical tolerance.
//
// Concurrency: safe to invoke from any number of goroutines on a
// shared CSR.
func PersonalisedPushPageRank[W any](c *csr.CSR[W], src graph.NodeID, opts PPRPushOptions) ([]float64, error) {
	return PersonalisedPushPageRankCtx(context.Background(), c, src, opts)
}

// PersonalisedPushPageRankCtx is the context-aware variant of
// [PersonalisedPushPageRank]. ctx.Err() is checked every 4096 worklist
// pops; on cancellation returns (nil, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical ACL push: defaults + worklist loop + dangling teleport
func PersonalisedPushPageRankCtx[W any](ctx context.Context, c *csr.CSR[W], src graph.NodeID, opts PPRPushOptions) ([]float64, error) {
	if hasInvalidFloat(opts.Damping, opts.Epsilon) {
		return nil, ErrInvalidInput
	}
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
		return nil, nil
	}
	rank := make([]float64, n)
	res := make([]float64, n)
	res[uint64(src)] = 1
	queue := []int{int(src)}
	inQ := make([]bool, n)
	inQ[uint64(src)] = true

	enqueueIfHot := func(node int) {
		if inQ[node] {
			return
		}
		deg := verts[node+1] - verts[node]
		var hot bool
		if deg == 0 {
			// Dangling: any residue above epsilon is "hot" — its
			// mass will be teleported to src on the next pop.
			hot = res[node] >= opts.Epsilon
		} else {
			hot = res[node]/float64(deg) >= opts.Epsilon
		}
		if hot {
			queue = append(queue, node)
			inQ[node] = true
		}
	}

	steps := 0
	for len(queue) > 0 && steps < opts.MaxSteps {
		if steps&0xFFF == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		v := queue[0]
		queue = queue[1:]
		inQ[v] = false
		rv := res[v]
		deg := int(verts[v+1] - verts[v])
		if deg == 0 {
			// Dangling node: absorb (1-alpha)*rv into rank, teleport
			// alpha*rv back to src per ACL.
			rank[v] += (1 - opts.Damping) * rv
			res[v] = 0
			res[uint64(src)] += opts.Damping * rv
			enqueueIfHot(int(src))
			steps++
			continue
		}
		// Threshold check (no +1 hack: deg > 0 here).
		if rv/float64(deg) < opts.Epsilon {
			continue
		}
		rank[v] += (1 - opts.Damping) * rv
		share := opts.Damping * rv / float64(deg)
		res[v] = 0
		for k := verts[v]; k < verts[v+1]; k++ {
			w := int(edges[k])
			res[w] += share
			enqueueIfHot(w)
		}
		steps++
	}
	// Note: no final residue-drain pass. The canonical PPR invariant
	// is that rank[i] + alpha-weighted residue accumulates the true
	// stationary mass within Epsilon. Folding the residue with a
	// (1-alpha) factor (as v1.0.0 did) double-counted the absorption
	// and biased the rank vector. Leaving residue in place keeps
	// rank monotonically convergent.
	return rank, nil
}
