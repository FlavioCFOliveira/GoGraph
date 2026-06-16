package exec

// drop_constraint.go — DropConstraintOp DDL operator (task-297).
//
// DropConstraintOp is a single-row DDL Volcano operator that removes a
// constraint from the ConstraintRegistry.
//
// For UNIQUE constraints the backing hash index is dropped from the
// index.Manager together with the registry entry as one unit — the index
// cannot outlive the constraint, which is what makes re-creating the same
// UNIQUE constraint work after a drop.
//
// For NOT NULL constraints only the registry entry is removed.
//
// When the named constraint is not registered the operator is a clean no-op
// under IF EXISTS, and returns a typed error wrapping ErrConstraintNotFound
// otherwise — it never reports success without removing anything.
//
// The caller resolves the constraint NAME to its (kind, label, property)
// identity (via ConstraintRegistry.ResolveByName) before constructing the
// operator: a by-name DROP otherwise carries an empty label/property in the IR.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// DropConstraintOp is a Volcano DDL operator that deregisters a constraint.
//
// DropConstraintOp is NOT safe for concurrent use.
type DropConstraintOp struct {
	name           string
	label          string
	prop           string
	kind           ConstraintKind
	ifExists       bool
	mgr            *index.Manager
	reg            *ConstraintRegistry
	onSchemaChange func()
	ctx            context.Context //nolint:containedctx // stored for per-Next ctx check
	done           bool
}

// NewDropConstraintOp creates a DropConstraintOp. onSchemaChange, when
// non-nil, is invoked exactly once after the operator successfully removes a
// constraint — i.e. NOT when the IF EXISTS branch silently absorbs an
// absent-constraint condition. The Engine wires e.ClearPlanCache as
// onSchemaChange so cached plans are invalidated after a real schema
// mutation.
func NewDropConstraintOp(
	name, label, prop string,
	kind ConstraintKind,
	ifExists bool,
	mgr *index.Manager,
	reg *ConstraintRegistry,
	onSchemaChange func(),
) *DropConstraintOp {
	return &DropConstraintOp{
		name:           name,
		label:          label,
		prop:           prop,
		kind:           kind,
		ifExists:       ifExists,
		mgr:            mgr,
		reg:            reg,
		onSchemaChange: onSchemaChange,
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
		// Resolve the backing index name from the registry. When the constraint
		// is not registered there is no UNIQUE constraint to drop: with IF EXISTS
		// this is a clean no-op, otherwise a typed not-found error. Never drop a
		// stray "__uniq__" index that no live constraint backs.
		idxName, ok := op.reg.UniqueIndexName(op.label, op.prop)
		if !ok {
			if op.ifExists {
				return false, nil // IF EXISTS — clean no-op; no schema change
			}
			return false, fmt.Errorf("exec: DropConstraint %q: %w", op.name, ErrConstraintNotFound)
		}
		// Drop the backing index and the registry entry as one unit: the index
		// cannot outlive the constraint on this path. ErrIndexNotFound is
		// tolerated (the constraint may predate a bound backing index), so the
		// registry entry is still removed and re-creation works.
		if err := op.mgr.DropIndex(idxName); err != nil && !errors.Is(err, index.ErrIndexNotFound) {
			return false, fmt.Errorf("exec: DropConstraint %q: drop backing index: %w", op.name, err)
		}
		op.reg.UnregisterUnique(op.label, op.prop)

	case ConstraintNotNull:
		if !op.reg.HasNotNull(op.label, op.prop) {
			if op.ifExists {
				return false, nil // IF EXISTS — clean no-op; no schema change
			}
			return false, fmt.Errorf("exec: DropConstraint %q: %w", op.name, ErrConstraintNotFound)
		}
		op.reg.UnregisterNotNull(op.label, op.prop)

	default:
		return false, fmt.Errorf("exec: DropConstraint: unknown constraint kind %d", op.kind)
	}

	// Real schema mutation: a UNIQUE backing index was dropped, or a NOT NULL
	// entry was unregistered. Notify so dependent caches (e.g. the plan cache)
	// can invalidate stale entries that depended on the removed constraint.
	if op.onSchemaChange != nil {
		op.onSchemaChange()
	}
	return false, nil // DDL emits no rows
}

// Close implements Operator.
func (op *DropConstraintOp) Close() error { return nil }
