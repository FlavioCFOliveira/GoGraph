package exec

// filter.go — Filter (Selection) operator with three-valued logic (task-242).
//
// Filter wraps a child Operator and applies a predicate to every row produced
// by the child.  Only rows for which the predicate returns BoolValue(true) are
// forwarded to the caller.  Per openCypher 9 §4.1.3, NULL and BoolValue(false)
// both suppress the row (three-valued logic: NULL drops the row too).
//
// # Concurrency
//
// Filter is NOT safe for concurrent use.

import (
	"context"

	"gograph/cypher/expr"
)

// FilterFn is a predicate over a Row. It must return (BoolValue(true), nil) to
// accept the row. Any other non-error return value (including NULL and
// BoolValue(false)) causes the row to be dropped. An error halts the pipeline.
type FilterFn func(row Row) (expr.Value, error)

// Filter is a Volcano pipeline operator that applies a [FilterFn] predicate to
// each row produced by its child operator. It emits only rows for which the
// predicate returns [expr.BoolValue](true).
//
// Null and false both suppress the row (three-valued logic).
//
// Filter is NOT safe for concurrent use.
type Filter struct {
	child  Operator
	predFn FilterFn
	ctx    context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewFilter creates a Filter operator that wraps child and applies predFn to
// every row.
func NewFilter(child Operator, predFn FilterFn) *Filter {
	return &Filter{child: child, predFn: predFn}
}

// Init initialises the operator and its child.
func (op *Filter) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next advances to the next row that passes the predicate. It pulls rows from
// the child until one satisfies the predicate, end-of-stream, or an error.
func (op *Filter) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		ok, err := op.child.Next(out)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}

		result, err := op.predFn(*out)
		if err != nil {
			return false, err
		}

		// Only BoolValue(true) passes; NULL and false both drop the row.
		if expr.IsTruthy(result) {
			return true, nil
		}
	}
}

// Close releases resources and closes the child operator.
func (op *Filter) Close() error {
	return op.child.Close()
}
