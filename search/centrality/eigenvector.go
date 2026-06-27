package centrality

import (
	"context"
	"fmt"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// EigenvectorOptions configures [Eigenvector]. It is an immutable value with no
// shared state and is safe for concurrent use (copy it freely across goroutines).
type EigenvectorOptions struct {
	MaxIterations int
	Tolerance     float64
}

// DefaultEigenvectorOptions returns the NetworkX-compatible parameters
// (max 100 iterations, tolerance 1e-6).
func DefaultEigenvectorOptions() EigenvectorOptions {
	return EigenvectorOptions{MaxIterations: 100, Tolerance: 1e-6}
}

// Eigenvector computes eigenvector centrality over the immutable snapshot c by
// power iteration, returning an L2-normalised per-NodeID slice of length
// c.MaxNodeID().
//
// A node's score is proportional to the sum of its neighbours' scores — the
// dominant eigenvector of the adjacency matrix. The iteration uses the
// NetworkX recurrence x ← x + A·x (equivalently power iteration on I+A), which
// shares A's dominant eigenvector but, unlike plain power iteration, converges
// on bipartite graphs (stars, paths, even cycles) instead of oscillating.
//
// Orientation: a node accumulates the scores of its IN-neighbours (predecessors)
// — the left dominant eigenvector, matching NetworkX. For the out-edge variant
// pass c.BuildReverse(). On an undirected snapshot the two coincide. Self-loops
// (diagonal of A) and parallel edges (edge multiplicity) are preserved and DO
// affect the result, as the measure is defined on A.
//
// Caveats (Perron-Frobenius): a unique strictly-positive eigenvector is
// guaranteed only on a strongly-connected (irreducible) graph. On a
// disconnected graph the vector concentrates on the dominant component and is
// ~0 elsewhere; on a directed acyclic graph the measure is degenerate (use
// [Katz] or [PageRank]). An edgeless graph has no eigenvector structure and
// yields all-zero scores. If the iteration does not converge within
// MaxIterations, Eigenvector returns [ErrMaxStepsExceeded] (never a half-
// converged iterate).
//
// Concurrency: Eigenvector allocates its own buffers per call and is safe for
// concurrent use on a snapshot CSR.
//
// Reference: Bonacich, J. Math. Sociology 2 (1972) 113-120.
func Eigenvector[W any](c *csr.CSR[W], opts EigenvectorOptions) ([]float64, int, error) {
	defer metrics.Time("search.centrality.Eigenvector").Stop()
	out, iters, err := EigenvectorCtx(context.Background(), c, opts)
	if err != nil {
		metrics.IncCounter("search.centrality.Eigenvector.errors", 1)
	}
	return out, iters, err
}

// EigenvectorCtx is the context-aware variant of [Eigenvector]. ctx.Err() is
// checked at every iteration boundary.
func EigenvectorCtx[W any](ctx context.Context, c *csr.CSR[W], opts EigenvectorOptions) ([]float64, int, error) {
	defer metrics.Time("search.centrality.EigenvectorCtx").Stop()
	if opts.MaxIterations <= 0 || opts.Tolerance <= 0 || hasInvalidFloat(opts.Tolerance) {
		return nil, 0, fmt.Errorf("%w: eigenvector options", ErrInvalidInput)
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	slots := len(verts) - 1
	if slots <= 0 {
		return nil, 0, nil
	}
	live, liveCount := liveMask(verts, edges, slots)
	if liveCount == 0 {
		return make([]float64, slots), 0, nil // no edges → no eigenvector structure
	}

	cur := make([]float64, slots)
	next := make([]float64, slots)
	start := 1.0 / math.Sqrt(float64(liveCount))
	for i := range cur {
		if live[i] {
			cur[i] = start
		}
	}

	threshold := float64(c.Order()) * opts.Tolerance
	for iter := 1; iter <= opts.MaxIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("eigenvector: %w", err)
		}
		// next = (I + A) · cur, with A accumulating over in-edges (scatter).
		copy(next, cur)
		for src := 0; src < slots; src++ {
			xs := cur[src]
			for k := verts[src]; k < verts[src+1]; k++ {
				next[edges[k]] += xs
			}
		}
		// L2-normalise.
		var norm float64
		for _, v := range next {
			norm += v * v
		}
		norm = math.Sqrt(norm)
		if norm == 0 {
			return make([]float64, slots), iter, nil
		}
		inv := 1.0 / norm
		var change float64
		for i := range next {
			next[i] *= inv
			change += math.Abs(next[i] - cur[i])
		}
		cur, next = next, cur
		if change < threshold {
			return cur, iter, nil
		}
	}
	return nil, opts.MaxIterations, ErrMaxStepsExceeded
}
