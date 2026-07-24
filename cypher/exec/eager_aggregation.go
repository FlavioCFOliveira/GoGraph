package exec

// eager_aggregation.go — EagerAggregation operator (pipeline breaker).
//
// EagerAggregation consumes all rows from its child, groups them by a set of
// key column indices, applies one or more aggregator functions per group, and
// then emits the results.
//
// # Memory cap
//
// The number of distinct groups is bounded by maxGroups (default 1 000 000).
// Exceeding the cap returns [ErrAggMemoryExceeded]. This bounds the group COUNT
// only; the size of any one group's buffering aggregator (collect / percentile)
// is bounded separately by the per-aggregator element budget enforced in
// [github.com/FlavioCFOliveira/GoGraph/cypher/funcs] — a grouping-key-free
// aggregate forms exactly one group, so the group-count cap never fires for it.
// A buffering aggregator that exceeds its budget surfaces the typed error from
// its Step call, which consume propagates so it reaches [Result.Err].
//
// # Output schema
//
// Each output row contains the group-key values (in the order given by
// KeyCols) followed by the aggregation results (in the order given by
// AggFactories).
//
//	output[i] = key column values... | aggregated values...
//
// # Concurrency
//
// EagerAggregation is NOT safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// DefaultMaxGroups is the default upper bound on distinct groups that
// EagerAggregation will hold in memory.
const DefaultMaxGroups = 1_000_000

// ErrAggMemoryExceeded is returned by EagerAggregation.Next when the number of
// distinct groups exceeds the configured maxGroups limit.
var ErrAggMemoryExceeded = errors.New("exec: aggregation memory cap exceeded")

// ─────────────────────────────────────────────────────────────────────────────
// groupKey — hashable composite key for a group
// ─────────────────────────────────────────────────────────────────────────────

// groupEntry holds the aggregators for a single group alongside the actual key
// values needed for collision resolution and output.
type groupEntry struct {
	keyVals []expr.Value       // snapshot of key column values for this group
	aggs    []funcs.Aggregator // one aggregator per AggFactory slot
}

// ─────────────────────────────────────────────────────────────────────────────
// EagerAggregation
// ─────────────────────────────────────────────────────────────────────────────

// EagerAggregation is a blocking (pipeline-breaking) Volcano operator that
// groups rows from its child by the specified key columns and applies per-group
// aggregators. It emits one output row per group once the child is exhausted.
//
// EagerAggregation is NOT safe for concurrent use.
type EagerAggregation struct {
	child Operator

	// chunkChild, when non-nil (set by WithChunkInput), makes the blocking consume
	// phase pull the child column-major via [ChunkProducer.FillChunk] and hash the
	// scalar grouping keys UNBOXED, boxing a key only when it opens a new group
	// (#2049), AND accumulate each aggregate argument column UNBOXED into group-id-
	// indexed SoA state via a per-slot [aggKernel] (#2104) — removing the O(input)
	// per-argument [Chunk.BoxCell] the row path required. It is the columnar-input
	// counterpart of the row-at-a-time consume; the two are equivalent by
	// construction because the unboxed key hash and equivalence delegate to the SAME
	// [expr.EquivalentHash]/[expr.Equivalent] the boxed path uses, and each kernel
	// either mirrors its [funcs.Aggregator]'s accumulation exactly on the unboxed
	// column or delegates the cells it cannot take unboxed to the same boxed
	// [funcs.Aggregator.Step]. nil keeps the row-input path, byte-identical.
	chunkChild ChunkProducer

	// Runtime state — valid between Init and Close.
	ctx     context.Context          //nolint:containedctx // stored for per-Next ctx check
	groups  map[uint64][]*groupEntry // hash → bucket (collision chain) — row-input path
	scratch *Chunk                   // reused source-batch buffer for the chunk-input path

	// Chunk-input SoA group state (built by consumeChunk, read by emitChunk).
	// chunkKeyVals[gid] is the boxed group key (for output and collision resolution);
	// chunkBuckets maps a group hash to the dense group ids sharing that bucket; kernels
	// holds one columnar accumulator per aggregate slot (the #2104 SoA de-box). These
	// stay nil on the row-input path.
	chunkBuckets map[uint64][]int
	chunkKeyVals [][]expr.Value
	kernels      []aggKernel

	keyCols      []int                     // column indices that form the group key
	aggFactories []funcs.AggregatorFactory // one factory per aggregate expression
	budget       byteBudget                // estimated-byte cap on retained group keys (#1841)
	order        []*groupEntry             // insertion-order for deterministic output — row path

	maxGroups int  // memory cap on distinct group count
	emitIdx   int  // cursor into order/chunkKeyVals during emit phase
	built     bool // true after the blocking consume phase
}

// NewEagerAggregation creates an EagerAggregation operator.
//
//   - child: the upstream operator to consume.
//   - keyCols: column indices whose values define the group key. An empty slice
//     computes a single global aggregate.
//   - aggFactories: one AggregatorFactory per aggregate expression. Must not be
//     empty.
//   - maxGroups: upper bound on distinct groups; pass 0 to use DefaultMaxGroups.
func NewEagerAggregation(
	child Operator,
	keyCols []int,
	aggFactories []funcs.AggregatorFactory,
	maxGroups int,
) (*EagerAggregation, error) {
	if len(aggFactories) == 0 {
		return nil, fmt.Errorf("exec: EagerAggregation requires at least one AggregatorFactory")
	}
	if maxGroups <= 0 {
		maxGroups = DefaultMaxGroups
	}
	return &EagerAggregation{
		child:        child,
		keyCols:      keyCols,
		aggFactories: aggFactories,
		maxGroups:    maxGroups,
	}, nil
}

// WithByteBudget bounds the estimated retained size of the group keys by
// maxBytes. It complements the maxGroups count cap so a few large-valued group
// keys cannot exceed the engine's result-byte budget before the count cap fires
// (#1841). A non-positive maxBytes or nil estimateRow leaves the byte dimension
// disabled. Returns op for chaining and must be called before Init.
func (op *EagerAggregation) WithByteBudget(maxBytes int64, estimateRow func(Row) int64) *EagerAggregation {
	op.budget.set(maxBytes, estimateRow)
	return op
}

// WithChunkInput switches the consume phase to pull the child column-major so the
// scalar grouping keys are hashed and compared UNBOXED, boxing a key only on
// new-group creation (#2049). The child must be a [ChunkProducer]; the grouping
// keys must occupy chunk columns 0..len(keyCols)-1 and the aggregate arguments the
// columns after them (the layout the aggregation pre-projection installs), which is
// the same layout the row-input consume assumes. It returns an error otherwise,
// leaving the operator on the byte-identical row-input path. Call before Init.
//
// The path is reversible and self-checking per batch: a grouping-key column that is
// not an unboxed scalar backing (a promoted/boxed or non-scalar column) is read via
// [Chunk.BoxCell] and hashed/compared through the same boxed equivalence as the
// row path, so a heterogeneous or non-scalar key stays byte-identical.
func (op *EagerAggregation) WithChunkInput() error {
	cp, ok := op.child.(ChunkProducer)
	if !ok {
		return fmt.Errorf("exec: EagerAggregation.WithChunkInput: child %T is not a ChunkProducer", op.child)
	}
	op.chunkChild = cp
	return nil
}

// Init initialises the operator. The blocking consume phase is deferred to the
// first Next call.
func (op *EagerAggregation) Init(ctx context.Context) error {
	op.ctx = ctx
	op.built = false
	op.groups = nil
	op.order = nil
	op.chunkBuckets = nil
	op.chunkKeyVals = nil
	op.kernels = nil
	op.emitIdx = 0
	op.scratch = nil
	op.budget.reset()
	return op.child.Init(ctx)
}

// Next emits the next aggregated row. On the first call it consumes all rows
// from the child (pipeline breaker) and builds the group table. Subsequent
// calls iterate through the completed groups.
func (op *EagerAggregation) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	if !op.built {
		if err := op.consume(); err != nil {
			return false, err
		}
		op.built = true
	}

	// The chunk-input path emits from the SoA kernels; the row path from op.order.
	if op.chunkChild != nil {
		return op.emitChunk(out)
	}

	if op.emitIdx >= len(op.order) {
		return false, nil
	}

	entry := op.order[op.emitIdx]
	op.emitIdx++

	// Build output row: key values | aggregated values.
	width := len(entry.keyVals) + len(entry.aggs)
	row := make(Row, width)
	copy(row, entry.keyVals)
	for i, agg := range entry.aggs {
		row[len(entry.keyVals)+i] = agg.Result()
	}
	*out = row
	return true, nil
}

// emitChunk emits one output row per group from the chunk-input SoA state: the
// boxed group key followed by each kernel's per-group result. Groups are emitted
// in creation order (the dense group id), matching the row path's insertion-order
// output. It is the chunk-input counterpart of the op.order emit in [Next].
func (op *EagerAggregation) emitChunk(out *Row) (bool, error) {
	if op.emitIdx >= len(op.chunkKeyVals) {
		return false, nil
	}
	gid := op.emitIdx
	op.emitIdx++

	keyVals := op.chunkKeyVals[gid]
	row := make(Row, len(keyVals)+len(op.kernels))
	copy(row, keyVals)
	for i, k := range op.kernels {
		row[len(keyVals)+i] = k.result(gid)
	}
	*out = row
	return true, nil
}

// Close closes the child operator and releases internal state.
func (op *EagerAggregation) Close() error {
	op.groups = nil
	op.order = nil
	op.chunkBuckets = nil
	op.chunkKeyVals = nil
	op.kernels = nil
	op.scratch = nil
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// consume — blocking phase
// ─────────────────────────────────────────────────────────────────────────────

// consume pulls every row from the child and populates the group table.
func (op *EagerAggregation) consume() error {
	if op.chunkChild != nil {
		return op.consumeChunk()
	}
	op.groups = make(map[uint64][]*groupEntry)
	op.order = make([]*groupEntry, 0, 64)

	var row Row
	iter := 0
	for {
		if iter%4096 == 0 {
			if err := op.ctx.Err(); err != nil {
				return err
			}
		}
		iter++

		ok, err := op.child.Next(&row)
		if err != nil {
			return err
		}
		if !ok {
			break
		}

		entry, err := op.getOrCreate(row)
		if err != nil {
			return err
		}

		// Feed each aggregate expression.
		// Convention: aggregate inputs start at column len(keyCols) in the input
		// row (i.e. the first len(keyCols) columns are the group keys and the
		// remaining columns supply the values to aggregate). When the input row
		// is narrower than expected, Null is supplied.
		for i, agg := range entry.aggs {
			col := len(op.keyCols) + i
			v := expr.Value(expr.Null)
			if col < len(row) {
				v = row[col]
			}
			if err := agg.Step(v); err != nil {
				return err
			}
		}
	}
	// openCypher 9 §3.6: a pure aggregation (no grouping keys) over an
	// empty input emits exactly one row carrying the empty-state values
	// of every aggregator (count → 0, sum → 0, collect → [], min/max
	// → null, avg → null). Synthesise the singleton group with its
	// default-initialised aggregators so the downstream projection has
	// something to render. When grouping keys are present this branch
	// is intentionally skipped: an empty input correctly yields zero
	// groups.
	if len(op.order) == 0 && len(op.keyCols) == 0 {
		aggs := make([]funcs.Aggregator, len(op.aggFactories))
		for i, factory := range op.aggFactories {
			aggs[i] = factory()
		}
		entry := &groupEntry{keyVals: nil, aggs: aggs}
		op.order = append(op.order, entry)
	}
	return nil
}

// getOrCreate looks up the group for the current row (by key columns), creates
// a new entry if absent, and returns the entry. Returns ErrAggMemoryExceeded
// if the group cap is reached.
func (op *EagerAggregation) getOrCreate(row Row) (*groupEntry, error) {
	// Extract key columns.
	keyVals := make([]expr.Value, len(op.keyCols))
	for i, col := range op.keyCols {
		if col < len(row) {
			keyVals[i] = row[col]
		} else {
			keyVals[i] = expr.Null
		}
	}

	h := expr.HashRowEquivalent(keyVals)
	bucket := op.groups[h]

	// Linear search within the bucket for equivalence.
	for _, e := range bucket {
		if rowsEqual(e.keyVals, keyVals) {
			return e, nil
		}
	}

	// New group.
	if len(op.order) >= op.maxGroups {
		return nil, ErrAggMemoryExceeded
	}
	if op.budget.charge(keyVals) {
		return nil, ErrAggMemoryExceeded
	}

	aggs := make([]funcs.Aggregator, len(op.aggFactories))
	for i, factory := range op.aggFactories {
		aggs[i] = factory()
	}

	entry := &groupEntry{keyVals: keyVals, aggs: aggs}
	op.groups[h] = append(bucket, entry)
	op.order = append(op.order, entry)
	return entry, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// consumeChunk — columnar-input blocking phase (#2049)
// ─────────────────────────────────────────────────────────────────────────────

// nullEquivHash is the equivalence-consistent hash of a NULL key cell, computed
// once. The unboxed key hash uses it for a NULL cell so it matches exactly what
// [expr.HashRowEquivalent] over a boxed keyVals slice (in which a NULL cell is
// [expr.Null]) would fold in, keeping the columnar and boxed group hashes equal.
var nullEquivHash = expr.EquivalentHash(expr.Null)

// consumeChunk is the columnar-input counterpart of [EagerAggregation.consume]: it
// pulls the child column-major in batches via [ChunkProducer.FillChunk], hashes the
// scalar grouping keys UNBOXED (#2049), and accumulates each aggregate ARGUMENT column
// UNBOXED into group-id-indexed SoA state through a per-slot [aggKernel] (#2104) —
// removing the O(input) per-argument [Chunk.BoxCell] the row path required.
//
// Each batch is processed in two phases so the accumulation is a tight per-column
// scatter loop (MonetDB/X100 hash-aggregation): phase 1 assigns every source row its
// dense group id (opening a new group — and boxing its key — only on first sight, via
// [EagerAggregation.groupIDChunk]); phase 2 scatter-accumulates each argument column
// into its kernel keyed by those group ids. Within a group the cells are visited in
// input-row order across the whole run, matching the row path exactly, so the
// order-sensitive float64 sum/avg results are bit-identical.
//
// It is equivalent to the row-input path by construction: the unboxed key hash and
// equivalence delegate to the same [expr.EquivalentHash]/[expr.Equivalent] the boxed
// path uses (see [hashCellEquivalent]/[cellEqualToStored]), and each kernel either
// mirrors its [funcs.Aggregator]'s accumulation on the unboxed column or delegates the
// cells it cannot take unboxed to the same boxed [funcs.Aggregator.Step].
func (op *EagerAggregation) consumeChunk() error {
	op.chunkBuckets = make(map[uint64][]int)
	op.chunkKeyVals = make([][]expr.Value, 0, 64)
	op.kernels = buildAggKernels(op.aggFactories)
	if op.scratch == nil {
		op.scratch = op.chunkChild.NewOutputChunk(DefaultChunkCapacity)
	}

	var gids []int // per-batch dense group ids, reused across batches
	iter := 0
	for {
		if err := op.ctx.Err(); err != nil {
			return err
		}
		op.scratch.Reset()
		n, ferr := op.chunkChild.FillChunk(op.scratch, DefaultChunkCapacity)
		if n > 0 {
			if cap(gids) < n {
				gids = make([]int, n)
			} else {
				gids = gids[:n]
			}
			// Phase 1: assign group ids (opens new groups, boxing keys once each).
			for row := 0; row < n; row++ {
				if iter%4096 == 0 {
					if err := op.ctx.Err(); err != nil {
						return err
					}
				}
				iter++

				gid, err := op.groupIDChunk(op.scratch, row)
				if err != nil {
					return err
				}
				gids[row] = gid
			}
			// Grow every kernel to the current group count, then scatter-accumulate
			// each argument column. The argument for aggregate slot i occupies chunk
			// column len(keyCols)+i (the layout the pre-projection installs).
			ngroups := len(op.chunkKeyVals)
			for _, k := range op.kernels {
				k.grow(ngroups)
			}
			for slot, k := range op.kernels {
				if err := k.stepColumn(op.scratch, len(op.keyCols)+slot, gids); err != nil {
					return err
				}
			}
		}
		if ferr != nil {
			return ferr
		}
		if n < DefaultChunkCapacity {
			break // child exhausted (short fill), mirroring materializeColumnar
		}
	}
	// A group-key-free aggregate over an empty input must still emit one neutral row
	// (openCypher 9 §3.6). The chunk path is wired only for grouped aggregation
	// (len(keyCols) > 0), so this synthesises nothing in practice; it is kept for
	// parity should the path ever be enabled group-key-free.
	if len(op.chunkKeyVals) == 0 && len(op.keyCols) == 0 {
		for _, k := range op.kernels {
			k.grow(1)
		}
		op.chunkKeyVals = append(op.chunkKeyVals, nil)
	}
	return nil
}

// groupIDChunk hashes and compares the grouping key of source row row UNBOXED and
// returns its dense group id, opening a new group — and boxing its key values (via
// [Chunk.BoxCell]) only then, never per row — when the key is first seen. It is the
// dense-id counterpart of [EagerAggregation.getOrCreate]; the maxGroups and
// group-key byte-budget checks fire in the same order as the row path.
func (op *EagerAggregation) groupIDChunk(src *Chunk, row int) (int, error) {
	h := op.hashKeyColsChunk(src, row)
	bucket := op.chunkBuckets[h]
	for _, gid := range bucket {
		if op.keyColsEqualChunk(src, row, op.chunkKeyVals[gid]) {
			return gid, nil
		}
	}

	// New group: now — and only now — box the key values for retention and output.
	if len(op.chunkKeyVals) >= op.maxGroups {
		return 0, ErrAggMemoryExceeded
	}
	keyVals := make([]expr.Value, len(op.keyCols))
	for i, col := range op.keyCols {
		if col < src.NumCols() {
			keyVals[i] = src.BoxCell(col, row)
		} else {
			keyVals[i] = expr.Null
		}
	}
	if op.budget.charge(keyVals) {
		return 0, ErrAggMemoryExceeded
	}

	gid := len(op.chunkKeyVals)
	op.chunkKeyVals = append(op.chunkKeyVals, keyVals)
	op.chunkBuckets[h] = append(bucket, gid)
	return gid, nil
}

// hashKeyColsChunk computes the equivalence-consistent group hash of source row row
// from the grouping-key columns read UNBOXED. It folds each cell's
// [hashCellEquivalent] with the exact FNV constants and step [expr.HashRowEquivalent]
// uses, so it equals HashRowEquivalent over the boxed key row (a key column out of
// range folds the NULL hash, matching the row path's keyVals[i] == expr.Null).
func (op *EagerAggregation) hashKeyColsChunk(src *Chunk, row int) uint64 {
	const (
		offset uint64 = 14695981039346656037
		prime  uint64 = 1099511628211
	)
	h := offset
	ncols := src.NumCols()
	for _, col := range op.keyCols {
		var ch uint64
		if col < ncols {
			ch = hashCellEquivalent(src, col, row)
		} else {
			ch = nullEquivHash
		}
		h = h*prime ^ ch
	}
	return h
}

// keyColsEqualChunk reports whether the grouping key of source row row is equivalent
// (openCypher grouping/DISTINCT, CIP2016-06-14) to the boxed key stored in a candidate
// group, reading the source cells UNBOXED. It is the columnar counterpart of
// [rowsEqual]: each column is compared through [cellEqualToStored], which delegates to
// [expr.Equivalent], so the result matches the boxed comparison byte-for-byte.
func (op *EagerAggregation) keyColsEqualChunk(src *Chunk, row int, stored []expr.Value) bool {
	ncols := src.NumCols()
	for i, col := range op.keyCols {
		s := expr.Null
		if i < len(stored) {
			s = stored[i]
		}
		if col < ncols {
			if !cellEqualToStored(src, col, row, s) {
				return false
			}
		} else if !expr.IsNull(s) { // out-of-range key column reads as NULL
			return false
		}
	}
	return true
}

// hashCellEquivalent returns [expr.EquivalentHash] of cell (col, row) of src,
// reading a scalar backing UNBOXED and boxing only for a promoted/boxed column. The
// inline scalar box (e.g. expr.IntegerValue(v)) does not escape [expr.EquivalentHash]
// (which only reads it), so no heap allocation occurs on the scalar fast paths; the
// boxed-column fallback uses [Chunk.BoxCell] and is byte-identical to the row path.
// Routing through expr.EquivalentHash — rather than reimplementing the float64-domain
// fold — is deliberate: it is the single source of truth the boxed path also uses, so
// the two cannot drift (the hazard recorded in cypher/expr/equiv.go for #1865).
func hashCellEquivalent(src *Chunk, col, row int) uint64 {
	switch {
	case src.IsInt64Column(col):
		if v, ok := src.Int64(col, row); ok {
			return expr.EquivalentHash(expr.IntegerValue(v))
		}
		return nullEquivHash
	case src.IsFloat64Column(col):
		if v, ok := src.Float64(col, row); ok {
			return expr.EquivalentHash(expr.FloatValue(v))
		}
		return nullEquivHash
	case src.IsStringColumn(col):
		if v, ok := src.String(col, row); ok {
			return expr.EquivalentHash(expr.StringValue(v))
		}
		return nullEquivHash
	case src.IsBoolColumn(col):
		if v, ok := src.Bool(col, row); ok {
			return expr.EquivalentHash(expr.BoolValue(v))
		}
		return nullEquivHash
	default: // boxed or dynamic column, or a non-scalar kind
		return expr.EquivalentHash(src.BoxCell(col, row))
	}
}

// cellEqualToStored reports whether cell (col, row) of src is equivalent to the
// boxed value stored (openCypher grouping equivalence), reading a scalar backing
// UNBOXED. It delegates to [expr.Equivalent] with the current cell as the first
// operand and the stored group key as the second, exactly as the row path's
// [rowsEqual] compares expr.Equivalent(current, stored) — so an integer/float
// cross-type key (1 ≡ 1.0), a NaN ≡ NaN key, a -0.0 ≡ +0.0 key, and two distinct
// integers ≥ 2^53 that share a float64 bit-pattern (equivalent-FALSE: separate
// groups, same hash bucket) all resolve identically to the boxed path. The inline
// scalar box does not escape expr.Equivalent, so the scalar fast paths do not
// allocate; the boxed-column fallback uses [Chunk.BoxCell].
func cellEqualToStored(src *Chunk, col, row int, stored expr.Value) bool {
	switch {
	case src.IsInt64Column(col):
		if v, ok := src.Int64(col, row); ok {
			return expr.Equivalent(expr.IntegerValue(v), stored)
		}
		return expr.IsNull(stored)
	case src.IsFloat64Column(col):
		if v, ok := src.Float64(col, row); ok {
			return expr.Equivalent(expr.FloatValue(v), stored)
		}
		return expr.IsNull(stored)
	case src.IsStringColumn(col):
		if v, ok := src.String(col, row); ok {
			return expr.Equivalent(expr.StringValue(v), stored)
		}
		return expr.IsNull(stored)
	case src.IsBoolColumn(col):
		if v, ok := src.Bool(col, row); ok {
			return expr.Equivalent(expr.BoolValue(v), stored)
		}
		return expr.IsNull(stored)
	default: // boxed or dynamic column, or a non-scalar kind
		return expr.Equivalent(src.BoxCell(col, row), stored)
	}
}

// rowsEqual returns true iff a and b have the same length and each element pair
// is equivalent per openCypher grouping/DISTINCT semantics (CIP2016-06-14):
// null ≡ null, NaN ≡ NaN, and these rules apply recursively inside lists and
// maps. Used by both Distinct and EagerAggregation for collision resolution.
func rowsEqual(a, b []expr.Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valuesEqualForGrouping(a[i], b[i]) {
			return false
		}
	}
	return true
}

// valuesEqualForGrouping compares two values for group-key purposes using
// openCypher equivalence semantics (CIP2016-06-14): null ≡ null, NaN ≡ NaN,
// and these rules apply recursively inside lists and maps.
// Unlike predicate equality (Equal / IsTruthy), this is always two-valued.
func valuesEqualForGrouping(a, b expr.Value) bool {
	return expr.Equivalent(a, b)
}
