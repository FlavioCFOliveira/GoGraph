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

// JohnsonAPSPParallel computes APSP on c using Johnson's algorithm
// with the per-source reweighted-Dijkstra pass parallelised across a
// bounded worker pool. The Bellman-Ford reweighting prologue runs
// serially (it is inherently sequential and a negligible fraction of
// the total work on sparse graphs); only the |V| independent Dijkstra
// runs are distributed.
//
// The result is bit-identical to the serial [JohnsonAPSP]: each
// source's Dijkstra is independent and deterministic, the recovery
// arithmetic d(u,v) = d'(u,v) - h[u] + h[v] is computed per cell with
// no cross-source floating-point reduction, and distinct sources write
// to disjoint rows of the output matrix. The output therefore does not
// depend on the worker count or on the order in which sources are
// scheduled. (For floating-point W, the same ULP-level reweight/recover
// caveat documented on [JohnsonAPSP] versus [FloydWarshall] applies
// equally here; the parallel and serial Johnson outputs agree exactly.)
//
// numWorkers <= 0 picks runtime.GOMAXPROCS(0). For tiny graphs the
// parallel overhead dominates and the serial [JohnsonAPSP] is
// preferable. The input contract (NaN/+-Inf gate, negative-cycle
// detection via [ErrNegativeCycle], integer-overflow precondition) is
// identical to [JohnsonAPSP].
//
// Concurrency: JohnsonAPSPParallel reads the immutable CSR without
// synchronisation; every worker owns a private Dijkstra state and
// writes only its own source rows, so it is safe to invoke
// concurrently on a shared CSR.
//
// Complexity: O(V * (V + E) * log V) total work as [JohnsonAPSP],
// divided across numWorkers for the Dijkstra pass.
func JohnsonAPSPParallel[W Weight](c *csr.CSR[W], numWorkers int) (*APSP[W], error) {
	defer metrics.Time("search.JohnsonAPSPParallel").Stop()
	res, err := JohnsonAPSPParallelCtx(context.Background(), c, numWorkers)
	if err != nil {
		metrics.IncCounter("search.JohnsonAPSPParallel.errors", 1)
	}
	return res, err
}

// JohnsonAPSPParallelCtx is the context-aware variant of
// [JohnsonAPSPParallel]. ctx.Err() is checked at every
// relaxation-round boundary during the serial Bellman-Ford prologue
// and once per source vertex inside every Dijkstra worker; on
// cancellation returns (nil, wrapped ctx.Err()).
func JohnsonAPSPParallelCtx[W Weight](ctx context.Context, c *csr.CSR[W], numWorkers int) (*APSP[W], error) {
	defer metrics.Time("search.JohnsonAPSPParallelCtx").Stop()
	p, err := johnsonPrepare[W](ctx, c)
	if err != nil {
		metrics.IncCounter("search.JohnsonAPSPParallelCtx.errors", 1)
		return nil, err
	}
	if p.out.live == 0 {
		return p.out, nil
	}

	// Materialise the live source ids so workers can stripe a dense
	// index range (compact[src] >= 0). The reweighted-Dijkstra cost is
	// per live source, so striping the live set keeps the workers
	// balanced even when the NodeID space is sparse.
	srcs := make([]int, 0, p.live)
	for src := 0; src < p.maxID; src++ {
		if p.compact[src] >= 0 {
			srcs = append(srcs, src)
		}
	}

	if numWorkers <= 0 {
		numWorkers = runtime.GOMAXPROCS(0)
	}
	if numWorkers > len(srcs) {
		numWorkers = len(srcs)
	}

	// Cancellation cascade: any worker that observes ctx.Err() (or an
	// inner Dijkstra error) calls cancel() on the shared cancellable
	// context, which propagates to every sibling via their per-source
	// ctx.Err() poll, so no worker keeps grinding its stripe after the
	// run has already failed.
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make([]error, numWorkers)
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			pprof.Do(workCtx, pprof.Labels("component", "johnson-apsp-parallel", "worker", fmt.Sprintf("%d", w)),
				func(wCtx context.Context) {
					// Each worker owns a private Dijkstra state acquired
					// from the per-W pool. The pool is safe for
					// concurrent acquisition; the state object itself is
					// never shared across goroutines, so the shared p
					// (read-only) and the disjoint output rows are the
					// only cross-goroutine memory — and they never alias.
					st := acquireDijkstra[W](uint64(p.maxID))
					defer releaseDijkstra(st)
					for i := w; i < len(srcs); i += numWorkers {
						if err := wCtx.Err(); err != nil {
							errs[w] = err
							cancel()
							return
						}
						if err := johnsonDijkstraSource[W](wCtx, c, p, srcs[i], st); err != nil {
							errs[w] = err
							cancel()
							return
						}
					}
				})
		}(w)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			metrics.IncCounter("search.JohnsonAPSPParallelCtx.errors", 1)
			return nil, e
		}
	}
	return p.out, nil
}
