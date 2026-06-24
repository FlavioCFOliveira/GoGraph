package centrality

import (
	"context"
	"fmt"
	"runtime"
	"runtime/pprof"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// BetweennessParallel computes the exact (unweighted) betweenness
// centrality of every NodeID in c using the Brandes algorithm
// parallelised across sources. Each worker goroutine processes a
// disjoint range of source vertices, accumulating into its own
// private centrality buffer; the final reduction sums these buffers
// into the returned slice.
//
// Output is deterministic for a fixed numWorkers value. Due to
// non-associative floating-point addition, the result may differ from
// [Betweenness] by up to ~1e-12 per node when numWorkers > 1; the
// two agree within this numerical tolerance. For exact bit-identity
// with the serial result, use [Betweenness] directly.
//
// numWorkers <= 0 picks runtime.GOMAXPROCS(0). For tiny graphs (V
// below ~1024) the parallel overhead dominates and the serial
// [Betweenness] is preferable.
func BetweennessParallel[W any](c *csr.CSR[W], numWorkers int) []float64 {
	defer metrics.Time("search.centrality.BetweennessParallel").Stop()
	out, _ := BetweennessParallelCtx(context.Background(), c, numWorkers)
	return out
}

// BetweennessParallelCtx is the context-aware variant of
// [BetweennessParallel]. ctx cancellation is checked once per
// source vertex inside every worker; on cancellation returns
// (nil, wrapped ctx.Err()).
func BetweennessParallelCtx[W any](ctx context.Context, c *csr.CSR[W], numWorkers int) ([]float64, error) {
	defer metrics.Time("search.centrality.BetweennessParallelCtx").Stop()
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
	// In-degrees are read-only and identical for every worker, so
	// compute them once and share the slice; each worker still builds
	// its own arena (the only mutable state) over this shared bound.
	indeg := computeInDegrees(n, verts, edges)

	// Cancellation cascade: any worker that observes ctx.Err() (or
	// fails its work) calls cancel() on the shared cancellable
	// context, which propagates the cancellation to every sibling
	// worker via their per-source ctx.Err() poll. Without this the
	// surviving workers would continue iterating their source range
	// after the parent ctx was already cancelled, wasting CPU.
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
			pprof.Do(workCtx, pprof.Labels("component", "betweenness-parallel", "worker", fmt.Sprintf("%d", w)),
				func(wCtx context.Context) {
					localCB := make([]float64, n)
					sigma := make([]float64, n)
					dist := make([]int, n)
					delta := make([]float64, n)
					// Each worker owns a private predecessor arena —
					// predArena is not safe for concurrent use, so it
					// must never be shared across workers. The shared
					// indeg slice is read-only.
					pred := newPredArena(n, indeg)
					queue := make([]int, 0, n)
					stack := make([]int, 0, n)
					for s := w; s < n; s += numWorkers {
						if err := wCtx.Err(); err != nil {
							results[w].err = err
							cancel()
							return
						}
						queue, stack = brandesSource(s, n, verts, edges, sigma, dist, delta, pred, localCB, queue, stack)
					}
					results[w].cb = localCB
				})
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
