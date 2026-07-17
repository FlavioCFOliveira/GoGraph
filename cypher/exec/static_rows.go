package exec

// static_rows.go — StaticRows leaf operator (#1922).
//
// StaticRows is a Volcano leaf operator that emits a fixed, pre-computed slice
// of rows in order, one per Next call. It is the source operator for statements
// whose rows are materialised up front rather than pulled from the graph — the
// SHOW CONSTRAINTS / SHOW INDEXES schema-introspection commands, whose row set
// is snapshotted from the constraint/index registries before iteration begins.
//
// # Concurrency
//
// StaticRows is NOT safe for concurrent use. It holds a reference to the caller
// supplied rows and never mutates them, so the caller may reuse the slice after
// the operator is closed.

import "context"

// StaticRows emits a fixed slice of rows, one per Next call, in slice order.
//
// StaticRows is NOT safe for concurrent use.
type StaticRows struct {
	rows []Row
	pos  int
	ctx  context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewStaticRows creates a StaticRows operator over rows. The slice is retained
// by reference and never modified; a nil or empty slice yields an operator that
// emits no rows.
func NewStaticRows(rows []Row) *StaticRows {
	return &StaticRows{rows: rows}
}

// Init stores ctx and rewinds the cursor to the first row.
func (op *StaticRows) Init(ctx context.Context) error {
	op.ctx = ctx
	op.pos = 0
	return nil
}

// Next emits the next buffered row, or (false, nil) once every row has been
// emitted. It honours context cancellation on every call.
func (op *StaticRows) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.pos >= len(op.rows) {
		return false, nil
	}
	*out = op.rows[op.pos]
	op.pos++
	return true, nil
}

// Close releases the row reference. It is idempotent and always returns nil.
func (op *StaticRows) Close() error {
	op.rows = nil
	return nil
}
