package exec

// parallel_aggregate.go — ParallelCountScan operator (#1672).
//
// ParallelCountScan is a morsel-parallel leaf operator specialised for the
// single shape that a parallel reduce can serve while staying BIT-IDENTICAL to
// the serial AllNodesScan + EagerAggregation pipeline: a group-by-less
// count(*) / count(<scan-var>) over a bare full-node scan.
//
// # Why count only
//
// The reduce combines per-worker partials with int64 addition. For count this
// is exact under any partition: int64 addition is associative and commutative
// (even on two's-complement wraparound), and count's NULL-skip is a per-value
// predicate independent of partition order, so the combined total equals the
// serial single-counter result regardless of how the node range is split. The
// other aggregates do not share this property and are deliberately NOT served
// here:
//
//   - sum over floats and avg are non-associative under IEEE-754 addition, so a
//     partitioned summation can differ from the serial left-fold in the last
//     ULP(s);
//   - integer sum carries an order-sensitive overflow guard whose firing depends
//     on the intermediate prefix, hence on the partition boundaries;
//   - collect / percentile / stdev are buffering or order-dependent.
//
// The planner routes every non-count shape to the serial path. The
// [github.com/FlavioCFOliveira/GoGraph/cypher/funcs].Aggregator interface
// exposes only Init/Step/Result (no Combine), which structurally prevents a
// future float aggregate from reaching this int64 combine by accident.
//
// # Architecture
//
// Init collects every live NodeID once, on the calling goroutine (identical to
// AllNodesScan.Init / ParallelScan.Init), splits the owned slice into disjoint
// morsels of [DefaultMorselSize] IDs, and launches up to GOMAXPROCS worker
// goroutines. Each worker owns a PRIVATE int64 counter and counts the IDs in
// the morsels it dequeues; workers never touch shared mutable state and never
// touch the graph after the single-threaded collection. There is no per-row
// output channel — that funnel is exactly the scaling ceiling this operator
// removes.
//
// The first call to Next joins every worker synchronously via wg.Wait on the
// caller's goroutine, then sums the per-worker partials into the single result
// row. The happens-before edge (worker return → wg.Done → wg.Wait) makes the
// combine race-free with no additional synchronisation. Because the join and
// combine run on the goroutine that drives Next — which the engine drives
// inside the graph's visibility-barrier RLock (lpg.Graph.View) — no worker
// goroutine outlives the barrier.
//
// # Bounded resources
//
// At most GOMAXPROCS workers. The work channel is buffered to the morsel count
// and pre-filled, so no goroutine blocks sending work. No goroutine leak: every
// worker exits when the work channel drains or ctx is cancelled, and Close
// cancels then joins any worker that a never-called Next left running.
//
// # Cancellation
//
// Workers check ctx.Err between morsels; Next checks ctx.Err before joining.
//
// # Concurrency contract
//
// ParallelCountScan is NOT safe for concurrent use (the caller drives
// Init/Next/Close from a single goroutine).

import (
	"context"
	"fmt"
	"runtime/pprof"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ParallelCountScan is a Volcano leaf operator that computes a group-by-less
// count over a full node scan using per-worker partial counters combined once
// at the end. It emits exactly one row with a single [expr.IntegerValue]
// column carrying the total live-node count.
//
// ParallelCountScan is NOT safe for concurrent use.
type ParallelCountScan struct {
	g          nodeWalker
	morselSize int
	gov        *ParallelGovernor // adaptive worker-budget governor (nil = unbounded)

	ctx      context.Context    //nolint:containedctx // stored for per-Next ctx check
	cancel   context.CancelFunc // cancels the worker context
	wg       sync.WaitGroup
	partials []int64 // one private counter per worker; read only after wg.Wait
	initErr  error   // error captured during Init (e.g. cancellation)
	entered  bool    // true once gov.Enter ran, so Close calls gov.Leave exactly once

	joined bool // true once the workers have been joined and the total combined
	total  int64
	done   bool // true after the single result row has been emitted
}

// NewParallelCountScan creates a ParallelCountScan over g. morselSize controls
// the chunk size per worker; pass 0 to use [DefaultMorselSize]. gov is the
// engine-shared adaptive worker-budget governor (nil = unbounded GOMAXPROCS).
func NewParallelCountScan(g nodeWalker, morselSize int, gov *ParallelGovernor) *ParallelCountScan {
	if morselSize <= 0 {
		morselSize = DefaultMorselSize
	}
	return &ParallelCountScan{g: g, morselSize: morselSize, gov: gov}
}

// Init collects all node IDs, partitions them into morsels, and launches worker
// goroutines that each accumulate a private count. The combine is deferred to
// the first Next call so every worker is joined on the Next goroutine, inside
// the engine's visibility barrier.
func (op *ParallelCountScan) Init(ctx context.Context) error {
	op.ctx = ctx
	op.joined = false
	op.done = false
	op.total = 0
	op.cancel = func() {}

	// Collect all NodeIDs on the calling goroutine (same pattern as
	// AllNodesScan.Init). This is the ONLY phase that touches graph state.
	var nodeIDs []graph.NodeID
	var cancelled bool
	var count int
	op.g.WalkNodeIDs(func(id graph.NodeID) bool {
		if count%4096 == 0 {
			if ctx.Err() != nil {
				cancelled = true
				return false
			}
		}
		nodeIDs = append(nodeIDs, id)
		count++
		return true
	})
	if cancelled {
		op.initErr = fmt.Errorf("exec: ParallelCountScan init cancelled: %w", ctx.Err())
		return op.initErr
	}
	if len(nodeIDs) == 0 {
		// Nothing to scan: the single row carries a count of zero. No workers.
		return nil
	}

	morsels := splitMorsels(nodeIDs, op.morselSize)

	// Adaptive worker budget shared across concurrent queries (#1705): divide
	// the GOMAXPROCS pool by the number of parallel leaves in flight so N
	// concurrent scans don't each spawn GOMAXPROCS workers. Register here (work
	// exists) and deregister in Close, guarded by op.entered.
	nWorkers := op.gov.Enter(len(morsels))
	op.entered = true

	// Bounded work channel pre-filled with every morsel (cap == morsel count),
	// so no send blocks and the channel is closed before any worker starts.
	workCh := make(chan []graph.NodeID, len(morsels))
	for _, m := range morsels {
		workCh <- m
	}
	close(workCh)

	wCtx, cancel := context.WithCancel(ctx)
	op.cancel = cancel
	op.partials = make([]int64, nWorkers)
	op.wg.Add(nWorkers)

	for i := range nWorkers {
		go func() {
			defer op.wg.Done()
			pprof.Do(wCtx, pprof.Labels("component", "cypher-parallel-count-scan", "worker", fmt.Sprintf("%d", i)), func(ctx context.Context) {
				op.partials[i] = countWorker(ctx, workCh)
			})
		}()
	}
	return nil
}

// countWorker dequeues morsels and returns the count of NodeIDs processed until
// the work channel drains or ctx is cancelled. It owns only its private return
// value and the immutable morsel sub-slices it reads; it touches no shared
// mutable state and no graph state.
func countWorker(ctx context.Context, workCh <-chan []graph.NodeID) int64 {
	var n int64
	for morsel := range workCh {
		if ctx.Err() != nil {
			return n
		}
		// Every NodeID in a bare full-node scan binds a non-null node, so both
		// count(*) and count(<scan-var>) increment once per ID — the per-morsel
		// length is the exact contribution.
		n += int64(len(morsel))
	}
	return n
}

// Next emits the single aggregated row on its first call. It joins every worker
// synchronously (wg.Wait) on the calling goroutine, then sums the per-worker
// partials. Subsequent calls report end-of-stream.
func (op *ParallelCountScan) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.initErr != nil {
		return false, op.initErr
	}
	if op.done {
		return false, nil
	}

	if !op.joined {
		op.wg.Wait() // happens-before: every worker has returned and written its partial
		op.joined = true
		var total int64
		for _, p := range op.partials {
			total += p // int64 addition: associative, partition-invariant for count
		}
		op.total = total
	}

	op.done = true
	*out = Row{expr.IntegerValue(op.total)}
	return true, nil
}

// Close cancels any still-running workers and joins them. It is idempotent and
// safe whether or not Next was ever called: wg.Wait returns immediately once the
// workers have drained, and cancel unblocks a worker stalled on ctx.
func (op *ParallelCountScan) Close() error {
	if op.cancel != nil {
		op.cancel()
	}
	op.wg.Wait()
	if op.entered {
		op.gov.Leave()
		op.entered = false
	}
	return nil
}
