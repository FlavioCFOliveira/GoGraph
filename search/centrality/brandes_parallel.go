package centrality

import (
	"context"
	"runtime"
	"sync"

	"gograph/graph"
	"gograph/graph/csr"
	"gograph/internal/metrics"
)

// BetweennessParallel computes the exact (unweighted) betweenness
// centrality of every NodeID in c using the Brandes algorithm
// parallelised across sources. Each worker goroutine processes a
// disjoint range of source vertices, accumulating into its own
// private centrality buffer; the final reduction sums these buffers
// into the returned slice. Output is bit-identical to [Betweenness]
// (the order of additions is deterministic).
//
// numWorkers <= 0 picks runtime.GOMAXPROCS(0). For tiny graphs (V
// below ~1024) the parallel overhead dominates and the serial
// [Betweenness] is preferable.
func BetweennessParallel[W any](c *csr.CSR[W], numWorkers int) []float64 {
	defer metrics.Time("search.centrality.BetweennessParallel")()
	out, _ := BetweennessParallelCtx(context.Background(), c, numWorkers)
	return out
}

// BetweennessParallelCtx is the context-aware variant of
// [BetweennessParallel]. ctx cancellation is checked once per
// source vertex inside every worker; on cancellation returns
// (nil, wrapped ctx.Err()).
func BetweennessParallelCtx[W any](ctx context.Context, c *csr.CSR[W], numWorkers int) ([]float64, error) {
	defer metrics.Time("search.centrality.BetweennessParallelCtx")()
	n := int(c.MaxNodeID())
	cb := make([]float64, n)
	if n == 0 {
		return cb, nil
	}
	if numWorkers <= 0 {
		numWorkers = runtime.GOMAXPROCS(0)
	}
	if numWorkers > n {
		numWorkers = n
	}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()

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
			localCB := make([]float64, n)
			sigma := make([]float64, n)
			dist := make([]int, n)
			delta := make([]float64, n)
			pred := make([][]int, n)
			queue := make([]int, 0, n)
			stack := make([]int, 0, n)
			for s := w; s < n; s += numWorkers {
				if err := ctx.Err(); err != nil {
					results[w].err = err
					return
				}
				queue, stack = brandesSource(s, n, verts, edges, sigma, dist, delta, pred, localCB, queue, stack)
			}
			results[w].cb = localCB
		}(w)
	}
	wg.Wait()
	for _, r := range results {
		if r.err != nil {
			metrics.IncCounter("search.centrality.BetweennessParallelCtx.errors", 1)
			return nil, r.err
		}
	}
	// Deterministic final reduce: sum every worker's localCB in
	// worker-id order.
	for w := 0; w < numWorkers; w++ {
		for i := 0; i < n; i++ {
			cb[i] += results[w].cb[i]
		}
	}
	return cb, nil
}

// Silence the linter for the unused graph import on builds where it
// would otherwise complain — it's used inside brandesSource which
// lives in the same package.
var _ graph.NodeID
