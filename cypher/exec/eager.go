package exec

// eager.go — Eager pipeline barrier for write-before-read isolation.
//
// Eager fully consumes its child during Init and re-emits the buffered rows
// on subsequent Next calls. The eager drain in Init guarantees write-side
// operators run to completion even when a downstream short-circuit operator
// (LIMIT 0 in particular) would otherwise return false before pulling its
// child.
//
// # Concurrency
//
// Eager is NOT safe for concurrent use.

import (
	"context"
	"fmt"
)

// Eager is a pipeline-breaking barrier. Init drains every row from the child
// into an internal buffer; Next then re-emits the buffered rows in insertion
// order.
type Eager struct {
	child   Operator
	rows    []Row
	emitIdx int
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

// NewEager wraps child in an Eager barrier.
func NewEager(child Operator) *Eager { return &Eager{child: child} }

// Init initialises the operator AND drains every child row into the buffer
// so the downstream pipeline can apply LIMIT / SKIP / DISTINCT without
// starving any write-side child of its driving rows.
func (op *Eager) Init(ctx context.Context) error {
	op.ctx = ctx
	op.rows = nil
	op.emitIdx = 0
	if err := op.child.Init(ctx); err != nil {
		return err
	}
	for {
		var row Row
		ok, err := op.child.Next(&row)
		if err != nil {
			return fmt.Errorf("exec: Eager: drain child: %w", err)
		}
		if !ok {
			break
		}
		cp := make(Row, len(row))
		copy(cp, row)
		op.rows = append(op.rows, cp)
	}
	return nil
}

// Next emits the next buffered row.
func (op *Eager) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.emitIdx >= len(op.rows) {
		return false, nil
	}
	*out = op.rows[op.emitIdx]
	op.emitIdx++
	return true, nil
}

// Close releases the buffer and closes the child.
func (op *Eager) Close() error {
	op.rows = nil
	return op.child.Close()
}
