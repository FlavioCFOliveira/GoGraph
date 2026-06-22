package search

import (
	"context"
	"runtime"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// floydParallelMinDim is the smallest live-node matrix dimension for
// which [FloydWarshallParallelCtx] fans a pivot's destination rows out
// to the worker pool. Below it the per-pivot goroutine hand-off costs
// more than the V^2/worker relaxation it saves, so the call transparently
// runs the serial pivot loop. The whole DP on such a matrix is already
// sub-millisecond.
const floydParallelMinDim = 128

// FloydWarshallParallel computes APSP via the same O(V^3) dynamic
// program as [FloydWarshall], with each pivot's destination-row sweep
// distributed across a bounded worker pool. The V sequential pivots are
// preserved (pivot k+1 depends on pivot k); only the inner i-loop of a
// fixed pivot — embarrassingly parallel once column k and row k are
// snapshotted — is parallelised.
//
// The result is bit-identical to the serial [FloydWarshall] for every
// Weight type, independent of numWorkers and of how the rows are
// scheduled. Both paths drive the identical [floydPivotRows] kernel over
// a per-pivot snapshot of column k and row k (the canonical CLRS
// recurrence d_k[i][j] = min(d_{k-1}[i][j], d_{k-1}[i][k] +
// d_{k-1}[k][j])); a worker reads only the two read-only snapshot
// vectors plus the disjoint rows it owns, so no relaxation depends on
// another worker's writes within a pivot. The negative-cycle contract is
// identical: the simple entry returns nil and the Ctx variant returns
// [ErrNegativeCycle]; the NaN/+-Inf gate and the integer-overflow
// precondition documented on [FloydWarshall] apply unchanged.
//
// numWorkers <= 0 picks runtime.GOMAXPROCS(0). With numWorkers == 1, or
// on a matrix smaller than [floydParallelMinDim], the call runs the
// serial pivot loop. The DP is memory-bandwidth bound, so the speedup
// plateaus near the host's physical core count.
//
// Concurrency: FloydWarshallParallel reads the immutable CSR without
// synchronisation and allocates its own working buffers per call, so it
// is safe to invoke concurrently on a shared CSR.
func FloydWarshallParallel[W Weight](c *csr.CSR[W], numWorkers int) *APSP[W] {
	defer metrics.Time("search.FloydWarshallParallel")()
	out, err := FloydWarshallParallelCtx(context.Background(), c, numWorkers)
	if err != nil {
		metrics.IncCounter("search.FloydWarshallParallel.errors", 1)
		return nil
	}
	return out
}

// FloydWarshallParallelCtx is the context-aware variant of
// [FloydWarshallParallel]. ctx.Err() is checked once at every k-pivot
// boundary (matching the serial granularity); on cancellation returns
// (nil, wrapped ctx.Err()).
//
// Errors surfaced are identical to [FloydWarshallCtx]: [ErrInvalidInput]
// (NaN/Inf float weight), [ErrNegativeCycle] (negative-weight cycle), or
// the underlying ctx.Err() on cancellation.
func FloydWarshallParallelCtx[W Weight](ctx context.Context, c *csr.CSR[W], numWorkers int) (*APSP[W], error) {
	defer metrics.Time("search.FloydWarshallParallelCtx")()
	if anyFloatInvalid(c.WeightsSlice()) {
		metrics.IncCounter("search.FloydWarshallParallelCtx.errors", 1)
		return nil, ErrInvalidInput
	}
	maxID := int(c.MaxNodeID())
	mask := c.LiveMask()
	compact := make([]int, maxID)
	live := 0
	for i := 0; i < maxID; i++ {
		if mask[i] {
			compact[i] = live
			live++
		} else {
			compact[i] = -1
		}
	}
	out := floydInit[W](c, maxID, compact, live)
	if live == 0 {
		return out, nil
	}

	if numWorkers <= 0 {
		numWorkers = runtime.GOMAXPROCS(0)
	}
	if numWorkers > live {
		numWorkers = live
	}

	var err error
	if numWorkers <= 1 || live < floydParallelMinDim {
		// Not worth the per-pivot hand-off: run the identical serial
		// pivot loop (same kernel ⇒ same result).
		err = floydRunDP[W](ctx, out, live)
	} else {
		err = floydRunDPParallel[W](ctx, out, live, numWorkers)
	}
	if err != nil {
		metrics.IncCounter("search.FloydWarshallParallelCtx.errors", 1)
		return nil, err
	}
	if floydHasNegativeCycle(out, live) {
		metrics.IncCounter("search.FloydWarshallParallelCtx.errors", 1)
		return nil, ErrNegativeCycle
	}
	return out, nil
}

// floydRunDPParallel runs the O(V^3) pivot loop with each pivot's
// destination-row range fanned out across numWorkers goroutines. The
// coordinator snapshots column k and row k once per pivot (establishing
// happens-before with the spawned workers), then each worker relaxes a
// contiguous, disjoint slice of [0,live) through [floydPivotRows]; the
// barrier (WaitGroup) at the end of every pivot guarantees pivot k's
// writes are visible before pivot k+1's snapshot reads them.
//
// numWorkers is assumed > 1 and <= live (the caller gates both).
func floydRunDPParallel[W Weight](ctx context.Context, out *APSP[W], live, numWorkers int) error {
	colDist := make([]W, live)
	colFound := make([]bool, live)
	rowDist := make([]W, live)
	rowFound := make([]bool, live)
	for k := 0; k < live; k++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		floydSnapshotPivot[W](out, live, k, colDist, colFound, rowDist, rowFound)

		var wg sync.WaitGroup
		base := live / numWorkers
		rem := live % numWorkers
		lo := 0
		// Spawn workers 1..numWorkers-1; the coordinator runs chunk 0
		// inline so one fewer goroutine is created per pivot.
		var chunk0Lo, chunk0Hi int
		for w := 0; w < numWorkers; w++ {
			size := base
			if w < rem {
				size++
			}
			hi := lo + size
			if w == 0 {
				chunk0Lo, chunk0Hi = lo, hi
			} else {
				wg.Add(1)
				go func(lo, hi int) {
					defer wg.Done()
					floydPivotRows[W](out.dist, out.found, live, lo, hi, colDist, colFound, rowDist, rowFound)
				}(lo, hi)
			}
			lo = hi
		}
		floydPivotRows[W](out.dist, out.found, live, chunk0Lo, chunk0Hi, colDist, colFound, rowDist, rowFound)
		wg.Wait()
	}
	return nil
}
