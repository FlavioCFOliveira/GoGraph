package exec

// drop_index.go — DropIndex DDL operator (task-295).
//
// DropIndex is a single-row DDL Volcano operator that removes a secondary
// index via index.Manager.DropIndex. It emits zero output rows.

import (
	"context"
	"errors"
	"fmt"

	"gograph/graph/index"
)

// DropIndexOp is a Volcano DDL operator that deregisters a secondary index.
//
// DropIndexOp is NOT safe for concurrent use.
type DropIndexOp struct {
	name           string
	ifExists       bool
	mgr            *index.Manager
	onSchemaChange func()
	ctx            context.Context //nolint:containedctx // stored for per-Next ctx check
	done           bool
}

// NewDropIndexOp creates a DropIndexOp. onSchemaChange, when non-nil, is
// invoked exactly once after the operator successfully drops the index — i.e.
// NOT when the IF EXISTS branch silently absorbs a missing-index error. The
// Engine wires e.ClearPlanCache as onSchemaChange so cached plans are
// invalidated after a real schema mutation.
func NewDropIndexOp(
	name string,
	ifExists bool,
	mgr *index.Manager,
	onSchemaChange func(),
) *DropIndexOp {
	return &DropIndexOp{
		name:           name,
		ifExists:       ifExists,
		mgr:            mgr,
		onSchemaChange: onSchemaChange,
	}
}

// Init implements Operator.
func (op *DropIndexOp) Init(ctx context.Context) error {
	op.ctx = ctx
	op.done = false
	return nil
}

// Next implements Operator. It performs the DROP INDEX side effect on the first
// call, then signals end-of-stream.
func (op *DropIndexOp) Next(_ *Row) (bool, error) {
	if op.done {
		return false, nil
	}
	op.done = true

	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	if err := op.mgr.DropIndex(op.name); err != nil {
		if op.ifExists && errors.Is(err, index.ErrIndexNotFound) {
			return false, nil // IF EXISTS — silently succeed; no schema change
		}
		return false, fmt.Errorf("exec: DropIndex %q: %w", op.name, err)
	}
	// Real schema mutation: notify so dependent caches (e.g. the plan cache)
	// can invalidate stale entries that referenced the now-removed index.
	if op.onSchemaChange != nil {
		op.onSchemaChange()
	}
	return false, nil // DDL emits no rows
}

// Close implements Operator.
func (op *DropIndexOp) Close() error { return nil }
