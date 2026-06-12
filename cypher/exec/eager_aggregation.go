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
	child        Operator
	keyCols      []int                     // column indices that form the group key
	aggFactories []funcs.AggregatorFactory // one factory per aggregate expression
	maxGroups    int                       // memory cap on distinct group count

	// Runtime state — valid between Init and Close.
	ctx     context.Context          //nolint:containedctx // stored for per-Next ctx check
	built   bool                     // true after the blocking consume phase
	groups  map[uint64][]*groupEntry // hash → bucket (collision chain)
	order   []*groupEntry            // insertion-order for deterministic output
	emitIdx int                      // cursor into order during emit phase
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

// Init initialises the operator. The blocking consume phase is deferred to the
// first Next call.
func (op *EagerAggregation) Init(ctx context.Context) error {
	op.ctx = ctx
	op.built = false
	op.groups = nil
	op.order = nil
	op.emitIdx = 0
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

// Close closes the child operator and releases internal state.
func (op *EagerAggregation) Close() error {
	op.groups = nil
	op.order = nil
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// consume — blocking phase
// ─────────────────────────────────────────────────────────────────────────────

// consume pulls every row from the child and populates the group table.
func (op *EagerAggregation) consume() error {
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

	aggs := make([]funcs.Aggregator, len(op.aggFactories))
	for i, factory := range op.aggFactories {
		aggs[i] = factory()
	}

	entry := &groupEntry{keyVals: keyVals, aggs: aggs}
	op.groups[h] = append(bucket, entry)
	op.order = append(op.order, entry)
	return entry, nil
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
