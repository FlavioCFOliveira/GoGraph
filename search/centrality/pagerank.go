package centrality

import (
	"context"
	"errors"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// ErrInvalidInput is returned by centrality algorithms when their
// float options carry NaN or +/-Inf. Non-finite inputs propagate
// through the power iteration / push loops and silently corrupt
// the rank vector; validating once at entry is mandatory.
var ErrInvalidInput = errors.New("centrality: input option contains NaN or Inf")

// ErrNonPositiveWeight is returned by [WeightedBetweenness] and
// [WeightedBetweennessCtx] when any edge weight is zero or negative.
// Brandes' weighted variant builds a predecessor DAG ordered by
// shortest-path distance using Dijkstra; zero-weight edges connect
// two nodes at equal distance, which can cause a node to be settled
// (and its σ consumed downstream) before a later-settled equal-distance
// predecessor has accumulated its contribution — making σ inconsistent
// and silently corrupting the centrality values. Strictly positive
// weights are therefore required.
var ErrNonPositiveWeight = errors.New("centrality: edge weight must be strictly positive")

// hasInvalidFloat returns true when any of values is NaN or +/-Inf.
func hasInvalidFloat(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return true
		}
	}
	return false
}

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
// The returned slice has length c.MaxNodeID(); only NodeIDs that
// participate in at least one edge (live nodes) carry non-zero rank.
// The sum over the slice equals 1.0 within numerical tolerance.
//
// Concurrency: PageRank is safe to invoke from any number of
// goroutines on a snapshot CSR; the function allocates its working
// buffers per call and does not share state.
//
// Algorithm. The implementation is the textbook power-iteration form
// with proper handling of dangling nodes (nodes with out-degree 0):
// at each iteration the mass currently held by dangling nodes is
// redistributed uniformly across all live nodes, modelling them as
// teleporting their entire share back into the system. This ensures
// total mass is conserved and the result is a true stationary
// distribution.
func PageRank[W any](c *csr.CSR[W], opts PageRankOptions) (ranks []float64, iterations int, err error) {
	defer metrics.Time("search.centrality.PageRank")()
	ranks, iterations, err = PageRankCtx(context.Background(), c, opts)
	if err != nil {
		metrics.IncCounter("search.centrality.PageRank.errors", 1)
	}
	return ranks, iterations, err
}

// PageRankCtx is the context-aware variant of [PageRank]. ctx.Err()
// is checked at every iteration boundary; on cancellation returns
// (nil, 0, wrapped ctx.Err()).
//
//nolint:gocyclo // canonical power-iteration: defaults + live detection + iteration loop
func PageRankCtx[W any](ctx context.Context, c *csr.CSR[W], opts PageRankOptions) (ranks []float64, iterations int, err error) {
	defer metrics.Time("search.centrality.PageRankCtx")()
	if hasInvalidFloat(opts.Damping, opts.Tolerance) {
		metrics.IncCounter("search.centrality.PageRankCtx.errors", 1)
		return nil, 0, ErrInvalidInput
	}
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
		return nil, 0, nil
	}

	// A node is "live" if it has at least one incident edge (in or out).
	// Dangling sinks (out-degree 0, in-degree > 0) count as live;
	// totally isolated ghost slots from sharded NodeID packing do not.
	isLive := make([]bool, n)
	// outdeg is logically an unsigned integer count; storing it as
	// uint32 halves its memory footprint (n nodes saved 4 bytes each
	// vs the v1.0 float64) and doubles its L1 lane density. The
	// per-source float64 conversion at use site is a single FCVT
	// instruction with no measurable cost on M4-class cores.
	outdeg := make([]uint32, n)
	for i := 0; i < n; i++ {
		deg := verts[i+1] - verts[i]
		if deg > 0 {
			outdeg[i] = uint32(deg)
			isLive[i] = true
			for k := verts[i]; k < verts[i+1]; k++ {
				isLive[int(edges[k])] = true
			}
		}
	}
	live := 0
	for i := 0; i < n; i++ {
		if isLive[i] {
			live++
		}
	}
	if live == 0 {
		return make([]float64, n), 0, nil
	}

	cur := make([]float64, n)
	next := make([]float64, n)
	initRank := 1.0 / float64(live)
	for i := 0; i < n; i++ {
		if isLive[i] {
			cur[i] = initRank
		}
	}
	teleport := (1 - opts.Damping) / float64(live)

	for iter := 1; iter <= opts.MaxIterations; iter++ {
		if cerr := ctx.Err(); cerr != nil {
			metrics.IncCounter("search.centrality.PageRankCtx.errors", 1)
			return nil, 0, cerr
		}
		// Dangling mass: sum of cur[i] for live nodes with no out-edges.
		// Redistributed uniformly across all live nodes (canonical PageRank).
		var danglingMass float64
		for i := 0; i < n; i++ {
			if isLive[i] && outdeg[i] == 0 {
				danglingMass += cur[i]
			}
		}
		baseShare := teleport + opts.Damping*danglingMass/float64(live)

		// Seed every live node with teleport + dangling redistribution.
		for i := 0; i < n; i++ {
			if isLive[i] {
				next[i] = baseShare
			} else {
				next[i] = 0
			}
		}

		// Distribute outgoing mass from non-dangling sources.
		for src := 0; src < n; src++ {
			if outdeg[src] == 0 {
				continue
			}
			share := opts.Damping * cur[src] / float64(outdeg[src])
			for k := verts[src]; k < verts[src+1]; k++ {
				next[int(edges[k])] += share
			}
		}

		// L1 delta.
		var delta float64
		for i := 0; i < n; i++ {
			d := next[i] - cur[i]
			if d < 0 {
				d = -d
			}
			delta += d
		}

		cur, next = next, cur
		if delta < opts.Tolerance {
			return cur, iter, nil
		}
	}
	return cur, opts.MaxIterations, nil
}
