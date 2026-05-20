package exec

// single_row.go — SingleRow operator (task-268).
//
// SingleRow is a leaf operator that emits exactly one empty row. It is used to
// drive write operators (e.g. CREATE without a leading MATCH) that require
// exactly one input to trigger their mutation logic.
//
// # Concurrency
//
// SingleRow is NOT safe for concurrent use.

import "context"

// SingleRow emits one empty row then signals exhaustion.
//
// SingleRow is NOT safe for concurrent use.
type SingleRow struct {
	ctx  context.Context //nolint:containedctx // stored for per-Next ctx check
	done bool
}

// NewSingleRowOperator returns a SingleRow operator.
func NewSingleRowOperator() *SingleRow {
	return &SingleRow{}
}

// Init resets the operator state.
func (op *SingleRow) Init(ctx context.Context) error {
	op.ctx = ctx
	op.done = false
	return nil
}

// Next emits one empty row on the first call and returns false on all
// subsequent calls.
func (op *SingleRow) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.done {
		return false, nil
	}
	op.done = true
	*out = Row{}
	return true, nil
}

// Close is a no-op; SingleRow holds no resources.
func (op *SingleRow) Close() error {
	return nil
}
