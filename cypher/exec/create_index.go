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

import (
	"context"
	"errors"
	"fmt"

	"gograph/graph/index"
	indexbtree "gograph/graph/index/btree"
	indexhash "gograph/graph/index/hash"
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
	name        string
	idxType     IndexKindExec
	ifNotExists bool
	mgr         *index.Manager
	ctx         context.Context //nolint:containedctx // stored for per-Next ctx check
	done        bool
}

// NewCreateIndexOp creates a CreateIndexOp.
func NewCreateIndexOp(name string, kind IndexKindExec, ifNotExists bool, mgr *index.Manager) *CreateIndexOp {
	return &CreateIndexOp{
		name:        name,
		idxType:     kind,
		ifNotExists: ifNotExists,
		mgr:         mgr,
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
			return false, nil // IF NOT EXISTS — silently succeed
		}
		return false, fmt.Errorf("exec: CreateIndex %q: %w", op.name, err)
	}
	return false, nil // DDL emits no rows
}

// Close implements Operator.
func (op *CreateIndexOp) Close() error { return nil }
