package centrality

import (
	"context"
	"fmt"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// KatzOptions configures [Katz].
//
// Alpha is the attenuation factor. Convergence of the Katz series requires
// Alpha < 1/λ_max, where λ_max is the largest eigenvalue of the adjacency
// matrix. When Alpha <= 0 a safe default is chosen automatically from the
// degree bound (λ_max ≤ d_max): Alpha = 0.85 / (1 + maxInDegree), which always
// satisfies the convergence condition. Supply an explicit Alpha only when you
// know it stays below 1/λ_max.
//
// Beta is the per-node baseline status (default 1.0 when <= 0).
type KatzOptions struct {
	Alpha         float64
	Beta          float64
	MaxIterations int
	Tolerance     float64
}

// DefaultKatzOptions returns parameters with auto-selected Alpha (0 sentinel),
// Beta 1.0, max 1000 iterations, tolerance 1e-6.
func DefaultKatzOptions() KatzOptions {
	return KatzOptions{Alpha: 0, Beta: 1.0, MaxIterations: 1000, Tolerance: 1e-6}
}

// Katz computes Katz centrality over the immutable snapshot c, returning an
// L2-normalised per-NodeID slice of length c.MaxNodeID().
//
// Katz centrality is the fixed point x = α·Aᵀ·x + β·1: a node's score is a
// baseline β plus α times the attenuated scores reaching it along incoming
// paths. The β baseline gives every node a non-zero floor, so — unlike
// [Eigenvector] — Katz is well-defined on disconnected graphs and directed
// acyclic graphs.
//
// Orientation: a node accumulates the attenuated scores of its IN-neighbours
// (left eigenvector, matching NetworkX). For the out-edge variant pass
// c.BuildReverse(); on an undirected snapshot the two coincide. Self-loops and
// parallel edges are taken from A and DO affect the result.
//
// Alpha must keep the series convergent (Alpha < 1/λ_max); see [KatzOptions]
// for the auto-selected safe default. If the iteration does not converge within
// MaxIterations, Katz returns [ErrMaxStepsExceeded].
//
// Representation note: Katz scores only participating nodes (≥1 incident edge).
// The immutable CSR cannot tell a genuinely isolated node from an unused slot
// in the sharded NodeID space, so isolated/ghost slots receive 0 rather than
// the textbook β floor — consistent with [PageRank] and [Eigenvector].
//
// Concurrency: Katz allocates its own buffers per call and is safe for
// concurrent use on a snapshot CSR.
//
// Reference: Katz, L., Psychometrika 18 (1953) 39-43.
func Katz[W any](c *csr.CSR[W], opts KatzOptions) ([]float64, int, error) {
	defer metrics.Time("search.centrality.Katz").Stop()
	out, iters, err := KatzCtx(context.Background(), c, opts)
	if err != nil {
		metrics.IncCounter("search.centrality.Katz.errors", 1)
	}
	return out, iters, err
}

// KatzCtx is the context-aware variant of [Katz]. ctx.Err() is checked at every
// iteration boundary.
func KatzCtx[W any](ctx context.Context, c *csr.CSR[W], opts KatzOptions) ([]float64, int, error) {
	defer metrics.Time("search.centrality.KatzCtx").Stop()
	if opts.MaxIterations <= 0 || opts.Tolerance <= 0 || hasInvalidFloat(opts.Tolerance, opts.Alpha, opts.Beta) {
		return nil, 0, fmt.Errorf("%w: katz options", ErrInvalidInput)
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	slots := len(verts) - 1
	if slots <= 0 {
		return nil, 0, nil
	}
	beta := opts.Beta
	if beta <= 0 {
		beta = 1.0
	}
	alpha := opts.Alpha
	if alpha <= 0 {
		alpha = autoKatzAlpha(verts, edges, slots)
	}
	if alpha <= 0 {
		// No edges: degree bound gives alpha = 0.85; the fixed point is β·1.
		alpha = 0.85
	}

	live, liveCount := liveMask(verts, edges, slots)
	if liveCount == 0 {
		return make([]float64, slots), 0, nil
	}

	cur := make([]float64, slots)
	next := make([]float64, slots)
	for i := range cur {
		if live[i] {
			cur[i] = beta
		}
	}

	threshold := float64(c.Order()) * opts.Tolerance
	for iter := 1; iter <= opts.MaxIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("katz: %w", err)
		}
		// next = α·Aᵀ·cur + β on participating nodes (scatter over in-edges).
		for i := range next {
			if live[i] {
				next[i] = beta
			} else {
				next[i] = 0
			}
		}
		for src := 0; src < slots; src++ {
			xs := alpha * cur[src]
			for k := verts[src]; k < verts[src+1]; k++ {
				next[edges[k]] += xs
			}
		}
		var change float64
		for i := range next {
			change += math.Abs(next[i] - cur[i])
		}
		cur, next = next, cur
		if change < threshold {
			return normaliseL2(cur), iter, nil
		}
	}
	return nil, opts.MaxIterations, ErrMaxStepsExceeded
}

// autoKatzAlpha returns 0.85/(1+maxInDegree), which is guaranteed below
// 1/λ_max because λ_max ≤ d_max (max degree). Returns 0 for an edgeless graph.
func autoKatzAlpha(verts []uint64, edges []graph.NodeID, slots int) float64 {
	indeg := make([]uint32, slots)
	for src := 0; src < slots; src++ {
		for k := verts[src]; k < verts[src+1]; k++ {
			indeg[edges[k]]++
		}
	}
	var maxIn uint32
	for _, d := range indeg {
		if d > maxIn {
			maxIn = d
		}
	}
	if maxIn == 0 {
		return 0
	}
	return 0.85 / float64(1+maxIn)
}

// normaliseL2 scales v to unit L2 norm in place and returns it. A zero vector
// is returned unchanged.
func normaliseL2(v []float64) []float64 {
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return v
	}
	inv := 1.0 / norm
	for i := range v {
		v[i] *= inv
	}
	return v
}
