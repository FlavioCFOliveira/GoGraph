package exec

// parallel_aggregate_scan.go — ParallelAggregateScan operator (#2111).
//
// ParallelAggregateScan is a morsel-parallel leaf that generalises
// [ParallelCountScan] (#1672) to the full family of aggregates that admit a
// deterministic, BYTE-IDENTICAL-to-serial parallel combine: count(*) / count(v),
// min, and max — global (group-by-less) and their GROUP BY forms. Every other
// aggregate (float sum/avg, int sum, collect, percentile, stdev) has no
// byte-identical combine and is structurally kept on the serial path: the
// planner never routes it here, and this operator carries no reducer kind that
// could accumulate it. The [github.com/FlavioCFOliveira/GoGraph/cypher/funcs].Aggregator
// interface remains free of any Combine method — the parallel merge lives here,
// as a closed, plan-selected kernel, exactly as the design mandates.
//
// # Why min/max are byte-identical to serial (position-carrying combine)
//
// Serial [funcs.MinAgg]/[funcs.MaxAgg] keep the FIRST-SEEN value among
// [expr.Compare]-ties (the update is strict </>), and that retained value is a
// COMPARED value: Compare(int 1, float 1.0) == 0 yet they stringify differently
// ("1" vs "1.0"); -0.0 and +0.0 tie yet stringify differently; distinct NaN
// payloads tie. A value-only parallel combine would therefore be a FALSE GREEN —
// it agrees with serial only until the data places such a tie at the extremum.
//
// Each per-worker partial for a min/max reducer carries (extremum value, global
// scan index of that value). Morsels are CONTIGUOUS slices of the deterministically
// collected node-ID list (the [nodeWalker].WalkNodeIDs order, stable under the
// graph's visibility barrier), and each morsel carries its base offset, so the
// global scan index of a row is base+localOffset — exactly the serial scan order.
// The per-worker update, and the cross-worker combine, both break a Compare-tie by
// the LOWEST global scan index. The result is the lowest-global-index Compare-
// extremal value: precisely the representative serial keeps (first-seen in scan
// order). It is a pure function of the set {(value, index)}, independent of worker
// count, morsel size, and scheduling — byte-identical to serial for ALL tie cases.
//
// # Group-by determinism
//
// Each worker builds a private group→partial map keyed by the EXACT serial
// grouping identity ([expr.HashRowEquivalent] + [rowsEqual], the same functions
// [EagerAggregation] uses), so an int/float bucket collision resolves into the
// same groups the serial path forms. The merge combines same-identity groups by
// the same comparator; the per-key combine is the associative count add / the
// position-carrying min/max semilattice above, so the (key, aggregated-value)
// multiset is partition-invariant. Each worker also tracks, per group, the
// minimum global scan index at which the group was seen (its first-seen position);
// the merged groups are emitted in ascending first-seen order, which reproduces
// the serial insertion order (group-creation order in scan order) byte-for-byte —
// so even a downstream positional read or a bare LIMIT observes the serial order.
//
// # Bounded resources
//
// Worker count is [ParallelGovernor.Enter]'s budget (≈ GOMAXPROCS / leaves in
// flight), so N concurrent parallel queries do not oversubscribe the cores. The
// work channel is pre-filled to the morsel count and closed before any worker
// starts, so no send blocks. For group-by, each worker's group count is capped at
// maxGroups and its retained group-key bytes at maxBytes (fail-fast with
// [ErrAggMemoryExceeded]); the merged map is capped identically. Because a worker's
// partials are a subset of the whole, neither per-worker cap can trip for an input
// the serial path accepts, so the caps bound peak memory without changing the
// result for any accepted input.
//
// # Inline-serial short-circuit (budget == 1)
//
// The worker count is [ParallelGovernor.Enter]'s budget, and under high
// concurrency the governor throttles every in-flight query toward budget 1. At
// budget 1 the multi-worker machinery — a goroutine, a work channel, a cancellable
// context, and a pprof.Do frame — would only ever run ONE worker over every
// morsel, so it is pure overhead and, under saturation, a measurable regression.
// When Enter returns 1 the operator therefore runs that single worker SYNCHRONOUSLY
// on the calling goroutine (no goroutine, no channel, no context.WithCancel, no
// pprof.Do): a lone worker dequeues every morsel, so iterating the morsel slice in
// order is identical to draining the pre-filled work channel, and the accumulated
// state — hence the result rows and the position-carrying tie representative — is
// byte-identical to the multi-worker path evaluated at one worker. combine() still
// folds the single state on the Next goroutine, and an inline error surfaces
// through Next exactly like a worker error, so nothing downstream can tell the two
// paths apart.
//
// # Lifecycle / cancellation / concurrency contract
//
// The join (wg.Wait) and the single-goroutine combine run on the goroutine that
// drives Next, which the engine drives inside the graph's visibility-barrier
// RLock ([lpg.Graph.View]); no worker goroutine outlives the barrier. The
// happens-before edge (worker return → wg.Done → wg.Wait) makes the read of each
// worker's state race-free with no extra synchronisation. Workers check ctx.Err
// between morsels and periodically inside the drain loop; Close cancels then joins
// any worker a never-called Next left running, so there is no goroutine leak.
//
// ParallelAggregateScan is NOT safe for concurrent use (the caller drives
// Init/Next/Close from a single goroutine).

import (
	"context"
	"fmt"
	"runtime/pprof"
	"sort"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// AggReducerKind identifies the deterministic, byte-identical-to-serial combine a
// [ParallelAggregateScan] reducer applies to one aggregate column. Only these four
// kinds have an admitted parallel combine; the planner emits no other kind.
type AggReducerKind uint8

const (
	// ReduceCountStar counts every row (count(*)). The combine is int64 addition.
	ReduceCountStar AggReducerKind = iota
	// ReduceCount counts non-NULL argument values (count(v)). The combine is int64
	// addition; the NULL-skip is a per-value predicate independent of partition.
	ReduceCount
	// ReduceMin keeps the minimum argument value under [expr.Compare], ties broken
	// by lowest global scan index (position-carrying). NULLs are skipped.
	ReduceMin
	// ReduceMax keeps the maximum argument value under [expr.Compare], ties broken
	// by lowest global scan index (position-carrying). NULLs are skipped.
	ReduceMax
)

// AggInputFactory builds an independent sub-plan that emits the PRE-AGGREGATION
// rows for exactly the node IDs in ids: one row per node in scan order, laid out
// as [groupKey0..groupKey{nKeys-1}, aggArg0..aggArg{nAggs-1}] — the same layout
// [EagerAggregation]'s pre-projection installs, evaluated by the same closures, so
// each worker's per-node key/argument values are byte-identical to serial. Each
// call must return a fresh operator sharing NO mutable state with any other call's
// (the planner rebuilds the pre-projection over a per-worker walker and a
// per-worker buildOpts copy). The ids slice is owned by the caller and is
// read-only for the operator's lifetime. The returned operator is driven
// Init → Next* → Close by exactly one worker goroutine.
type AggInputFactory func(ids []graph.NodeID) (Operator, error)

// reducerAcc is one reducer's partial accumulation for one group (or the single
// global group). count is used by the count reducers; val/pos carry the
// position-tagged extremum for the min/max reducers (val == nil ⇒ no non-NULL
// value seen yet).
type reducerAcc struct {
	val   expr.Value
	pos   int64
	count int64
}

// aggGroup is one group's partial state: the boxed grouping key (for collision
// resolution and output), one accumulator per reducer, and the minimum global scan
// index at which any row of the group was seen (first-seen position, used to
// reproduce the serial group-creation order at emit).
type aggGroup struct {
	keyVals  []expr.Value
	accs     []reducerAcc
	firstPos int64
}

// workerState is one worker's private partial: either the global accumulators
// (nKeys == 0) or the group→partial map (nKeys > 0). Read only after wg.Wait.
type workerState struct {
	global []reducerAcc
	table  map[uint64][]*aggGroup
	groups int // distinct group count in table (for the per-worker cap)
}

// ParallelAggregateScan is a Volcano leaf operator that partitions a full-node
// scan into contiguous morsels, accumulates per-worker count/min/max partials over
// each morsel's pre-aggregation rows, and combines them into the final aggregation
// result with a deterministic, byte-identical-to-serial combine.
//
// ParallelAggregateScan is NOT safe for concurrent use.
type ParallelAggregateScan struct {
	g           nodeWalker
	ctx         context.Context //nolint:containedctx // stored for per-Next ctx check
	initErr     error
	inlineErr   error // set by the budget==1 inline path; surfaced by Next like a worker error
	factory     AggInputFactory
	gov         *ParallelGovernor
	cancel      context.CancelFunc
	workErr     chan error
	estimateRow func(Row) int64

	reducers []AggReducerKind
	states   []*workerState // one per worker; read only after wg.Wait
	out      []Row          // combined result rows, streamed by Next

	wg         sync.WaitGroup
	nKeys      int
	morselSize int
	maxGroups  int
	maxBytes   int64
	pos        int // cursor into out

	entered   bool // gov.Enter ran → Close calls gov.Leave exactly once
	joined    bool // workers joined and result combined
	emptyIn   bool // Init found zero live nodes (no workers spawned)
	globalIn  bool // nKeys == 0 (a single global aggregate)
	ranInline bool // diagnostic seam: Init took the budget==1 inline-serial path
}

// NewParallelAggregateScan creates a ParallelAggregateScan over g. nKeys is the
// number of leading grouping-key columns in each pre-aggregation row (0 ⇒ a global
// aggregate); reducers gives the combine for each aggregate column (in order, at
// row columns nKeys+i). factory rebuilds the per-worker pre-aggregation sub-plan.
// morselSize controls the chunk size per worker (0 ⇒ [DefaultMorselSize]); gov is
// the engine-shared worker-budget governor (nil ⇒ unbounded GOMAXPROCS).
func NewParallelAggregateScan(g nodeWalker, factory AggInputFactory, nKeys int, reducers []AggReducerKind, morselSize int, gov *ParallelGovernor) *ParallelAggregateScan {
	if morselSize <= 0 {
		morselSize = DefaultMorselSize
	}
	return &ParallelAggregateScan{
		g:          g,
		factory:    factory,
		nKeys:      nKeys,
		reducers:   reducers,
		morselSize: morselSize,
		gov:        gov,
		maxGroups:  DefaultMaxGroups,
		globalIn:   nKeys == 0,
	}
}

// WithGroupCap sets the maximum distinct group count (per worker and on the merged
// map); 0 keeps [DefaultMaxGroups]. Returns op for chaining; call before Init.
func (op *ParallelAggregateScan) WithGroupCap(maxGroups int) *ParallelAggregateScan {
	if maxGroups > 0 {
		op.maxGroups = maxGroups
	}
	return op
}

// WithByteBudget bounds the estimated retained size of the grouping keys by
// maxBytes (per worker and on the merged map), the group-by analogue of the
// serial [EagerAggregation.WithByteBudget] (#1841). A non-positive maxBytes or a
// nil estimateRow leaves the byte dimension disabled. Returns op for chaining;
// call before Init.
func (op *ParallelAggregateScan) WithByteBudget(maxBytes int64, estimateRow func(Row) int64) *ParallelAggregateScan {
	op.maxBytes = maxBytes
	op.estimateRow = estimateRow
	return op
}

// Init collects all live node IDs on the calling goroutine (the ONLY phase that
// touches graph state), partitions them into contiguous morsels tagged with their
// base offset, and launches the workers. Each worker accumulates private
// count/min/max partials over the morsels it dequeues. The join and combine are
// deferred to the first Next so every worker is joined on the Next goroutine,
// inside the engine's visibility barrier.
func (op *ParallelAggregateScan) Init(ctx context.Context) error {
	op.ctx = ctx
	op.joined = false
	op.emptyIn = false
	op.ranInline = false
	op.inlineErr = nil
	op.pos = 0
	op.out = nil
	op.cancel = func() {}

	var nodeIDs []graph.NodeID
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
		op.initErr = fmt.Errorf("exec: ParallelAggregateScan init cancelled: %w", ctx.Err())
		return op.initErr
	}
	if len(nodeIDs) == 0 {
		// Nothing to scan: a global aggregate still emits one neutral row; a
		// group-by aggregate emits zero rows. Handled in Next; spawn no workers.
		op.emptyIn = true
		return nil
	}

	morsels := splitMorselsWithBase(nodeIDs, op.morselSize)

	nWorkers := op.gov.Enter(len(morsels))
	op.entered = true

	// Budget==1 inline-serial short-circuit (#2115): run the lone governed worker
	// synchronously on the calling goroutine — no goroutine, no work channel, no
	// context.WithCancel, no pprof.Do. One worker over every morsel is byte-identical
	// to the multi-worker path at nWorkers==1 (see the type doc). An inline error is
	// stashed and surfaced by Next like a worker error, so Init returns nil and Drain
	// wraps it identically ("operator next:"); Close still runs gov.Leave via
	// op.entered. combine() folds the single state on the Next goroutine, unchanged.
	if nWorkers == 1 {
		op.ranInline = true
		st, err := op.runMorselsInline(ctx, morsels)
		if err != nil {
			op.inlineErr = err
			return nil
		}
		op.states = []*workerState{st}
		return nil
	}

	workCh := make(chan aggMorsel, len(morsels))
	for _, m := range morsels {
		workCh <- m
	}
	close(workCh)

	wCtx, cancel := context.WithCancel(ctx)
	op.cancel = cancel
	op.states = make([]*workerState, nWorkers)
	op.workErr = make(chan error, nWorkers)
	op.wg.Add(nWorkers)

	for i := range nWorkers {
		go func() {
			defer op.wg.Done()
			pprof.Do(wCtx, pprof.Labels("component", "cypher-parallel-aggregate-scan", "worker", fmt.Sprintf("%d", i)), func(ctx context.Context) {
				st, err := op.runWorker(ctx, workCh)
				if err != nil {
					select {
					case op.workErr <- err:
					default:
					}
					return
				}
				op.states[i] = st
			})
		}()
	}
	return nil
}

// aggMorsel is a contiguous slice of node IDs together with its base offset in the
// full scan order, so a worker can tag each row with its global scan index.
type aggMorsel struct {
	ids  []graph.NodeID
	base int64
}

// splitMorselsWithBase partitions ids into contiguous morsels of at most size
// elements, each tagged with its base offset in ids (its first element's scan
// index). Because the morsels are contiguous slices in scan order, base+localOffset
// is exactly the serial scan index of every row.
func splitMorselsWithBase(ids []graph.NodeID, size int) []aggMorsel {
	n := (len(ids) + size - 1) / size
	morsels := make([]aggMorsel, 0, n)
	var base int64
	for len(ids) > 0 {
		end := size
		if end > len(ids) {
			end = len(ids)
		}
		morsels = append(morsels, aggMorsel{ids: ids[:end], base: base})
		base += int64(end)
		ids = ids[end:]
	}
	return morsels
}

// runWorker dequeues morsels, builds a fresh pre-aggregation sub-plan per morsel,
// drains it, and accumulates the worker's private count/min/max partials, tagging
// each row with its global scan index (morsel base + local offset). It returns the
// accumulated state or the first error (sub-plan build/exec, a memory cap, or ctx
// cancellation).
func (op *ParallelAggregateScan) runWorker(ctx context.Context, workCh <-chan aggMorsel) (*workerState, error) {
	st := op.newWorkerState()
	var byteB byteBudget
	byteB.set(op.maxBytes, op.estimateRow)

	for m := range workCh {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := op.runMorsel(ctx, st, &byteB, m); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// runMorselsInline runs the single governed worker synchronously over every morsel,
// in scan order, on the calling goroutine — the budget==1 short-circuit (#2115). It
// is the channel-free, goroutine-free twin of [runWorker]: because a lone worker
// dequeues every morsel, iterating the morsel slice is identical to draining the
// pre-filled work channel, so the accumulated state is byte-identical to the
// multi-worker path evaluated at one worker. It shares [runMorsel] and the same
// per-morsel ctx check, so cancellation and the memory caps behave identically.
func (op *ParallelAggregateScan) runMorselsInline(ctx context.Context, morsels []aggMorsel) (*workerState, error) {
	st := op.newWorkerState()
	var byteB byteBudget
	byteB.set(op.maxBytes, op.estimateRow)

	for _, m := range morsels {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := op.runMorsel(ctx, st, &byteB, m); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// newWorkerState allocates a fresh per-worker partial: the global accumulator slice
// for a group-by-less aggregate, or an empty group table for a GROUP BY aggregate.
func (op *ParallelAggregateScan) newWorkerState() *workerState {
	st := &workerState{}
	if op.globalIn {
		st.global = make([]reducerAcc, len(op.reducers))
	} else {
		st.table = make(map[uint64][]*aggGroup)
	}
	return st
}

// runMorsel builds and drains one pre-aggregation sub-plan over m.ids, folding each
// row into the worker state. The sub-plan is Closed before return (including on the
// error path). A bare full-node scan emits exactly one row per node in morsel order
// under the visibility barrier, so the j-th row's global scan index is m.base+j.
func (op *ParallelAggregateScan) runMorsel(ctx context.Context, st *workerState, byteB *byteBudget, m aggMorsel) error {
	sub, err := op.factory(m.ids)
	if err != nil {
		return fmt.Errorf("exec: ParallelAggregateScan subplan build: %w", err)
	}
	defer func() { _ = sub.Close() }()
	if err := sub.Init(ctx); err != nil {
		return err
	}

	var row Row
	var j int64
	for {
		if j&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		ok, err := sub.Next(&row)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		idx := m.base + j
		j++
		if op.globalIn {
			op.stepAccs(st.global, row, idx)
			continue
		}
		if err := op.stepGroup(st, byteB, row, idx); err != nil {
			return err
		}
	}
	return nil
}

// stepAccs folds one pre-aggregation row (at global scan index idx) into accs, one
// reducer at a time. The argument for reducer i is at row column nKeys+i.
func (op *ParallelAggregateScan) stepAccs(accs []reducerAcc, row Row, idx int64) {
	for i, kind := range op.reducers {
		col := op.nKeys + i
		v := expr.Value(expr.Null)
		if col < len(row) {
			v = row[col]
		}
		switch kind {
		case ReduceCountStar:
			accs[i].count++
		case ReduceCount:
			if !expr.IsNull(v) {
				accs[i].count++
			}
		case ReduceMin:
			if !expr.IsNull(v) && (accs[i].val == nil || betterMin(v, idx, accs[i].val, accs[i].pos)) {
				accs[i].val, accs[i].pos = v, idx
			}
		case ReduceMax:
			if !expr.IsNull(v) && (accs[i].val == nil || betterMax(v, idx, accs[i].val, accs[i].pos)) {
				accs[i].val, accs[i].pos = v, idx
			}
		}
	}
}

// stepGroup folds one pre-aggregation row (at global scan index idx) into the
// worker's group table, opening a new group — and boxing its key — only on first
// sight, exactly mirroring [EagerAggregation.getOrCreate]: the maxGroups check then
// the byte-budget charge, both before creation. The grouping identity is
// [expr.HashRowEquivalent] + [rowsEqual], the same the serial path uses.
func (op *ParallelAggregateScan) stepGroup(st *workerState, byteB *byteBudget, row Row, idx int64) error {
	keyVals := make([]expr.Value, op.nKeys)
	for i := 0; i < op.nKeys; i++ {
		if i < len(row) {
			keyVals[i] = row[i]
		} else {
			keyVals[i] = expr.Null
		}
	}
	h := expr.HashRowEquivalent(keyVals)
	bucket := st.table[h]
	for _, g := range bucket {
		if rowsEqual(g.keyVals, keyVals) {
			op.stepAccs(g.accs, row, idx)
			if idx < g.firstPos {
				g.firstPos = idx
			}
			return nil
		}
	}
	if st.groups >= op.maxGroups {
		return ErrAggMemoryExceeded
	}
	if byteB.charge(keyVals) {
		return ErrAggMemoryExceeded
	}
	g := &aggGroup{keyVals: keyVals, accs: make([]reducerAcc, len(op.reducers)), firstPos: idx}
	op.stepAccs(g.accs, row, idx)
	st.table[h] = append(bucket, g)
	st.groups++
	return nil
}

// betterMin reports whether value v at global scan index idx should replace the
// current minimum cur held at index curPos: a strictly smaller value, or an equal
// value (Compare == 0) seen at a lower scan index. The idx < curPos tie-break pins
// the retained representative to the lowest-scan-index Compare-minimal value —
// exactly what the serial first-seen [funcs.MinAgg] keeps.
func betterMin(v expr.Value, idx int64, cur expr.Value, curPos int64) bool {
	c := expr.Compare(v, cur)
	return c < 0 || (c == 0 && idx < curPos)
}

// betterMax is the symmetric rule for the maximum: a strictly greater value, or an
// equal value seen at a lower scan index.
func betterMax(v expr.Value, idx int64, cur expr.Value, curPos int64) bool {
	c := expr.Compare(v, cur)
	return c > 0 || (c == 0 && idx < curPos)
}

// combineAcc folds a source partial src into the destination dst for reducer kind.
// count adds; min/max apply the same position-carrying tie-break as the per-row
// step, so the combine is associative and commutative and its representative is the
// serial one.
func combineAcc(dst *reducerAcc, src reducerAcc, kind AggReducerKind) {
	switch kind {
	case ReduceCountStar, ReduceCount:
		dst.count += src.count
	case ReduceMin:
		if src.val != nil && (dst.val == nil || betterMin(src.val, src.pos, dst.val, dst.pos)) {
			dst.val, dst.pos = src.val, src.pos
		}
	case ReduceMax:
		if src.val != nil && (dst.val == nil || betterMax(src.val, src.pos, dst.val, dst.pos)) {
			dst.val, dst.pos = src.val, src.pos
		}
	}
}

// resultValue renders a reducer's final accumulator into the output value: an
// IntegerValue count, or the retained min/max value (NULL when no non-NULL value
// was seen) — identical to the serial [funcs.CountAgg]/[funcs.MinAgg]/[funcs.MaxAgg]
// Result.
func resultValue(acc reducerAcc, kind AggReducerKind) expr.Value {
	switch kind {
	case ReduceCountStar, ReduceCount:
		return expr.IntegerValue(acc.count)
	default: // ReduceMin / ReduceMax
		if acc.val == nil {
			return expr.Null
		}
		return acc.val
	}
}

// Next emits the aggregation result. The first call joins every worker (wg.Wait),
// surfaces the first worker error, combines the per-worker partials, and builds the
// output rows; subsequent calls stream them.
func (op *ParallelAggregateScan) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.initErr != nil {
		return false, op.initErr
	}

	if !op.joined {
		op.wg.Wait() // happens-before: every worker has written its state
		op.joined = true
		if op.inlineErr != nil { // budget==1 inline path failed; surface as a worker error
			return false, op.inlineErr
		}
		select {
		case err := <-op.workErr:
			return false, err
		default:
		}
		rows, err := op.combine()
		if err != nil {
			return false, err
		}
		op.out = rows
	}

	if op.pos >= len(op.out) {
		return false, nil
	}
	*out = op.out[op.pos]
	op.pos++
	return true, nil
}

// combine folds the per-worker partials into the final result rows. For a global
// aggregate it produces exactly one row (the combined accumulators, or the neutral
// row when the input was empty). For a group-by aggregate it merges same-identity
// groups under the exact serial grouping comparator and emits one row per group in
// ascending first-seen order — the serial group-creation order.
func (op *ParallelAggregateScan) combine() ([]Row, error) {
	if op.globalIn {
		accs := make([]reducerAcc, len(op.reducers))
		for _, st := range op.states {
			if st == nil {
				continue
			}
			for i := range op.reducers {
				combineAcc(&accs[i], st.global[i], op.reducers[i])
			}
		}
		row := make(Row, len(op.reducers))
		for i := range op.reducers {
			row[i] = resultValue(accs[i], op.reducers[i])
		}
		return []Row{row}, nil
	}

	if op.emptyIn {
		return nil, nil // group-by over empty input ⇒ zero groups ⇒ zero rows
	}

	merged := make(map[uint64][]*aggGroup)
	var order []*aggGroup
	var mergedGroups int
	var byteB byteBudget
	byteB.set(op.maxBytes, op.estimateRow)

	for _, st := range op.states {
		if st == nil {
			continue
		}
		for h, bucket := range st.table {
			for _, wg := range bucket {
				dst := findGroup(merged[h], wg.keyVals)
				if dst == nil {
					if mergedGroups >= op.maxGroups {
						return nil, ErrAggMemoryExceeded
					}
					if byteB.charge(wg.keyVals) {
						return nil, ErrAggMemoryExceeded
					}
					dst = &aggGroup{keyVals: wg.keyVals, accs: make([]reducerAcc, len(op.reducers)), firstPos: wg.firstPos}
					merged[h] = append(merged[h], dst)
					order = append(order, dst)
					mergedGroups++
				}
				for i := range op.reducers {
					combineAcc(&dst.accs[i], wg.accs[i], op.reducers[i])
				}
				if wg.firstPos < dst.firstPos {
					dst.firstPos = wg.firstPos
				}
			}
		}
	}

	// Emit in ascending first-seen scan index: the serial group-creation order.
	// firstPos values are distinct across groups (each group's first row is a
	// distinct node with a distinct scan index), so the sort is total and
	// deterministic run-to-run.
	sort.Slice(order, func(i, j int) bool { return order[i].firstPos < order[j].firstPos })

	rows := make([]Row, len(order))
	for gi, g := range order {
		row := make(Row, op.nKeys+len(op.reducers))
		copy(row, g.keyVals)
		for i := range op.reducers {
			row[op.nKeys+i] = resultValue(g.accs[i], op.reducers[i])
		}
		rows[gi] = row
	}
	return rows, nil
}

// findGroup returns the group in bucket whose key is equivalent to keyVals (the
// exact serial grouping comparator), or nil. It is the merge-phase counterpart of
// the per-worker collision search.
func findGroup(bucket []*aggGroup, keyVals []expr.Value) *aggGroup {
	for _, g := range bucket {
		if rowsEqual(g.keyVals, keyVals) {
			return g
		}
	}
	return nil
}

// Close cancels any still-running workers and joins them. It is idempotent and safe
// whether or not Next was ever called.
func (op *ParallelAggregateScan) Close() error {
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
