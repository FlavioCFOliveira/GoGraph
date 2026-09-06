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

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
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

	// removed accumulates the rows this filter pulled from its child and then
	// dropped, over its whole lifetime including every Init it has been restarted
	// by (rmp #2764). It is bumped ONLY on the reject branch, which the operator
	// has already decided to take and on which it does nothing else, so the
	// accepted path — the one every ordinary query runs — gains neither an
	// increment nor a branch. That is PostgreSQL's own placement of the same
	// counter (execScan.c:255, REL_17_STABLE); see rows_removed.go.
	//
	// It is deliberately NOT reset by Init: a Filter driven once per outer row
	// under an Apply reports its WHOLE lifetime, the convention Expand.slotsRead
	// and VarLengthExpand's traversal counter already follow.
	removed int64
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
		op.removed++
	}
}

// rowsRemovedByFilter reports the rows this operator pulled from its child and
// discarded because the predicate did not return true. It implements the
// rowsRemovedCounter marker in rows_removed.go, so PROFILE renders this
// operator's cell as a figure rather than omitting it (rmp #2764).
//
// The figure is EXACT by construction rather than by inference: every row the
// child yields leaves the loop through exactly one of two exits — a `return true`
// that the profiling wrapper counts as a Row, or the increment above — so
// `rowsRemovedByFilter() + Rows == rows pulled from the child`, with no third
// outcome. A row lost to a predicate ERROR is neither: the pipeline halts and the
// PROFILE is never rendered.
//
// It counts rows the PREDICATE rejected, and nothing else. NULL and false are both
// rejections here (openCypher 9 §4.1.3 three-valued logic), which is the same
// decision the operator already takes to drop the row.
//
// [ColumnarFilter] inherits this method through its embedded Filter and shares the
// same counter, so the columnar and boxed paths of one operator report into one
// figure — as they must, since a ColumnarFilter driven row-at-a-time by a
// non-columnar parent runs exactly the loop above.
func (op *Filter) rowsRemovedByFilter() int64 { return op.removed }

// Close releases resources and closes the child operator.
func (op *Filter) Close() error {
	return op.child.Close()
}
