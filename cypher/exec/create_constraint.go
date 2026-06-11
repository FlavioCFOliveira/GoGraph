package exec

// create_constraint.go — CreateConstraintOp DDL operator (task-296).
//
// CreateConstraintOp is a single-row DDL Volcano operator that registers a
// constraint in the ConstraintRegistry.
//
// For UNIQUE constraints a hash index named "__uniq__<label>.<prop>" is also
// created in index.Manager. The engine supplies a pre-built bound subscriber
// (via WithBackingIndex) so the index self-maintains from the change fan-out
// at commit time. When no subscriber is supplied, a plain unbound index is
// created as a safe fallback (e.g. in tests that do not have a live graph).
//
// For NOT NULL constraints only the registry entry is needed; no backing index
// is created.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
)

// uniqueIndexName returns the synthetic backing-index name for a UNIQUE
// constraint on (label, prop).
func uniqueIndexName(label, prop string) string {
	return "__uniq__" + label + "." + prop
}

// UniqueIndexName returns the deterministic synthetic name of the hash index
// that backs a UNIQUE constraint on (label, prop). It is exported so the engine
// can re-create the same backing index when re-registering a constraint
// recovered from disk, keeping the name in lockstep with the one the
// CreateConstraint operator uses.
func UniqueIndexName(label, prop string) string { return uniqueIndexName(label, prop) }

// NewUniqueBackingIndex returns a fresh unbound hash-index subscriber. It is
// kept for callers (tests, recovery) that do not have a live graph and
// therefore cannot build a bound index. The engine's write path always
// supplies a bound subscriber via [CreateConstraintOp.WithBackingIndex] so
// the index self-maintains from the change fan-out.
func NewUniqueBackingIndex() index.Subscriber { return indexhash.New[string]() }

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
	backingIndex   index.Subscriber // optional pre-built bound subscriber for UNIQUE
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

// WithBackingIndex supplies a pre-built index subscriber to use as the UNIQUE
// constraint's backing hash index instead of creating a fresh unbound index.
// The engine passes a bound index (built and backfilled from the live graph)
// so the index self-maintains from the change fan-out at commit time. Returns
// op for chaining.
func (op *CreateConstraintOp) WithBackingIndex(sub index.Subscriber) *CreateConstraintOp {
	op.backingIndex = sub
	return op
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
		sub := op.backingIndex
		if sub == nil {
			sub = indexhash.New[string]()
		}
		if err := op.mgr.CreateIndex(idxName, sub); err != nil {
			if op.ifNotExists && errors.Is(err, index.ErrIndexExists) {
				return false, nil // IF NOT EXISTS — silently succeed; no schema change
			}
			return false, fmt.Errorf("exec: CreateConstraint %q: create backing index: %w", op.name, err)
		}
		op.reg.RegisterUnique(op.label, op.prop, idxName)
		op.reg.SetConstraintName(true, op.label, op.prop, op.name)

	case ConstraintNotNull:
		if op.ifNotExists && op.reg.HasNotNull(op.label, op.prop) {
			return false, nil // IF NOT EXISTS — silently succeed; no schema change
		}
		op.reg.RegisterNotNull(op.label, op.prop)
		op.reg.SetConstraintName(false, op.label, op.prop, op.name)

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
