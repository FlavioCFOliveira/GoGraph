package exec

// global_aggregate_adapter.go — empty-input adapter for global aggregation.
//
// openCypher requires that a global (group-by-less) aggregation over zero input
// rows emit one output row populated with the neutral element of each aggregate
// function. EagerAggregation, as a multiset operator, emits zero rows in that
// case because no groups are formed. GlobalAggregateAdapter wraps an
// EagerAggregation that has no group-by keys and synthesises that single
// fallback row when the child is empty.
//
// When the wrapped EagerAggregation produces any rows, the adapter is a
// transparent pass-through.
//
// # Concurrency
//
// GlobalAggregateAdapter is NOT safe for concurrent use, matching the
// EagerAggregation contract.

import (
	"context"

	"gograph/cypher/funcs"
)

// GlobalAggregateAdapter wraps an EagerAggregation operator that has no
// group-by keys and ensures the output stream contains exactly one row even
// when the child is empty.
//
// GlobalAggregateAdapter is NOT safe for concurrent use.
type GlobalAggregateAdapter struct {
	child        Operator
	aggFactories []funcs.AggregatorFactory

	ctx       context.Context //nolint:containedctx // stored for per-Next ctx check
	gotRow    bool            // true after the child produced at least one row
	exhausted bool            // true after the child reported end-of-stream
	emitted   bool            // true after a synthetic fallback row has been emitted
}

// NewGlobalAggregateAdapter returns a GlobalAggregateAdapter that wraps child.
// aggFactories must contain one factory per aggregate column, in the same
// order as the columns produced by child. The factories are invoked exactly
// once, on empty input, to synthesise the neutral row.
//
// Note: the adapter does not validate that child is a group-by-less
// EagerAggregation; callers are responsible for that. Wrapping a grouped
// aggregation has no effect because grouped aggregations always emit either
// zero (no input) or N rows (one per group), and the empty-input case for a
// grouped aggregation is correct as is.
func NewGlobalAggregateAdapter(child Operator, aggFactories []funcs.AggregatorFactory) *GlobalAggregateAdapter {
	cp := make([]funcs.AggregatorFactory, len(aggFactories))
	copy(cp, aggFactories)
	return &GlobalAggregateAdapter{child: child, aggFactories: cp}
}

// Init initialises the adapter and its child.
func (op *GlobalAggregateAdapter) Init(ctx context.Context) error {
	op.ctx = ctx
	op.gotRow = false
	op.exhausted = false
	op.emitted = false
	return op.child.Init(ctx)
}

// Next forwards child rows unchanged. When the child reports end-of-stream
// without ever producing a row, Next emits exactly one synthetic row built
// from the neutral results of each aggregate factory before signalling
// exhaustion itself.
func (op *GlobalAggregateAdapter) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	if !op.exhausted {
		ok, err := op.child.Next(out)
		if err != nil {
			return false, err
		}
		if ok {
			op.gotRow = true
			return true, nil
		}
		op.exhausted = true
	}

	// Child is exhausted. Emit the synthetic neutral row exactly once when no
	// child rows were observed.
	if op.gotRow || op.emitted {
		return false, nil
	}
	op.emitted = true

	row := make(Row, len(op.aggFactories))
	for i, factory := range op.aggFactories {
		agg := factory()
		row[i] = agg.Result()
	}
	*out = row
	return true, nil
}

// Close releases the adapter's child.
func (op *GlobalAggregateAdapter) Close() error {
	return op.child.Close()
}
