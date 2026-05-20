package exec

// drop_constraint.go — DropConstraintOp DDL operator (task-297).
//
// DropConstraintOp is a single-row DDL Volcano operator that removes a
// constraint from the ConstraintRegistry.
//
// For UNIQUE constraints the backing hash index is also dropped from the
// index.Manager. If the index is not found and IF EXISTS is set, the
// operation is silently skipped.
//
// For NOT NULL constraints only the registry entry is removed.

import (
	"context"
	"errors"
	"fmt"

	"gograph/graph/index"
)

// DropConstraintOp is a Volcano DDL operator that deregisters a constraint.
//
// DropConstraintOp is NOT safe for concurrent use.
type DropConstraintOp struct {
	name     string
	label    string
	prop     string
	kind     ConstraintKind
	ifExists bool
	mgr      *index.Manager
	reg      *ConstraintRegistry
	ctx      context.Context //nolint:containedctx // stored for per-Next ctx check
	done     bool
}

// NewDropConstraintOp creates a DropConstraintOp.
func NewDropConstraintOp(
	name, label, prop string,
	kind ConstraintKind,
	ifExists bool,
	mgr *index.Manager,
	reg *ConstraintRegistry,
) *DropConstraintOp {
	return &DropConstraintOp{
		name:     name,
		label:    label,
		prop:     prop,
		kind:     kind,
		ifExists: ifExists,
		mgr:      mgr,
		reg:      reg,
	}
}

// Init implements Operator.
func (op *DropConstraintOp) Init(ctx context.Context) error {
	op.ctx = ctx
	op.done = false
	return nil
}

// Next implements Operator. Performs the DROP CONSTRAINT side effect on the
// first call, then signals end-of-stream.
func (op *DropConstraintOp) Next(_ *Row) (bool, error) {
	if op.done {
		return false, nil
	}
	op.done = true

	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	switch op.kind {
	case ConstraintUnique:
		// Resolve the backing index name from the registry when label+prop are
		// known; fall back to the deterministic synthetic name otherwise.
		idxName, ok := op.reg.UniqueIndexName(op.label, op.prop)
		if !ok {
			idxName = uniqueIndexName(op.label, op.prop)
		}
		if err := op.mgr.DropIndex(idxName); err != nil {
			if op.ifExists && errors.Is(err, index.ErrIndexNotFound) {
				return false, nil
			}
			return false, fmt.Errorf("exec: DropConstraint %q: drop backing index: %w", op.name, err)
		}
		op.reg.UnregisterUnique(op.label, op.prop)

	case ConstraintNotNull:
		if !op.reg.HasNotNull(op.label, op.prop) {
			if op.ifExists {
				return false, nil
			}
			return false, fmt.Errorf("exec: DropConstraint %q: constraint not found", op.name)
		}
		op.reg.UnregisterNotNull(op.label, op.prop)

	default:
		return false, fmt.Errorf("exec: DropConstraint: unknown constraint kind %d", op.kind)
	}

	return false, nil // DDL emits no rows
}

// Close implements Operator.
func (op *DropConstraintOp) Close() error { return nil }
