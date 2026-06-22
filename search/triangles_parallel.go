package search

import (
	"context"
	"fmt"
	"runtime"
	"runtime/pprof"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// CountTrianglesParallel returns the total number of triangles in the
// undirected graph c plus a per-NodeID participation count, computed
// with the same degree-ordered node-iterator algorithm as
// [CountTriangles] but with the outer per-vertex loop striped across a
// bounded worker pool. c must be a symmetric directed CSR (each
// undirected edge present as both (u, v) and (v, u)).
//
// The result is bit-identical to [CountTriangles]. The triangle count
// is an integer-addition monoid: each triangle is tallied exactly once
// (at its lowest-ranked vertex), each worker accumulates into a private
// total and a private per-node buffer, and the partials are summed
// exactly during the reduce. Integer addition is associative and
// commutative, so the totals are independent of the worker count and of
// the order in which vertices are scheduled.
//
// numWorkers <= 0 picks runtime.GOMAXPROCS(0). For tiny graphs the
// parallel overhead and the per-worker per-node buffers dominate, so
// the serial [CountTriangles] is preferable.
//
// Concurrency: CountTrianglesParallel reads the immutable CSR without
// synchronisation (the per-vertex-sorted adjacency is computed once in
// a fresh copy, never mutating c); every worker owns its private
// accumulators, so it is safe to invoke concurrently on a shared CSR.
//
// Complexity: the same O(E * sqrt(E)) total work as [CountTriangles],
// divided across numWorkers, plus O(numWorkers * V) for the per-worker
// per-node buffers.
func CountTrianglesParallel[W any](c *csr.CSR[W], numWorkers int) (total int64, perNode []int64) {
	defer metrics.Time("search.CountTrianglesParallel")()
	total, perNode, _ = CountTrianglesParallelCtx(context.Background(), c, numWorkers)
	return total, perNode
}

// CountTrianglesParallelCtx is the context-aware variant of
// [CountTrianglesParallel]. ctx.Err() is checked every 4096 candidate-
// pair iterations inside each worker; on cancellation returns
// (0, nil, wrapped ctx.Err()).
func CountTrianglesParallelCtx[W any](ctx context.Context, c *csr.CSR[W], numWorkers int) (total int64, perNode []int64, err error) {
	defer metrics.Time("search.CountTrianglesParallelCtx")()
	if cerr := ctx.Err(); cerr != nil {
		metrics.IncCounter("search.CountTrianglesParallelCtx.errors", 1)
		return 0, nil, cerr
	}
	n := int(c.MaxNodeID())
	if n == 0 {
		return 0, nil, nil
	}
	p := prepareTrianglePlan(c, n)

	if numWorkers <= 0 {
		numWorkers = runtime.GOMAXPROCS(0)
	}
	if numWorkers > n {
		numWorkers = n
	}

	// Cancellation cascade: any worker that observes ctx.Err() calls
	// cancel() on the shared cancellable context, so its siblings stop
	// at their next per-pair poll rather than finishing their stripes.
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		total   int64
		perNode []int64
		err     error
	}
	results := make([]result, numWorkers)
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			pprof.Do(workCtx, pprof.Labels("component", "triangles-parallel", "worker", fmt.Sprintf("%d", w)),
				func(wCtx context.Context) {
					// Private accumulators: no worker shares a counter,
					// so there is no contention and no data race on the
					// hot path. The plan p is read-only.
					localTotal := int64(0)
					localPerNode := make([]int64, n)
					loops := 0
					for v := w; v < n; v += numWorkers {
						if cerr := countTrianglesVertex(wCtx, p, v, &localTotal, localPerNode, &loops); cerr != nil {
							results[w].err = cerr
							cancel()
							return
						}
					}
					results[w].total = localTotal
					results[w].perNode = localPerNode
				})
		}(w)
	}
	wg.Wait()
	for _, r := range results {
		if r.err != nil {
			metrics.IncCounter("search.CountTrianglesParallelCtx.errors", 1)
			return 0, nil, r.err
		}
	}
	// Exact integer-sum reduce in worker-id order. Integer addition is
	// associative and commutative, so the result is bit-identical to
	// the serial count regardless of worker count or scheduling.
	perNode = make([]int64, n)
	for w := 0; w < numWorkers; w++ {
		total += results[w].total
		for i := 0; i < n; i++ {
			perNode[i] += results[w].perNode[i]
		}
	}
	return total, perNode, nil
}
