package exec

// unwind.go — Unwind operator for the UNWIND clause (task-375).
//
// Unwind evaluates a list-valued expression once per input row and emits one
// output row per list element, binding each element to a new column. When the
// expression evaluates to NULL or an empty list no rows are emitted for that
// input row (per openCypher semantics).
//
// # Concurrency
//
// Unwind is NOT safe for concurrent use.

import (
	"context"
	"errors"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ErrUnwindNilChild is returned by [NewUnwind] when child is nil.
var ErrUnwindNilChild = errors.New("exec: NewUnwind requires non-nil child Operator")

// ErrUnwindNilListFn is returned by [NewUnwind] when listFn is nil.
var ErrUnwindNilListFn = errors.New("exec: NewUnwind requires non-nil listFn")

// UnwindListFn evaluates the list expression for one input row. It returns a
// [expr.ListValue] when the expression evaluates to a list, or nil/empty when
// there is nothing to expand.
type UnwindListFn func(row Row) (expr.ListValue, error)

// Unwind is a Volcano pipeline operator that implements the UNWIND clause. For
// each input row it evaluates a list expression and emits one output row per
// list element, appending the element value as a new column.
//
// Unwind is NOT safe for concurrent use.
type Unwind struct {
	child   Operator
	listFn  UnwindListFn
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
	curRow  Row             // current input row being expanded
	curList expr.ListValue  // list being expanded
	listIdx int             // index into curList
	closed  bool            // guards Close against duplicate child.Close calls
}

// NewUnwind creates an Unwind operator.
//
// child provides the context rows. listFn is evaluated once per input row and
// must return the list to expand. The caller is responsible for appending the
// element column to the output row; Unwind handles that internally.
//
// Both child and listFn are required: a nil argument returns the typed sentinel
// [ErrUnwindNilChild] or [ErrUnwindNilListFn] respectively, so callers can
// distinguish the cause via [errors.Is]. NewUnwind never panics.
func NewUnwind(child Operator, listFn UnwindListFn) (*Unwind, error) {
	if child == nil {
		return nil, ErrUnwindNilChild
	}
	if listFn == nil {
		return nil, ErrUnwindNilListFn
	}
	return &Unwind{child: child, listFn: listFn}, nil
}

// Init initialises the operator and its child. It clears all per-iteration
// state (curRow, curList, listIdx) and resets the idempotency guard (closed)
// so that Init is the exact dual of Close, allowing an operator instance to
// be safely re-Init'd after a previous Close.
func (op *Unwind) Init(ctx context.Context) error {
	op.ctx = ctx
	op.curRow = nil
	op.curList = nil
	op.listIdx = 0
	op.closed = false
	return op.child.Init(ctx)
}

// Next advances to the next element. It pulls a new input row from the child
// whenever the current list is exhausted, then emits one row per element.
//
// Returns (true, nil) when an output row was written to out, (false, nil) at
// end-of-stream, (false, err) on error.
func (op *Unwind) Next(out *Row) (bool, error) {
	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// Advance within the current list if elements remain.
		if op.curList != nil && op.listIdx < len(op.curList) {
			elem := op.curList[op.listIdx]
			op.listIdx++
			newRow := make(Row, len(op.curRow)+1)
			copy(newRow, op.curRow)
			newRow[len(op.curRow)] = elem
			*out = newRow
			return true, nil
		}

		// Fetch the next input row.
		var childRow Row
		ok, err := op.child.Next(&childRow)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}

		// Evaluate the list expression for this input row.
		list, err := op.listFn(childRow)
		if err != nil {
			return false, err
		}

		// NULL or empty list → skip (no rows emitted for this input row).
		if len(list) == 0 {
			continue
		}

		op.curRow = childRow
		op.curList = list
		op.listIdx = 0
	}
}

// Close releases resources and closes the child operator.
//
// Close is idempotent within a single pipeline lifecycle: calling it more than
// once between two Init invocations returns nil from the second and later
// calls and does NOT propagate to op.child.Close again. The idempotency guard
// is reset by Init, so an Init→Close→Init→Close sequence still closes the
// child twice — once per cycle, as expected.
func (op *Unwind) Close() error {
	if op.closed {
		return nil
	}
	op.closed = true
	op.curList = nil
	op.curRow = nil
	return op.child.Close()
}
