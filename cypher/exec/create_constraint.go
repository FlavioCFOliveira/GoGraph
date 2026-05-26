package exec

// create_constraint.go — CreateConstraintOp DDL operator (task-296).
//
// CreateConstraintOp is a single-row DDL Volcano operator that registers a
// constraint in the ConstraintRegistry.
//
// For UNIQUE constraints a hash index named "__uniq__<label>.<prop>" is also
// created in index.Manager. The synthetic name is deterministic so that
// CheckSetProperty can look it up without extra metadata.
//
// For NOT NULL constraints only the registry entry is needed; no backing index
// is created.

import (
	"context"
	"errors"
	"fmt"

	"gograph/graph/index"
	indexhash "gograph/graph/index/hash"
)

// uniqueIndexName returns the synthetic backing-index name for a UNIQUE
// constraint on (label, prop).
func uniqueIndexName(label, prop string) string {
	return "__uniq__" + label + "." + prop
}

// CreateConstraintOp is a Volcano DDL operator that registers a constraint.
//
// CreateConstraintOp is NOT safe for concurrent use.
type CreateConstraintOp struct {
	name           string
	label          string
	prop           string
	kind           ConstraintKind
	ifNotExists    bool
	mgr            *index.Manager
	reg            *ConstraintRegistry
	onSchemaChange func()
	ctx            context.Context //nolint:containedctx // stored for per-Next ctx check
	done           bool
}

// NewCreateConstraintOp creates a CreateConstraintOp. onSchemaChange, when
// non-nil, is invoked exactly once after the operator successfully registers
// a new constraint — i.e. NOT when the IF NOT EXISTS branch silently absorbs
// an already-registered constraint. The Engine wires e.ClearPlanCache as
// onSchemaChange so cached plans are invalidated after a real schema
// mutation.
func NewCreateConstraintOp(
	name, label, prop string,
	kind ConstraintKind,
	ifNotExists bool,
	mgr *index.Manager,
	reg *ConstraintRegistry,
	onSchemaChange func(),
) *CreateConstraintOp {
	return &CreateConstraintOp{
		name:           name,
		label:          label,
		prop:           prop,
		kind:           kind,
		ifNotExists:    ifNotExists,
		mgr:            mgr,
		reg:            reg,
		onSchemaChange: onSchemaChange,
	}
}

// Init implements Operator.
func (op *CreateConstraintOp) Init(ctx context.Context) error {
	op.ctx = ctx
	op.done = false
	return nil
}

// Next implements Operator. Performs the CREATE CONSTRAINT side effect on the
// first call, then signals end-of-stream.
func (op *CreateConstraintOp) Next(_ *Row) (bool, error) {
	if op.done {
		return false, nil
	}
	op.done = true

	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	switch op.kind {
	case ConstraintUnique:
		idxName := uniqueIndexName(op.label, op.prop)
		sub := indexhash.New[string]()
		if err := op.mgr.CreateIndex(idxName, sub); err != nil {
			if op.ifNotExists && errors.Is(err, index.ErrIndexExists) {
				return false, nil // IF NOT EXISTS — silently succeed; no schema change
			}
			return false, fmt.Errorf("exec: CreateConstraint %q: create backing index: %w", op.name, err)
		}
		op.reg.RegisterUnique(op.label, op.prop, idxName)

	case ConstraintNotNull:
		if op.ifNotExists && op.reg.HasNotNull(op.label, op.prop) {
			return false, nil // IF NOT EXISTS — silently succeed; no schema change
		}
		op.reg.RegisterNotNull(op.label, op.prop)

	default:
		return false, fmt.Errorf("exec: CreateConstraint: unknown constraint kind %d", op.kind)
	}

	// Real schema mutation: a new UNIQUE backing index was created, or a new
	// NOT NULL entry was registered. Notify so dependent caches (e.g. the
	// plan cache) can invalidate stale entries.
	if op.onSchemaChange != nil {
		op.onSchemaChange()
	}
	return false, nil // DDL emits no rows
}

// Close implements Operator.
func (op *CreateConstraintOp) Close() error { return nil }
