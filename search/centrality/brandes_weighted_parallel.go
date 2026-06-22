package centrality

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"runtime/pprof"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// WeightedBetweennessParallel computes the weighted betweenness
// centrality of every NodeID in c using Dijkstra-augmented Brandes
// (Brandes 2001 §3, weighted variant) parallelised across sources.
// Each worker goroutine processes a disjoint stripe of source
// vertices, accumulating into its own private centrality buffer; the
// final reduction sums those buffers into the returned slice in
// worker-id order. Edge weights must be finite and strictly positive.
//
// Output is deterministic for a fixed numWorkers value. It is NOT
// numerically equal to the serial [WeightedBetweenness]: parallelising
// over sources re-associates the cross-source dependency sum, and
// IEEE-754 floating-point addition is non-associative, so the result
// may differ from [WeightedBetweenness] by up to ~1e-12 per node when
// numWorkers > 1; the two agree within this numerical tolerance. For
// an exact, reproducible-against-serial result use
// [WeightedBetweenness] directly.
//
// numWorkers <= 0 picks runtime.GOMAXPROCS(0). For tiny graphs (V
// below ~1024) the parallel overhead dominates and the serial
// [WeightedBetweenness] is preferable.
//
// Input contract. Returns [ErrInvalidInput] when any edge weight is
// NaN or +/-Inf; returns [ErrNonPositiveWeight] when any edge weight
// is zero or negative — identical to [WeightedBetweenness].
//
// Concurrency: WeightedBetweennessParallel reads the immutable CSR
// without synchronisation and is safe to invoke concurrently on a
// shared CSR; every worker owns its private scratch.
func WeightedBetweennessParallel(c *csr.CSR[float64], numWorkers int) ([]float64, error) {
	defer metrics.Time("search.centrality.WeightedBetweennessParallel")()
	return WeightedBetweennessParallelCtx(context.Background(), c, numWorkers)
}

// WeightedBetweennessParallelCtx is the context-aware variant of
// [WeightedBetweennessParallel]. ctx cancellation is checked once per
// source vertex inside every worker; on cancellation returns
// (nil, wrapped ctx.Err()).
func WeightedBetweennessParallelCtx(ctx context.Context, c *csr.CSR[float64], numWorkers int) ([]float64, error) {
	defer metrics.Time("search.centrality.WeightedBetweennessParallelCtx")()
	n := int(c.MaxNodeID())
	cb := make([]float64, n)
	if n == 0 {
		return cb, nil
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	weights := c.WeightsSlice()
	// Input validation: NaN/Inf silently corrupt sigma and dist;
	// non-positive weights break Dijkstra's correctness guarantee.
	// Fail fast at the public-API boundary before any work is done,
	// mirroring [WeightedBetweennessCtx].
	for _, w := range weights {
		if math.IsNaN(w) || math.IsInf(w, 0) {
			metrics.IncCounter("search.centrality.WeightedBetweennessParallelCtx.errors", 1)
			return nil, ErrInvalidInput
		}
		if w <= 0 {
			metrics.IncCounter("search.centrality.WeightedBetweennessParallelCtx.errors", 1)
			return nil, ErrNonPositiveWeight
		}
	}
	if numWorkers <= 0 {
		numWorkers = runtime.GOMAXPROCS(0)
	}
	if numWorkers > n {
		numWorkers = n
	}

	// Cancellation cascade: any worker that observes ctx.Err() calls
	// cancel() on the shared cancellable context, which propagates the
	// cancellation to every sibling worker via their per-source
	// ctx.Err() poll. Without this the surviving workers would keep
	// iterating their source stripe after the parent ctx was already
	// cancelled, wasting CPU.
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		cb  []float64
		err error
	}
	results := make([]result, numWorkers)
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			pprof.Do(workCtx, pprof.Labels("component", "weighted-betweenness-parallel", "worker", fmt.Sprintf("%d", w)),
				func(wCtx context.Context) {
					// Each worker owns a private scratch set — none of
					// these structures is safe for concurrent use, so
					// they must never be shared across workers. The
					// verts/edges/weights slices are read-only.
					localCB := make([]float64, n)
					sigma := make([]float64, n)
					dist := make([]float64, n)
					delta := make([]float64, n)
					pred := make([][]int, n)
					stack := make([]int, 0, n)
					h := newWeightedHeap(n)
					for s := w; s < n; s += numWorkers {
						if err := wCtx.Err(); err != nil {
							results[w].err = err
							cancel()
							return
						}
						weightedBrandesSource(s, n, verts, edges, weights, sigma, dist, delta, pred, localCB, &stack, h)
					}
					results[w].cb = localCB
				})
		}(w)
	}
	wg.Wait()
	for _, r := range results {
		if r.err != nil {
			metrics.IncCounter("search.centrality.WeightedBetweennessParallelCtx.errors", 1)
			return nil, r.err
		}
	}
	// Deterministic final reduce: sum every worker's localCB in
	// worker-id order so the output is reproducible for a fixed
	// numWorkers.
	for w := 0; w < numWorkers; w++ {
		for i := 0; i < n; i++ {
			cb[i] += results[w].cb[i]
		}
	}
	return cb, nil
}
