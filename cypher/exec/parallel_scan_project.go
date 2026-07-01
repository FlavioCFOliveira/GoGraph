package exec

// parallel_scan_project.go — ParallelScanProject operator (#1682).
//
// ParallelScanProject is a morsel-parallel leaf operator that fuses a full-node
// scan with its downstream Filter and (scalar) Projection and runs them on up to
// GOMAXPROCS worker goroutines. It is the general-scan counterpart of
// [ParallelCountScan] (#1672): where ParallelCountScan serves only a group-by-less
// count, ParallelScanProject serves `MATCH (n) [WHERE …] RETURN <scalar items>`
// by pushing the filter and projection INTO each worker.
//
// # Why no per-row channel
//
// An earlier attempt funnelled every scanned NodeID through a single
// `chan expr.Value` to a serial downstream Filter/Project consumer. The per-row
// channel serialised the whole pipeline and benchmarked as a ~2x regression. This
// operator removes that funnel entirely: each worker owns a PRIVATE sub-plan
// (morsel scan → Filter → Project) and accumulates its result rows in a private
// buffer. Only whole per-worker result slices cross a goroutine boundary, so the
// hot path is channel-free and lock-free per row.
//
// # Architecture
//
// Init collects every live NodeID once, on the calling goroutine (identical to
// AllNodesScan.Init / ParallelCountScan.Init — the ONLY phase that touches graph
// state), splits the owned slice into disjoint morsels of [DefaultMorselSize]
// IDs, and builds one INDEPENDENT sub-plan per worker via the supplied
// [SubplanFactory]. The factory is invoked on the calling goroutine, before any
// worker launches, so every build-time write (schema population, build-time
// buildOpts fields) stays single-threaded. Each worker then drives its own
// sub-plan over the morsels it dequeues from a pre-filled bounded work channel,
// deep-copying every result row into a private []Row buffer.
//
// The first call to Next joins every worker synchronously via wg.Wait on the
// caller's goroutine, then concatenates the per-worker buffers and streams them.
// The happens-before edge (worker return → wg.Done → wg.Wait) makes the read of
// each worker's buffer race-free with no additional synchronisation. Because the
// join and the streaming run on the goroutine that drives Next — which the engine
// drives inside the graph's visibility-barrier RLock ([lpg.Graph.View]) — no
// worker goroutine outlives the barrier.
//
// # Result multiset
//
// A full-node scan is unordered, so concatenating the per-worker result slices
// yields the same MULTISET as the serial AllNodesScan → Filter → Project
// pipeline. Filter's three-valued logic (NULL drops the row) is a per-value
// predicate independent of partition order, and the projection items are scalar
// (no aggregation, DISTINCT, ordering, or row reshaping — the planner refuses to
// fuse those shapes), so partition boundaries never change the multiset. A
// downstream Sort / Distinct / aggregation operator above this leaf still
// receives the full multiset and produces its ordered/deduplicated/aggregated
// result correctly.
//
// # Bounded resources
//
// At most min(GOMAXPROCS, morselCount) workers. The work channel is buffered to
// the morsel count and pre-filled, so no goroutine blocks sending work. No
// goroutine leak: every worker exits when the work channel drains, the sub-plan
// errors, or ctx is cancelled, and Close cancels then joins any worker that a
// never-called (or partially drained) Next left running.
//
// # Cancellation
//
// Workers check ctx.Err between morsels; the sub-plan operators check ctx.Err in
// their own Next loops. Next checks ctx.Err before joining and while streaming.
//
// # Concurrency contract
//
// ParallelScanProject is NOT safe for concurrent use (the caller drives
// Init/Next/Close from a single goroutine).

import (
	"context"
	"errors"
	"fmt"
	"runtime/pprof"
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// errBudgetReached is an INTERNAL sentinel a worker returns from runMorsel once
// the fleet-wide result budget (row count or byte estimate) is exceeded. It is
// never propagated to the caller: runWorker treats it as a clean stop, keeps
// the rows accumulated so far, and returns them with a nil error. The operator
// therefore emits a bounded prefix of the result set (~the budget plus a small
// per-worker overshoot), and the engine's Result-drain layer — which sums the
// same estimate — produces the canonical ErrResultRowsExceeded /
// ErrResultBytesExceeded from that bounded set, exactly as it does on the serial
// path. This keeps the cap error's definition in one place (package cypher)
// without exec importing it.
var errBudgetReached = errors.New("exec: parallel scan result budget reached")

// SubplanFactory builds an independent physical sub-plan that scans exactly the
// NodeIDs in morsel and applies the fused Filter/Projection over them. Each call
// must return a fresh operator tree that shares NO mutable state with any other
// call's tree (the planner achieves this with a per-worker schema map and a
// per-worker buildOpts copy whose lazily-written fields are zeroed). The morsel
// slice is owned by the caller (ParallelScanProject) and is read-only for the
// lifetime of the returned operator; the factory must not retain or mutate it
// beyond feeding it to the morsel scan leaf.
//
// The returned operator is driven Init → Next* → Close by exactly one worker
// goroutine. It must honour the standard [Operator] lifecycle.
type SubplanFactory func(morsel []graph.NodeID) (Operator, error)

// ParallelScanProject is a Volcano leaf operator that partitions a full-node
// scan into morsels, runs an independent fused scan→filter→project sub-plan per
// morsel on up to GOMAXPROCS workers, and emits the concatenated per-worker
// result rows. The output schema is the projection's output schema (set by the
// factory's Project), so this operator's rows are ready for the engine's final
// column passthrough.
//
// ParallelScanProject is NOT safe for concurrent use.
type ParallelScanProject struct {
	g          nodeWalker
	morselSize int
	factory    SubplanFactory
	gov        *ParallelGovernor // adaptive worker-budget governor (nil = unbounded)

	ctx     context.Context    //nolint:containedctx // stored for per-Next ctx check
	cancel  context.CancelFunc // cancels the worker context
	wg      sync.WaitGroup
	results [][]Row // one private result buffer per worker; read only after wg.Wait
	workErr chan error
	initErr error // error captured during Init (e.g. cancellation, factory build)
	entered bool  // true once gov.Enter ran, so Close calls gov.Leave exactly once

	joined bool  // true once the workers have been joined
	combo  []Row // concatenated worker results, streamed by Next
	pos    int   // cursor into combo

	// Result-memory budget (#1830). maxRows/maxBytes mirror the engine's
	// per-query MaxResultRows / MaxResultBytes (0 = unbounded). Without them the
	// morsel workers would materialise the ENTIRE projected result set before
	// Next hands the first row to the drain layer, so the drain's incremental
	// caps — which do protect the serial (lazy Volcano) path — could not bound
	// peak memory on the parallel path: an untrusted client could force a
	// multi-gigabyte resident allocation regardless of the 1 GiB default byte
	// budget. sharedRows/sharedBytes are the fleet-wide running totals the
	// workers check via overResultBudget so accumulation stops at ~the budget
	// (plus one in-flight batch per worker) instead of the whole set. estimateRow
	// is the engine's own coarse row-size estimate, injected so the worker's byte
	// accounting matches the drain's exactly.
	maxRows     int64
	maxBytes    int64
	estimateRow func(Row) int64
	sharedRows  atomic.Int64
	sharedBytes atomic.Int64
}

// WithResultBudget threads the engine's per-query result-memory budget into the
// operator so the morsel workers stop accumulating once the fleet-wide total
// exceeds maxRows or maxBytes, bounding peak memory on the parallel path (#1830).
// maxRows/maxBytes of 0 leave that dimension unbounded (the engine convention);
// estimateRow is the engine's coarse per-row byte estimate and may be nil to
// disable byte-budget enforcement. It returns op for chaining and must be called
// before Init. When neither bound is set the operator behaves exactly as before
// (full materialisation), so the result multiset is unchanged under budget.
func (op *ParallelScanProject) WithResultBudget(maxRows, maxBytes int64, estimateRow func(Row) int64) *ParallelScanProject {
	op.maxRows = maxRows
	op.maxBytes = maxBytes
	op.estimateRow = estimateRow
	return op
}

// overResultBudget records one emitted row against the fleet-wide running
// totals and reports whether the engine's result budget is now exceeded. It is
// called by every worker (concurrently) after appending a row, so the counters
// are atomic. When it returns true the worker stops accumulating and the
// operator emits only the bounded prefix gathered so far. It never drops a row
// while under budget, so the result multiset is unchanged when the budget is not
// exceeded.
func (op *ParallelScanProject) overResultBudget(row Row) bool {
	over := false
	if op.maxRows > 0 && op.sharedRows.Add(1) > op.maxRows {
		over = true
	}
	if op.maxBytes > 0 && op.estimateRow != nil {
		if op.sharedBytes.Add(op.estimateRow(row)) > op.maxBytes {
			over = true
		}
	}
	return over
}

// NewParallelScanProject creates a ParallelScanProject over g whose per-worker
// fused sub-plans are built by factory. morselSize controls the chunk size per
// worker; pass 0 to use [DefaultMorselSize].
func NewParallelScanProject(g nodeWalker, factory SubplanFactory, morselSize int, gov *ParallelGovernor) *ParallelScanProject {
	if morselSize <= 0 {
		morselSize = DefaultMorselSize
	}
	return &ParallelScanProject{g: g, morselSize: morselSize, factory: factory, gov: gov}
}

// Init collects all node IDs, partitions them into morsels, builds one
// independent sub-plan per worker on the calling goroutine, and launches the
// workers. Each worker drives its sub-plan over the morsels it dequeues and
// accumulates deep-copied result rows into its private buffer. The join and
// combine are deferred to the first Next call so every worker is joined on the
// Next goroutine, inside the engine's visibility barrier.
func (op *ParallelScanProject) Init(ctx context.Context) error {
	op.ctx = ctx
	op.joined = false
	op.pos = 0
	op.combo = nil
	op.cancel = func() {}
	op.sharedRows.Store(0)
	op.sharedBytes.Store(0)

	// Collect all NodeIDs on the calling goroutine (same pattern as
	// AllNodesScan.Init). This is the ONLY phase that touches graph state before
	// the workers run; each worker's morsel scan reads only its immutable
	// sub-slice of this owned slice.
	var nodeIDs []graph.NodeID
	// Pre-size the collection slice from the walker's live-node-count hint when
	// it exposes one (the *lpgNodeWalker does), removing the O(log N) geometric
	// re-grows of a potentially large slice. The hint is an upper bound
	// (tombstones make it an over-estimate at worst), so this never under-sizes.
	if h, ok := op.g.(interface{ LiveOrderHint() int }); ok {
		if n := h.LiveOrderHint(); n > 0 {
			nodeIDs = make([]graph.NodeID, 0, n)
		}
	}
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
		op.initErr = fmt.Errorf("exec: ParallelScanProject init cancelled: %w", ctx.Err())
		return op.initErr
	}
	if len(nodeIDs) == 0 {
		// Nothing to scan: zero result rows, no workers.
		return nil
	}

	morsels := splitMorsels(nodeIDs, op.morselSize)

	// Adaptive worker budget: divide the GOMAXPROCS pool by the number of
	// parallel leaves in flight across all concurrent queries, so N concurrent
	// scans don't each spawn GOMAXPROCS workers (#1705). Register here (after we
	// know there is work) and deregister in Close, guarded by op.entered.
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
	op.results = make([][]Row, nWorkers)
	op.workErr = make(chan error, nWorkers)
	op.wg.Add(nWorkers)

	for i := range nWorkers {
		go func() {
			defer op.wg.Done()
			pprof.Do(wCtx, pprof.Labels("component", "cypher-parallel-scan-project", "worker", fmt.Sprintf("%d", i)), func(ctx context.Context) {
				rows, err := op.runWorker(ctx, workCh)
				if err != nil {
					select {
					case op.workErr <- err:
					default:
					}
					return
				}
				op.results[i] = rows
			})
		}()
	}
	return nil
}

// runWorker dequeues morsels and, for each, builds a fresh fused sub-plan via the
// factory, drains it to completion, and deep-copies every result row into the
// worker's private buffer. It returns the accumulated rows or the first error
// (sub-plan build/exec error or ctx cancellation). The morsel sub-slices it reads
// are immutable; the buffer it returns is owned solely by this worker until the
// caller joins via wg.Wait.
func (op *ParallelScanProject) runWorker(ctx context.Context, workCh <-chan []graph.NodeID) ([]Row, error) {
	var out []Row
	for morsel := range workCh {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := op.runMorsel(ctx, morsel)
		if err != nil {
			if errors.Is(err, errBudgetReached) {
				// Fleet-wide result budget exceeded: keep the rows this morsel
				// produced and stop dequeuing further work. The emitted prefix
				// already exceeds the budget, so the drain layer trips the
				// canonical cap error; peak memory is bounded to what is gathered
				// here rather than the whole result set.
				out = append(out, rows...)
				return out, nil
			}
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// runMorsel builds and drains one fused sub-plan over morsel, returning the
// deep-copied result rows. The sub-plan is Closed before return (including on the
// error path) so a worker never leaks an operator's resources.
func (op *ParallelScanProject) runMorsel(ctx context.Context, morsel []graph.NodeID) ([]Row, error) {
	sub, err := op.factory(morsel)
	if err != nil {
		return nil, fmt.Errorf("exec: ParallelScanProject subplan build: %w", err)
	}
	defer func() { _ = sub.Close() }()

	if err := sub.Init(ctx); err != nil {
		return nil, err
	}
	// Per-worker row arena: instead of allocating a fresh backing slice per
	// result row (the old append(Row(nil), row...)), pack every row's values into
	// a pre-sized flat slab and hand out three-index sub-slices into it. A morsel
	// scan→filter→project yields at most one row per morsel node, so one slab of
	// len(morsel)*width holds the whole morsel in a single allocation. The
	// overflow guard re-chunks if a future fused shape ever exceeds that bound;
	// older sub-slices stay valid because they alias the prior, still-referenced
	// slab. The slab is owned solely by this worker until wg.Wait, so it needs no
	// synchronisation. Row headers are independent (Project reuses one outBuf
	// across Next calls); the element Values are escaping-safe (fully-materialised,
	// never a reused lazy node), so copying the values into the slab and sharing
	// them is sufficient and correct.
	rows := make([]Row, 0, len(morsel))
	var slab []expr.Value
	var row Row
	for {
		ok, err := sub.Next(&row)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		w := len(row)
		if w == 0 {
			rows = append(rows, Row{})
			if op.overResultBudget(nil) {
				return rows, errBudgetReached
			}
			continue
		}
		if cap(slab)-len(slab) < w {
			n := len(morsel) * w
			if n < w {
				n = w
			}
			slab = make([]expr.Value, 0, n)
		}
		start := len(slab)
		slab = append(slab, row...)
		rows = append(rows, slab[start:start+w:start+w])
		if op.overResultBudget(row) {
			return rows, errBudgetReached
		}
	}
	return rows, nil
}

// Next streams the concatenated per-worker result rows. The first call joins
// every worker synchronously (wg.Wait) on the calling goroutine, surfaces the
// first worker error if any, then concatenates the per-worker buffers.
// Subsequent calls advance the cursor through the concatenation.
func (op *ParallelScanProject) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.initErr != nil {
		return false, op.initErr
	}

	if !op.joined {
		op.wg.Wait() // happens-before: every worker has returned and written its buffer
		op.joined = true
		select {
		case err := <-op.workErr:
			return false, err
		default:
		}
		// Concatenate per-worker buffers in worker index order. The order is
		// irrelevant to correctness (a full scan is unordered), but a fixed order
		// keeps the stream deterministic for a given partition.
		if len(op.results) == 1 {
			// Single worker: stream its buffer directly, eliding the concat copy.
			op.combo = op.results[0]
		} else {
			total := 0
			for _, r := range op.results {
				total += len(r)
			}
			op.combo = make([]Row, 0, total)
			for _, r := range op.results {
				op.combo = append(op.combo, r...)
			}
		}
	}

	if op.pos >= len(op.combo) {
		return false, nil
	}
	*out = op.combo[op.pos]
	op.pos++
	return true, nil
}

// Close cancels any still-running workers and joins them. It is idempotent and
// safe whether or not Next was ever called: wg.Wait returns immediately once the
// workers have drained, and cancel unblocks a worker stalled on ctx or inside a
// sub-plan's Next.
func (op *ParallelScanProject) Close() error {
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
