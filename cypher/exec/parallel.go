package exec

// parallel.go — ParallelScan operator with morsel-based parallelism (task-249).
//
// ParallelScan wraps a nodeWalker, splits the node ID range into fixed-size
// morsels (chunks of MorselSize IDs each), and runs up to GOMAXPROCS worker
// goroutines concurrently. Each worker owns a private AllNodesScan replica over
// its morsel, and emits rows into a shared bounded channel.
//
// # Architecture
//
// The splitter goroutine collects all NodeIDs once during Init, slices them
// into morsels of MorselSize, and sends each morsel to a work channel. Worker
// goroutines dequeue morsels, iterate the IDs, and send rows to the output
// channel. Next() reads from the output channel.
//
// # Bounded resources
//
// The work channel and output channel are both bounded. The output channel
// capacity is GOMAXPROCS*2; the work channel capacity equals the number of
// morsels. No goroutine leaks: every goroutine exits when ctx is cancelled
// or the work channel is drained.
//
// # Cancellation
//
// Every send and receive checks ctx.Done. Workers propagate the first error
// (including context cancellation) via a dedicated error channel.
//
// # Race safety
//
// Each worker owns its own local slice; there is no shared mutable state
// between workers beyond the bounded channels.
//
// # Concurrency contract
//
// ParallelScan is NOT safe for concurrent use from multiple goroutines
// (the caller drives Init/Next/Close from a single goroutine).

import (
	"context"
	"fmt"
	"runtime"
	"runtime/pprof"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// DefaultMorselSize is the number of NodeIDs processed per worker goroutine
// per scheduling quantum. Sized to fill roughly one or two cache lines of
// work before touching a channel.
const DefaultMorselSize = 1024

// ParallelScan is a Volcano leaf operator that partitions the full node scan
// into fixed-size morsels and executes them concurrently on up to GOMAXPROCS
// worker goroutines.
//
// ParallelScan is NOT safe for concurrent use.
type ParallelScan struct {
	g          nodeWalker
	morselSize int

	ctx      context.Context    //nolint:containedctx // stored for per-Next ctx check
	outCh    chan expr.Value    // carries IntegerValue NodeIDs
	errCh    chan error         // first worker error, capacity 1
	cancel   context.CancelFunc // cancels the worker context
	wg       sync.WaitGroup     // joins the worker goroutines
	closerWG sync.WaitGroup     // joins the closer goroutine (#1795)
	initErr  error              // error set during Init if any
}

// NewParallelScan creates a ParallelScan over g. morselSize controls the
// chunk size per worker; pass 0 to use [DefaultMorselSize].
func NewParallelScan(g nodeWalker, morselSize int) *ParallelScan {
	if morselSize <= 0 {
		morselSize = DefaultMorselSize
	}
	return &ParallelScan{
		g:          g,
		morselSize: morselSize,
	}
}

// Init collects all node IDs, partitions them into morsels, and launches
// worker goroutines. The output channel is populated lazily by the workers.
func (op *ParallelScan) Init(ctx context.Context) error {
	op.ctx = ctx

	// Collect all NodeIDs (same pattern as AllNodesScan.Init).
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
		return fmt.Errorf("exec: ParallelScan init cancelled: %w", ctx.Err())
	}
	if len(nodeIDs) == 0 {
		// Nothing to scan; set up a closed channel so Next returns immediately.
		ch := make(chan expr.Value)
		close(ch)
		op.outCh = ch
		op.errCh = make(chan error, 1)
		op.cancel = func() {}
		return nil
	}

	// Partition node IDs into morsels.
	morsels := splitMorsels(nodeIDs, op.morselSize)

	nWorkers := runtime.GOMAXPROCS(0)
	if nWorkers > len(morsels) {
		nWorkers = len(morsels)
	}

	// Bounded channels sized to avoid deadlock while limiting buffering.
	workCh := make(chan []graph.NodeID, len(morsels))
	outCh := make(chan expr.Value, nWorkers*2)
	errCh := make(chan error, 1)

	// Fill work channel without blocking (cap = len(morsels)).
	for _, m := range morsels {
		workCh <- m
	}
	close(workCh)

	// Derive a cancellable context for workers.
	wCtx, cancel := context.WithCancel(ctx)

	op.outCh = outCh
	op.errCh = errCh
	op.cancel = cancel
	op.wg.Add(nWorkers)

	for i := range nWorkers {
		i := i
		go func() {
			defer op.wg.Done()
			pprof.Do(wCtx, pprof.Labels("component", "cypher-parallel-scan", "worker", fmt.Sprintf("%d", i)), func(ctx context.Context) {
				op.workerLoop(ctx, workCh, outCh, errCh)
			})
		}()
	}

	// Closer goroutine: when all workers finish, close the output channel so
	// Next() gets the end-of-stream signal. Tracked in its own WaitGroup so
	// Close() joins it too — otherwise a caller that abandons the operator
	// without draining outCh leaves the closer blocked on op.wg.Wait() until the
	// workers exit, and Close() returns before the closer has actually finished
	// (#1795).
	op.closerWG.Add(1)
	go pprof.Do(wCtx, pprof.Labels("component", "cypher-parallel-scan-closer"), func(_ context.Context) {
		defer op.closerWG.Done()
		op.wg.Wait()
		close(outCh)
	})

	return nil
}

// workerLoop dequeues morsels and sends NodeIDs to outCh until the work
// channel is empty or ctx is cancelled.
func (op *ParallelScan) workerLoop(ctx context.Context, workCh <-chan []graph.NodeID, outCh chan<- expr.Value, errCh chan<- error) {
	for morsel := range workCh {
		for _, id := range morsel {
			if err := ctx.Err(); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			select {
			case outCh <- expr.IntegerValue(int64(id)):
			case <-ctx.Done():
				select {
				case errCh <- ctx.Err():
				default:
				}
				return
			}
		}
	}
}

// Next reads the next NodeID from the output channel.
func (op *ParallelScan) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.initErr != nil {
		return false, op.initErr
	}

	select {
	case v, ok := <-op.outCh:
		if !ok {
			// Channel closed: check for a worker error.
			select {
			case err := <-op.errCh:
				return false, err
			default:
				return false, nil
			}
		}
		// Reuse a one-element backing array via local stack allocation.
		row := Row{v}
		*out = row
		return true, nil
	case <-op.ctx.Done():
		return false, op.ctx.Err()
	}
}

// Close cancels workers and waits for them — and the closer goroutine — to
// exit. Joining closerWG (not just wg) guarantees no goroutine outlives Close,
// even when the caller never drained outCh (#1795).
func (op *ParallelScan) Close() error {
	if op.cancel != nil {
		op.cancel()
	}
	op.wg.Wait()
	op.closerWG.Wait()
	return nil
}

// splitMorsels partitions ids into chunks of at most size elements.
func splitMorsels(ids []graph.NodeID, size int) [][]graph.NodeID {
	n := (len(ids) + size - 1) / size
	morsels := make([][]graph.NodeID, 0, n)
	for len(ids) > 0 {
		end := size
		if end > len(ids) {
			end = len(ids)
		}
		morsels = append(morsels, ids[:end])
		ids = ids[end:]
	}
	return morsels
}
