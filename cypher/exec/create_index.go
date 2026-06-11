package exec

// create_index.go — CreateIndex DDL operator (task-294).
//
// CreateIndex is a single-row DDL Volcano operator that creates a secondary
// index via index.Manager.CreateIndex. It emits zero output rows on success.
//
// Index construction strategy:
//   - hash  → graph/index/hash.Index[string] (property values treated as strings)
//   - btree → graph/index/btree.Index[string] (property values treated as strings)
//
// The created subscriber is registered with the manager and will be included in
// the next snapshot via store/snapshot.WriteIndexes.
//
// Note: the Cypher engine routes hash CREATE INDEX through its own path
// (Engine.runCreateHashIndex, task #1340), which builds a BOUND hash index —
// backfilled from pre-existing data and self-maintaining via the change
// fan-out — under the engine's writer serialisation. This operator remains
// the btree route and a building block for embedders driving the exec layer
// directly; the indexes it creates are unbound (empty until populated by
// explicit Insert calls).

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	indexbtree "github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
)

// IndexKindExec distinguishes hash vs. btree in the exec layer.
type IndexKindExec uint8

const (
	// ExecIndexHash creates a hash.Index[string].
	ExecIndexHash IndexKindExec = iota
	// ExecIndexBTree creates a btree.Index[string].
	ExecIndexBTree
)

// CreateIndexOp is a Volcano DDL operator that registers a new secondary index.
//
// CreateIndexOp is NOT safe for concurrent use.
type CreateIndexOp struct {
	name           string
	idxType        IndexKindExec
	ifNotExists    bool
	mgr            *index.Manager
	onSchemaChange func()
	ctx            context.Context //nolint:containedctx // stored for per-Next ctx check
	done           bool
}

// NewCreateIndexOp creates a CreateIndexOp. onSchemaChange, when non-nil, is
// invoked exactly once after the operator successfully creates a new index in
// mgr — i.e. NOT when the IF NOT EXISTS branch silently absorbs a duplicate.
// The Engine wires e.ClearPlanCache as onSchemaChange so cached plans are
// invalidated after a real schema mutation.
func NewCreateIndexOp(
	name string,
	kind IndexKindExec,
	ifNotExists bool,
	mgr *index.Manager,
	onSchemaChange func(),
) *CreateIndexOp {
	return &CreateIndexOp{
		name:           name,
		idxType:        kind,
		ifNotExists:    ifNotExists,
		mgr:            mgr,
		onSchemaChange: onSchemaChange,
	}
}

// Init implements Operator.
func (op *CreateIndexOp) Init(ctx context.Context) error {
	op.ctx = ctx
	op.done = false
	return nil
}

// Next implements Operator. It performs the CREATE INDEX side effect on the
// first call, then signals end-of-stream. Returns (false, nil) immediately on
// subsequent calls.
func (op *CreateIndexOp) Next(_ *Row) (bool, error) {
	if op.done {
		return false, nil
	}
	op.done = true

	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	var sub index.Subscriber
	switch op.idxType {
	case ExecIndexHash:
		sub = indexhash.New[string]()
	case ExecIndexBTree:
		sub = indexbtree.New[string]()
	default:
		return false, fmt.Errorf("exec: CreateIndex: unknown index type %d", op.idxType)
	}

	if err := op.mgr.CreateIndex(op.name, sub); err != nil {
		if op.ifNotExists && errors.Is(err, index.ErrIndexExists) {
			return false, nil // IF NOT EXISTS — silently succeed; no schema change
		}
		return false, fmt.Errorf("exec: CreateIndex %q: %w", op.name, err)
	}
	// Real schema mutation: notify so dependent caches (e.g. the plan cache)
	// can invalidate stale entries built before the new index existed.
	if op.onSchemaChange != nil {
		op.onSchemaChange()
	}
	return false, nil // DDL emits no rows
}

// Close implements Operator.
func (op *CreateIndexOp) Close() error { return nil }
