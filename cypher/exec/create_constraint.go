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
	name        string
	label       string
	prop        string
	kind        ConstraintKind
	ifNotExists bool
	mgr         *index.Manager
	reg         *ConstraintRegistry
	ctx         context.Context //nolint:containedctx // stored for per-Next ctx check
	done        bool
}

// NewCreateConstraintOp creates a CreateConstraintOp.
func NewCreateConstraintOp(
	name, label, prop string,
	kind ConstraintKind,
	ifNotExists bool,
	mgr *index.Manager,
	reg *ConstraintRegistry,
) *CreateConstraintOp {
	return &CreateConstraintOp{
		name:        name,
		label:       label,
		prop:        prop,
		kind:        kind,
		ifNotExists: ifNotExists,
		mgr:         mgr,
		reg:         reg,
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
				return false, nil
			}
			return false, fmt.Errorf("exec: CreateConstraint %q: create backing index: %w", op.name, err)
		}
		op.reg.RegisterUnique(op.label, op.prop, idxName)

	case ConstraintNotNull:
		if op.ifNotExists && op.reg.HasNotNull(op.label, op.prop) {
			return false, nil
		}
		op.reg.RegisterNotNull(op.label, op.prop)

	default:
		return false, fmt.Errorf("exec: CreateConstraint: unknown constraint kind %d", op.kind)
	}

	return false, nil // DDL emits no rows
}

// Close implements Operator.
func (op *CreateConstraintOp) Close() error { return nil }
